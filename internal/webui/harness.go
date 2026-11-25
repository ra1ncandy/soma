package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ra1ncandy/soma/internal/bridge"
	"github.com/ra1ncandy/soma/internal/daemon"
	"github.com/ra1ncandy/soma/internal/dag"
	"github.com/ra1ncandy/soma/internal/desktop"
	"github.com/ra1ncandy/soma/internal/mcp"
	"github.com/ra1ncandy/soma/internal/store"
	"gopkg.in/yaml.v3"
)

// AccountEndpointCfg stores per-account endpoint, credentials, and agent role metadata in .soma/harness.json.
type AccountEndpointCfg struct {
	APIBase string `json:"api_base,omitempty"`
	APIKey  string `json:"api_key,omitempty"`
	Role    string `json:"role,omitempty"` // "default" | "worker" | "frontier" | "gui" | "fallback"
}

// HarnessConfig persists dashboard-managed harness, connector, and bridge settings in .soma/harness.json.
type HarnessConfig struct {
	Provider         ProviderConfig                `json:"provider"`
	DefaultAccountID string                        `json:"default_account_id,omitempty"`
	AccountConfigs   map[string]AccountEndpointCfg `json:"account_configs,omitempty"`
	MCPServers       []MCPServerCfg                `json:"mcp_servers"`
	Bridges          BridgesConfig                 `json:"bridges"`
	Tunnel           TunnelConfig                  `json:"tunnel"`
}

// ProviderConfig configures the active LLM provider, embedder, and execution defaults.
type ProviderConfig struct {
	ProviderType      string `json:"provider_type"` // "auto" | "openai_compat" | "mock"
	APIBase           string `json:"api_base"`
	APIKey            string `json:"api_key,omitempty"`
	ModelID           string `json:"model_id"`
	EmbedModel        string `json:"embed_model"`
	EmbedURL          string `json:"embed_url"`
	MaxTurns            int    `json:"max_turns"`
	WorkspaceStrategy   string `json:"workspace_strategy"`    // "worktree" | "copy" | "direct"
	SandboxMode         string `json:"sandbox_mode"`          // "auto" | "bwrap" | "seatbelt"
	AllowSandboxNetwork bool   `json:"allow_sandbox_network"` // allow outbound internet inside OS sandbox
	AutonomyMode        string `json:"autonomy_mode"`         // "standard" | "autonomous"
}

// MCPServerCfg stores an MCP connector endpoint in .soma/harness.json.
type MCPServerCfg struct {
	Name            string            `json:"name"`
	PresetID        string            `json:"preset_id,omitempty"`
	Transport       string            `json:"transport"` // "stdio" | "streamable_http" | "sse" | "http"
	Command         string            `json:"command,omitempty"`
	Args            []string          `json:"args,omitempty"`
	URL             string            `json:"url,omitempty"`
	AuthToken       string            `json:"auth_token,omitempty"`
	Env             map[string]string `json:"env,omitempty"`
	Headers         map[string]string `json:"headers,omitempty"`
	IncludeTools    []string          `json:"include_tools,omitempty"`
	ExcludeTools    []string          `json:"exclude_tools,omitempty"`
	TimeoutSeconds  int               `json:"timeout_seconds,omitempty"`
	OAuth           *mcp.OAuthConfig  `json:"oauth,omitempty"`
	IsolatedDesktop bool              `json:"isolated_desktop"`
	Enabled         bool              `json:"enabled"`
}

// BridgesConfig stores all messaging channel bridge settings.
type BridgesConfig struct {
	Discord    DiscordBridgeCfg    `json:"discord"`
	Telegram   TelegramBridgeCfg   `json:"telegram"`
	Slack      SlackBridgeCfg      `json:"slack"`
	Matrix     MatrixBridgeCfg     `json:"matrix"`
	Mattermost MattermostBridgeCfg `json:"mattermost"`
	Teams      WebhookChannelCfg   `json:"teams"`
	GoogleChat WebhookChannelCfg   `json:"google_chat"`
	Email      EmailBridgeCfg      `json:"email"`
	WhatsApp   SidecarBridgeCfg    `json:"whatsapp"`
	Signal     SidecarBridgeCfg    `json:"signal"`
	SMS        SidecarBridgeCfg    `json:"sms"`
}

// SlackBridgeCfg stores Slack Events API + Bot Token settings.
type SlackBridgeCfg struct {
	Enabled           bool     `json:"enabled"`
	BotToken          string   `json:"bot_token,omitempty"`
	SigningSecret     string   `json:"signing_secret,omitempty"`
	AllowedUserIDs    []string `json:"allowed_user_ids,omitempty"`
	AllowedChannelIDs []string `json:"allowed_channel_ids,omitempty"`
}

// MatrixBridgeCfg stores Matrix Client-Server v3 settings.
type MatrixBridgeCfg struct {
	Enabled        bool     `json:"enabled"`
	HomeserverURL  string   `json:"homeserver_url,omitempty"`
	AccessToken    string   `json:"access_token,omitempty"`
	UserID         string   `json:"user_id,omitempty"`
	AllowedSenders []string `json:"allowed_senders,omitempty"`
	AllowedRooms   []string `json:"allowed_rooms,omitempty"`
}

// MattermostBridgeCfg stores Mattermost Bot/Webhook settings.
type MattermostBridgeCfg struct {
	Enabled        bool     `json:"enabled"`
	ServerURL      string   `json:"server_url,omitempty"`
	BotToken       string   `json:"bot_token,omitempty"`
	WebhookURL     string   `json:"webhook_url,omitempty"`
	OutgoingToken  string   `json:"outgoing_token,omitempty"`
	AllowedUserIDs []string `json:"allowed_user_ids,omitempty"`
}

// WebhookChannelCfg stores Microsoft Teams or Google Chat webhook settings.
type WebhookChannelCfg struct {
	Enabled        bool     `json:"enabled"`
	WebhookURL     string   `json:"webhook_url,omitempty"`
	AllowedUserIDs []string `json:"allowed_user_ids,omitempty"`
}

// EmailBridgeCfg stores SMTP email notification settings.
type EmailBridgeCfg struct {
	Enabled  bool   `json:"enabled"`
	SMTPHost string `json:"smtp_host,omitempty"`
	SMTPPort int    `json:"smtp_port,omitempty"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	From     string `json:"from,omitempty"`
}

// SidecarBridgeCfg stores generic webhook sidecar settings (WhatsApp, Signal, SMS).
type SidecarBridgeCfg struct {
	Enabled        bool     `json:"enabled"`
	OutboundURL    string   `json:"outbound_url,omitempty"`
	BearerToken    string   `json:"bearer_token,omitempty"`
	HMACSecret     string   `json:"hmac_secret,omitempty"`
	AllowedSenders []string `json:"allowed_senders,omitempty"`
}

// DiscordBridgeCfg stores Discord bridge settings.
type DiscordBridgeCfg struct {
	Enabled           bool     `json:"enabled"`
	BotToken          string   `json:"bot_token,omitempty"`
	PublicKeyHex      string   `json:"public_key_hex,omitempty"`
	ChannelID         string   `json:"channel_id,omitempty"`
	AllowedUserIDs    []string `json:"allowed_user_ids,omitempty"`
	AllowedChannelIDs []string `json:"allowed_channel_ids,omitempty"`
	ListenAddr        string   `json:"listen_addr,omitempty"`
}

// TelegramBridgeCfg stores Telegram bridge settings.
type TelegramBridgeCfg struct {
	Enabled        bool    `json:"enabled"`
	BotToken       string  `json:"bot_token,omitempty"`
	AllowedUserIDs []int64 `json:"allowed_user_ids,omitempty"`
	AllowedChatIDs []int64 `json:"allowed_chat_ids,omitempty"`
	AlertChatID    int64   `json:"alert_chat_id,omitempty"`
}

// HarnessDTO is the secret-safe JSON snapshot returned by GET /api/harness.
type HarnessDTO struct {
	Provider        ProviderDTO                     `json:"provider"`
	ProviderPresets []dag.ProviderPreset            `json:"provider_presets"`
	MCPServers      []MCPServerDTO                  `json:"mcp_servers"`
	Presets         []MCPPresetDTO                  `json:"presets"`
	Bridges         BridgesDTO                      `json:"bridges"`
	BridgeRunning   map[string]bool                 `json:"bridge_running,omitempty"`
	BridgeErrors    map[string]string               `json:"bridge_errors,omitempty"`
	Pairing         PairingDTO                      `json:"pairing"`
	Triggers        TriggersConfigDTO               `json:"triggers"`
	DaemonRunning   bool                            `json:"daemon_running"`
	TriggerLastRun  map[string]TriggerRunSummaryDTO `json:"trigger_last_run,omitempty"`
	Accounts        []AccountDTO                    `json:"accounts"`
	Desktop         DesktopStatusDTO                `json:"desktop"`
	Tunnel          TunnelDTO                       `json:"tunnel"`
}

// PairingDTO surfaces pending DM pairing requests and approved paired senders.
type PairingDTO struct {
	Approved []bridge.PairedSender   `json:"approved"`
	Pending  []bridge.PendingPairing `json:"pending"`
}

// ProviderDTO is the secret-safe provider view.
type ProviderDTO struct {
	Mode              string `json:"mode"`
	ProviderType      string `json:"provider_type"`
	APIBase           string `json:"api_base"`
	HasAPIKey         bool   `json:"has_api_key"`
	APIKeyMasked      string `json:"api_key_masked"`
	ModelID           string `json:"model_id"`
	EmbedModel        string `json:"embed_model"`
	EmbedURL          string `json:"embed_url"`
	MaxTurns            int    `json:"max_turns"`
	WorkspaceStrategy   string `json:"workspace_strategy"`
	SandboxMode         string `json:"sandbox_mode"`
	AllowSandboxNetwork bool   `json:"allow_sandbox_network"`
	AutonomyMode        string `json:"autonomy_mode"`
}

// MCPServerDTO is the secret-safe MCP connector view.
type MCPServerDTO struct {
	Name            string   `json:"name"`
	PresetID        string   `json:"preset_id,omitempty"`
	Transport       string   `json:"transport"`
	Command         string   `json:"command,omitempty"`
	Args            []string `json:"args,omitempty"`
	URL             string   `json:"url,omitempty"`
	HasAuthToken    bool     `json:"has_auth_token"`
	EnvKeys         []string `json:"env_keys,omitempty"`
	HeaderKeys      []string `json:"header_keys,omitempty"`
	IncludeTools    []string `json:"include_tools,omitempty"`
	ExcludeTools    []string `json:"exclude_tools,omitempty"`
	TimeoutSeconds  int      `json:"timeout_seconds,omitempty"`
	OAuthEnabled    bool     `json:"oauth_enabled"`
	OAuthConnected  bool     `json:"oauth_connected"`
	IsolatedDesktop bool     `json:"isolated_desktop"`
	Enabled         bool     `json:"enabled"`
}

// MCPPresetDTO describes a one-click MCP connector preset.
type MCPPresetDTO struct {
	ID              string            `json:"id"`
	Name            string            `json:"name"`
	Category        string            `json:"category"`
	Description     string            `json:"description"`
	Transport       string            `json:"transport"`
	Command         string            `json:"command,omitempty"`
	Args            []string          `json:"args,omitempty"`
	URL             string            `json:"url,omitempty"`
	Env             map[string]string `json:"env,omitempty"`
	RequiredEnv     []string          `json:"required_env,omitempty"`
	OAuthEnabled    bool              `json:"oauth_enabled"`
	IsolatedDesktop bool              `json:"isolated_desktop"`
	Installed       bool              `json:"installed"`
}

// BridgesDTO is the secret-safe bridge view.
type BridgesDTO struct {
	Discord    DiscordBridgeDTO    `json:"discord"`
	Telegram   TelegramBridgeDTO   `json:"telegram"`
	Slack      SlackBridgeDTO      `json:"slack"`
	Matrix     MatrixBridgeDTO     `json:"matrix"`
	Mattermost MattermostBridgeDTO `json:"mattermost"`
	Teams      WebhookChannelDTO   `json:"teams"`
	GoogleChat WebhookChannelDTO   `json:"google_chat"`
	Email      EmailBridgeDTO      `json:"email"`
	WhatsApp   SidecarBridgeDTO    `json:"whatsapp"`
	Signal     SidecarBridgeDTO    `json:"signal"`
	SMS        SidecarBridgeDTO    `json:"sms"`
}

// SlackBridgeDTO is the secret-safe Slack bridge view.
type SlackBridgeDTO struct {
	Enabled           bool     `json:"enabled"`
	HasBotToken       bool     `json:"has_bot_token"`
	BotTokenMasked    string   `json:"bot_token_masked"`
	HasSigningSecret  bool     `json:"has_signing_secret"`
	AllowedUserIDs    []string `json:"allowed_user_ids"`
	AllowedChannelIDs []string `json:"allowed_channel_ids"`
}

// MatrixBridgeDTO is the secret-safe Matrix bridge view.
type MatrixBridgeDTO struct {
	Enabled        bool     `json:"enabled"`
	HomeserverURL  string   `json:"homeserver_url"`
	HasAccessToken bool     `json:"has_access_token"`
	UserID         string   `json:"user_id"`
	AllowedSenders []string `json:"allowed_senders"`
	AllowedRooms   []string `json:"allowed_rooms"`
}

// MattermostBridgeDTO is the secret-safe Mattermost bridge view.
type MattermostBridgeDTO struct {
	Enabled        bool     `json:"enabled"`
	ServerURL      string   `json:"server_url"`
	HasBotToken    bool     `json:"has_bot_token"`
	WebhookURL     string   `json:"webhook_url"`
	AllowedUserIDs []string `json:"allowed_user_ids"`
}

// WebhookChannelDTO is the secret-safe Teams/Google Chat view.
type WebhookChannelDTO struct {
	Enabled        bool     `json:"enabled"`
	WebhookURL     string   `json:"webhook_url"`
	AllowedUserIDs []string `json:"allowed_user_ids"`
}

// EmailBridgeDTO is the secret-safe Email/SMTP bridge view.
type EmailBridgeDTO struct {
	Enabled     bool   `json:"enabled"`
	SMTPHost    string `json:"smtp_host"`
	SMTPPort    int    `json:"smtp_port"`
	Username    string `json:"username"`
	HasPassword bool   `json:"has_password"`
	From        string `json:"from"`
}

// SidecarBridgeDTO is the secret-safe WhatsApp/Signal/SMS sidecar view.
type SidecarBridgeDTO struct {
	Enabled        bool     `json:"enabled"`
	OutboundURL    string   `json:"outbound_url"`
	HasBearerToken bool     `json:"has_bearer_token"`
	HasHMACSecret  bool     `json:"has_hmac_secret"`
	AllowedSenders []string `json:"allowed_senders"`
}

// DiscordBridgeDTO is the secret-safe Discord bridge view.
type DiscordBridgeDTO struct {
	Enabled           bool     `json:"enabled"`
	HasBotToken       bool     `json:"has_bot_token"`
	BotTokenMasked    string   `json:"bot_token_masked"`
	PublicKeyHex      string   `json:"public_key_hex"`
	ChannelID         string   `json:"channel_id"`
	AllowedUserIDs    []string `json:"allowed_user_ids"`
	AllowedChannelIDs []string `json:"allowed_channel_ids"`
	ListenAddr        string   `json:"listen_addr"`
}

// TelegramBridgeDTO is the secret-safe Telegram bridge view.
type TelegramBridgeDTO struct {
	Enabled        bool    `json:"enabled"`
	HasBotToken    bool    `json:"has_bot_token"`
	BotTokenMasked string  `json:"bot_token_masked"`
	AllowedUserIDs []int64 `json:"allowed_user_ids"`
	AllowedChatIDs []int64 `json:"allowed_chat_ids"`
	AlertChatID    int64   `json:"alert_chat_id"`
}

// TriggersConfigDTO represents .soma/triggers.yaml for the dashboard.
type TriggersConfigDTO struct {
	QuietHours            string              `json:"quiet_hours"`
	MaxPendingInbox       int                 `json:"max_pending_inbox"`
	DailyMeteredBudgetUSD float64             `json:"daily_metered_budget_usd"`
	WebhookAddr           string              `json:"webhook_addr"`
	GCInterval            string              `json:"gc_interval"`
	Triggers              []daemon.TriggerSpec `json:"triggers"`
}

// DesktopStatusDTO reports virtual desktop container status and viewer URLs.
type DesktopStatusDTO struct {
	Running       bool   `json:"running"`
	ContainerName string `json:"container_name"`
	Image         string `json:"image"`
	NoVNCPort     int    `json:"novnc_port"`
	NoVNCURL      string `json:"novnc_url"`
	ProxyURL      string `json:"proxy_url"`
	Resolution    string `json:"resolution"`
}

func maskSecret(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) <= 4 {
		return "••••"
	}
	return "••••" + s[len(s)-4:]
}

func (s *Server) harnessPath() string {
	return filepath.Join(s.store.SomaDir(), "harness.json")
}

func (s *Server) triggersPath() string {
	return filepath.Join(s.store.SomaDir(), "triggers.yaml")
}

// LoadWorkspaceHarnessConfig reads and normalizes .soma/harness.json inside workspaceDir.
func LoadWorkspaceHarnessConfig(workspaceDir string) HarnessConfig {
	return loadHarnessConfigFromPath(filepath.Join(workspaceDir, ".soma", "harness.json"))
}

// ResolveDefaultProviderFromHarness resolves a configured ModelProvider and modelID from .soma/harness.json
// (and store accounts when st is non-nil) for shared CLI / daemon / bridge fallback.
func ResolveDefaultProviderFromHarness(ctx context.Context, st *store.Store, workspaceDir string) (dag.ModelProvider, string, bool) {
	cfg := LoadWorkspaceHarnessConfig(workspaceDir)
	if st != nil {
		accs, _ := st.ListAccounts(ctx)
		pick := strings.TrimSpace(cfg.DefaultAccountID)
		if pick == "" && len(accs) > 0 {
			pick = accs[0].ID
		}
		for _, a := range accs {
			if pick != "" && a.ID != pick {
				continue
			}
			if strings.EqualFold(a.Provider, "mock") {
				m := a.ModelID
				if m == "" {
					m = "mock-worker-v1"
				}
				return &dag.MockProvider{ModelName: m}, m, true
			}
			ac := cfg.AccountConfigs[a.ID]
			base, key, defMod, ok := dag.ResolveProviderEndpoint(a.Provider, ac.APIBase, ac.APIKey, a.ModelID)
			if !ok {
				base = strings.TrimSpace(ac.APIBase)
				key = ac.APIKey
			}
			m := a.ModelID
			if m == "" {
				m = defMod
			}
			if base != "" {
				return dag.NewOpenAICompatProvider(base, key), m, true
			}
		}
	}
	if strings.EqualFold(cfg.Provider.ProviderType, "mock") {
		m := cfg.Provider.ModelID
		if m == "" {
			m = "mock-worker-v1"
		}
		return &dag.MockProvider{ModelName: m}, m, true
	}
	if base, key, m, ok := dag.ResolveProviderEndpoint(cfg.Provider.ProviderType, cfg.Provider.APIBase, cfg.Provider.APIKey, cfg.Provider.ModelID); ok && base != "" {
		if cfg.Provider.ModelID != "" {
			m = cfg.Provider.ModelID
		}
		return dag.NewOpenAICompatProvider(base, key), m, true
	}
	if strings.TrimSpace(cfg.Provider.APIBase) != "" {
		return dag.NewOpenAICompatProvider(strings.TrimSpace(cfg.Provider.APIBase), cfg.Provider.APIKey), cfg.Provider.ModelID, true
	}
	return nil, "", false
}

func loadHarnessConfigFromPath(path string) HarnessConfig {
	cfg := HarnessConfig{
		Provider: ProviderConfig{
			ProviderType:      "auto",
			MaxTurns:          15,
			WorkspaceStrategy: "worktree",
			SandboxMode:       "auto",
			AutonomyMode:      "standard",
		},
		MCPServers: []MCPServerCfg{},
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	if jErr := json.Unmarshal(raw, &cfg); jErr != nil && len(bytes.TrimSpace(raw)) > 0 {
		// Never let a damaged file be silently replaced by defaults on the next save: keep a
		// copy of what was there (API keys, connectors, bridge tokens) for recovery.
		backup := path + ".corrupt"
		if _, sErr := os.Stat(backup); sErr != nil {
			_ = os.WriteFile(backup, raw, 0o600)
		}
	}
	if cfg.Provider.MaxTurns <= 0 {
		cfg.Provider.MaxTurns = 15
	}
	if cfg.Provider.WorkspaceStrategy == "" {
		cfg.Provider.WorkspaceStrategy = "worktree"
	}
	if cfg.Provider.SandboxMode == "" {
		cfg.Provider.SandboxMode = "auto"
	}
	if cfg.Provider.AutonomyMode == "" {
		cfg.Provider.AutonomyMode = "standard"
	}
	if cfg.MCPServers == nil {
		cfg.MCPServers = []MCPServerCfg{}
	}
	return cfg
}

func (s *Server) loadHarnessConfig() HarnessConfig {
	return loadHarnessConfigFromPath(s.harnessPath())
}

func (s *Server) saveHarnessConfig(cfg HarnessConfig) error {
	if err := os.MkdirAll(s.store.SomaDir(), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	// Write-then-rename so a concurrent reader never sees a truncated file (which would parse as
	// defaults and be saved back, wiping every credential).
	tmp := s.harnessPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.harnessPath()); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return s.syncMCPYAML(cfg.MCPServers)
}

func (s *Server) syncMCPYAML(servers []MCPServerCfg) error {
	mcpCfg := mcp.ConfigFile{MCPServers: make(map[string]mcp.ServerEndpoint, len(servers))}
	for _, m := range servers {
		en := m.Enabled
		mcpCfg.MCPServers[m.Name] = mcp.ServerEndpoint{
			Name:            m.Name,
			Transport:       m.Transport,
			URL:             m.URL,
			AuthToken:       m.AuthToken,
			Headers:         m.Headers,
			Command:         m.Command,
			Args:            m.Args,
			Env:             m.Env,
			IncludeTools:    m.IncludeTools,
			ExcludeTools:    m.ExcludeTools,
			TimeoutSeconds:  m.TimeoutSeconds,
			OAuth:           m.OAuth,
			Enabled:         &en,
			IsolatedDesktop: m.IsolatedDesktop,
		}
	}
	return mcp.WriteConfigFile(filepath.Join(s.store.SomaDir(), "mcp.yaml"), mcpCfg)
}

func builtinMCPPresetCatalog(containerName string) []MCPPresetDTO {
	if containerName == "" {
		containerName = "soma-desktop"
	}
	pw := mcp.PlaywrightPreset("", true)
	cd := mcp.ChromeDevToolsPreset(true)
	cua := mcp.CUADriverPreset(true)
	dd := mcp.DockerDesktopPreset(containerName)
	dpw := mcp.DockerPlaywrightPreset(containerName)
	out := []MCPPresetDTO{
		{
			ID:              "playwright",
			Name:            "Playwright Browser",
			Category:        "Browser & Web",
			Description:     "Headless or headed browser automation via Microsoft @playwright/mcp.",
			Transport:       pw.EffectiveTransport(),
			Command:         pw.Command,
			Args:            pw.Args,
			IsolatedDesktop: pw.IsolatedDesktop,
		},
		{
			ID:              "chrome-devtools",
			Name:            "Chrome DevTools",
			Category:        "Browser & Web",
			Description:     "Inspect DOM, console logs, network requests, and performance via CDP.",
			Transport:       cd.EffectiveTransport(),
			Command:         cd.Command,
			Args:            cd.Args,
			IsolatedDesktop: cd.IsolatedDesktop,
		},
		{
			ID:              "cua-desktop",
			Name:            "CUA Desktop Driver",
			Category:        "Virtual Desktop",
			Description:     "Non-intrusive accessibility & GUI automation via trycua/cua-driver.",
			Transport:       cua.EffectiveTransport(),
			Command:         cua.Command,
			Args:            cua.Args,
			IsolatedDesktop: cua.IsolatedDesktop,
		},
		{
			ID:              "docker-desktop",
			Name:            "Virtual Desktop Control",
			Category:        "Virtual Desktop",
			Description:     "Mouse, keyboard, and screen control inside soma's isolated Docker desktop.",
			Transport:       dd.EffectiveTransport(),
			Command:         dd.Command,
			Args:            dd.Args,
			IsolatedDesktop: dd.IsolatedDesktop,
		},
		{
			ID:              "docker-playwright",
			Name:            "Desktop Headed Playwright",
			Category:        "Virtual Desktop",
			Description:     "Run Playwright headed on DISPLAY=:99 inside the Virtual Desktop.",
			Transport:       dpw.EffectiveTransport(),
			Command:         dpw.Command,
			Args:            dpw.Args,
			IsolatedDesktop: dpw.IsolatedDesktop,
		},
	}

	for _, p := range mcp.ListPresets() {
		if p.Name == "playwright" || p.Name == "chrome-devtools" || p.Name == "cua-desktop" {
			continue
		}
		ep := p.Build(nil)
		cat := p.Category
		if cat == "" {
			cat = "Connectors"
		}
		out = append(out, MCPPresetDTO{
			ID:           p.Name,
			Name:         p.Name,
			Category:     cat,
			Description:  p.Description,
			Transport:    ep.EffectiveTransport(),
			Command:      ep.Command,
			Args:         ep.Args,
			URL:          ep.URL,
			Env:          ep.Env,
			RequiredEnv:  p.RequiredEnv,
			OAuthEnabled: ep.OAuth != nil && ep.OAuth.Enabled,
		})
	}
	return out
}

func (s *Server) desktopStatusDTO(ctx context.Context) DesktopStatusDTO {
	st, running := s.cachedDesktopStatus(ctx)
	proxyURL := ""
	novncURL := ""
	if running && st.NoVNCPort > 0 {
		novncURL = st.NoVNCURL()
		proxyURL = fmt.Sprintf("/desktop-vnc/vnc.html?autoconnect=true&resize=scale&path=desktop-vnc/websockify&password=%s", url.QueryEscape(st.VNCPassword))
	}
	if st.Image == "" {
		st.Image = desktop.DefaultImage
	}
	if st.Resolution == "" {
		st.Resolution = desktop.DefaultResolution
	}
	return DesktopStatusDTO{
		Running:       running,
		ContainerName: st.ContainerName,
		Image:         st.Image,
		NoVNCPort:     st.NoVNCPort,
		NoVNCURL:      novncURL,
		ProxyURL:      proxyURL,
		Resolution:    st.Resolution,
	}
}

func (s *Server) buildHarnessDTO(ctx context.Context) HarnessDTO {
	cfg := s.loadHarnessConfig()
	desk := s.desktopStatusDTO(ctx)

	// These fields are written under s.mu by the provider/account handlers.
	s.mu.Lock()
	useMock, hasProvider, curModelID := s.cfg.UseMock, s.cfg.Provider != nil, s.cfg.ModelID
	s.mu.Unlock()
	provMode := "read-only"
	if useMock || cfg.Provider.ProviderType == "mock" {
		provMode = "mock"
	} else if hasProvider || strings.TrimSpace(cfg.Provider.APIBase) != "" {
		provMode = "live"
	}
	modelID := cfg.Provider.ModelID
	if modelID == "" {
		modelID = curModelID
	}

	installedByPreset := make(map[string]bool)
	installedByName := make(map[string]bool)
	mcpDTOs := make([]MCPServerDTO, 0, len(cfg.MCPServers))
	for _, m := range cfg.MCPServers {
		if m.PresetID != "" {
			installedByPreset[m.PresetID] = true
		}
		installedByName[m.Name] = true
		var keys []string
		for k := range m.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var hKeys []string
		for k := range m.Headers {
			hKeys = append(hKeys, k)
		}
		sort.Strings(hKeys)
		oauthConnected := false
		if m.OAuth != nil && m.OAuth.Enabled {
			if _, tErr := mcp.DefaultTokenStore().Load(m.Name); tErr == nil {
				oauthConnected = true
			}
		}
		mcpDTOs = append(mcpDTOs, MCPServerDTO{
			Name:            m.Name,
			PresetID:        m.PresetID,
			Transport:       m.Transport,
			Command:         m.Command,
			Args:            m.Args,
			URL:             m.URL,
			HasAuthToken:    strings.TrimSpace(m.AuthToken) != "",
			EnvKeys:         keys,
			HeaderKeys:      hKeys,
			IncludeTools:    m.IncludeTools,
			ExcludeTools:    m.ExcludeTools,
			TimeoutSeconds:  m.TimeoutSeconds,
			OAuthEnabled:    m.OAuth != nil && m.OAuth.Enabled,
			OAuthConnected:  oauthConnected,
			IsolatedDesktop: m.IsolatedDesktop,
			Enabled:         m.Enabled,
		})
	}

	presets := builtinMCPPresetCatalog(desk.ContainerName)
	for i := range presets {
		if installedByPreset[presets[i].ID] || installedByName[presets[i].ID] {
			presets[i].Installed = true
		}
	}

	dCfg, err := daemon.LoadConfig(s.triggersPath())
	if err != nil {
		dCfg = daemon.DefaultConfig()
	}
	if dCfg.Triggers == nil {
		dCfg.Triggers = []daemon.TriggerSpec{}
	}

	accs, _ := s.store.ListAccounts(ctx)
	accDTOs := make([]AccountDTO, 0, len(accs))
	for _, a := range accs {
		var cooldownSec int64
		if a.CooldownUntil > time.Now().Unix() {
			cooldownSec = a.CooldownUntil - time.Now().Unix()
		}
		ac := cfg.AccountConfigs[a.ID]
		role := ac.Role
		if role == "" {
			if cfg.DefaultAccountID == a.ID {
				role = "default"
			} else {
				role = "worker"
			}
		}
		apiBase := ac.APIBase
		if apiBase == "" {
			if base, _, _, ok := dag.ResolveProviderEndpoint(a.Provider, "", "", ""); ok {
				apiBase = base
			}
		}
		accDTOs = append(accDTOs, AccountDTO{
			ID:             a.ID,
			Provider:       a.Provider,
			ModelID:        a.ModelID,
			APIBase:        apiBase,
			HasAPIKey:      strings.TrimSpace(ac.APIKey) != "",
			APIKeyMasked:   maskSecret(ac.APIKey),
			Role:           role,
			IsDefault:      cfg.DefaultAccountID == a.ID || (cfg.DefaultAccountID == "" && len(accs) == 1),
			BillingMode:    a.BillingMode,
			MaxConcurrent:  a.MaxConcurrent,
			ActiveLeases:   a.ActiveLeases,
			CooldownSec:    cooldownSec,
			DailyBudgetUSD: a.DailyBudgetUSD,
			DailySpentUSD:  a.DailySpentUSD,
		})
	}

	var tunnelDTO TunnelDTO
	if s.tunnel != nil {
		tunnelDTO = s.tunnel.Status(cfg.Tunnel)
	}
	var daemonRunning bool
	var bridgeRunning map[string]bool
	var bridgeErrors map[string]string
	if s.supervisor != nil {
		daemonRunning, bridgeRunning, bridgeErrors = s.supervisor.StatusSnapshot()
	}
	trigLast := make(map[string]TriggerRunSummaryDTO)
	if trRuns, tErr := s.store.ListTriggerRuns(ctx, "", 100); tErr == nil {
		for _, tr := range trRuns {
			if _, seen := trigLast[tr.TriggerID]; !seen {
				trigLast[tr.TriggerID] = TriggerRunSummaryDTO{
					Status:    tr.Status,
					BranchID:  tr.BranchID,
					UpdatedAt: tr.CreatedAt,
				}
			}
		}
	}
	return HarnessDTO{
		Provider: ProviderDTO{
			Mode:              provMode,
			ProviderType:      cfg.Provider.ProviderType,
			APIBase:           cfg.Provider.APIBase,
			HasAPIKey:         strings.TrimSpace(cfg.Provider.APIKey) != "",
			APIKeyMasked:      maskSecret(cfg.Provider.APIKey),
			ModelID:           modelID,
			EmbedModel:        cfg.Provider.EmbedModel,
			EmbedURL:          cfg.Provider.EmbedURL,
			MaxTurns:            cfg.Provider.MaxTurns,
			WorkspaceStrategy:   cfg.Provider.WorkspaceStrategy,
			SandboxMode:         cfg.Provider.SandboxMode,
			AllowSandboxNetwork: cfg.Provider.AllowSandboxNetwork,
			AutonomyMode:        cfg.Provider.AutonomyMode,
		},
		ProviderPresets: dag.ListProviderPresets(),
		MCPServers:      mcpDTOs,
		Presets:         presets,
		BridgeRunning:   bridgeRunning,
		BridgeErrors:    bridgeErrors,
		Bridges: BridgesDTO{
			Discord: DiscordBridgeDTO{
				Enabled:           cfg.Bridges.Discord.Enabled,
				HasBotToken:       strings.TrimSpace(cfg.Bridges.Discord.BotToken) != "",
				BotTokenMasked:    maskSecret(cfg.Bridges.Discord.BotToken),
				PublicKeyHex:      cfg.Bridges.Discord.PublicKeyHex,
				ChannelID:         cfg.Bridges.Discord.ChannelID,
				AllowedUserIDs:    cfg.Bridges.Discord.AllowedUserIDs,
				AllowedChannelIDs: cfg.Bridges.Discord.AllowedChannelIDs,
				ListenAddr:        cfg.Bridges.Discord.ListenAddr,
			},
			Telegram: TelegramBridgeDTO{
				Enabled:        cfg.Bridges.Telegram.Enabled,
				HasBotToken:    strings.TrimSpace(cfg.Bridges.Telegram.BotToken) != "",
				BotTokenMasked: maskSecret(cfg.Bridges.Telegram.BotToken),
				AllowedUserIDs: cfg.Bridges.Telegram.AllowedUserIDs,
				AllowedChatIDs: cfg.Bridges.Telegram.AllowedChatIDs,
				AlertChatID:    cfg.Bridges.Telegram.AlertChatID,
			},
			Slack: SlackBridgeDTO{
				Enabled:           cfg.Bridges.Slack.Enabled,
				HasBotToken:       strings.TrimSpace(cfg.Bridges.Slack.BotToken) != "",
				BotTokenMasked:    maskSecret(cfg.Bridges.Slack.BotToken),
				HasSigningSecret:  strings.TrimSpace(cfg.Bridges.Slack.SigningSecret) != "",
				AllowedUserIDs:    cfg.Bridges.Slack.AllowedUserIDs,
				AllowedChannelIDs: cfg.Bridges.Slack.AllowedChannelIDs,
			},
			Matrix: MatrixBridgeDTO{
				Enabled:        cfg.Bridges.Matrix.Enabled,
				HomeserverURL:  cfg.Bridges.Matrix.HomeserverURL,
				HasAccessToken: strings.TrimSpace(cfg.Bridges.Matrix.AccessToken) != "",
				UserID:         cfg.Bridges.Matrix.UserID,
				AllowedSenders: cfg.Bridges.Matrix.AllowedSenders,
				AllowedRooms:   cfg.Bridges.Matrix.AllowedRooms,
			},
			Mattermost: MattermostBridgeDTO{
				Enabled:        cfg.Bridges.Mattermost.Enabled,
				ServerURL:      cfg.Bridges.Mattermost.ServerURL,
				HasBotToken:    strings.TrimSpace(cfg.Bridges.Mattermost.BotToken) != "",
				WebhookURL:     cfg.Bridges.Mattermost.WebhookURL,
				AllowedUserIDs: cfg.Bridges.Mattermost.AllowedUserIDs,
			},
			Teams: WebhookChannelDTO{
				Enabled:        cfg.Bridges.Teams.Enabled,
				WebhookURL:     cfg.Bridges.Teams.WebhookURL,
				AllowedUserIDs: cfg.Bridges.Teams.AllowedUserIDs,
			},
			GoogleChat: WebhookChannelDTO{
				Enabled:        cfg.Bridges.GoogleChat.Enabled,
				WebhookURL:     cfg.Bridges.GoogleChat.WebhookURL,
				AllowedUserIDs: cfg.Bridges.GoogleChat.AllowedUserIDs,
			},
			Email: EmailBridgeDTO{
				Enabled:     cfg.Bridges.Email.Enabled,
				SMTPHost:    cfg.Bridges.Email.SMTPHost,
				SMTPPort:    cfg.Bridges.Email.SMTPPort,
				Username:    cfg.Bridges.Email.Username,
				HasPassword: strings.TrimSpace(cfg.Bridges.Email.Password) != "",
				From:        cfg.Bridges.Email.From,
			},
			WhatsApp: sidecarToDTO(cfg.Bridges.WhatsApp),
			Signal:   sidecarToDTO(cfg.Bridges.Signal),
			SMS:      sidecarToDTO(cfg.Bridges.SMS),
		},
		Pairing: s.pairingDTO(),
		Triggers: TriggersConfigDTO{
			QuietHours:            dCfg.QuietHours,
			MaxPendingInbox:       dCfg.MaxPendingInbox,
			DailyMeteredBudgetUSD: dCfg.DailyMeteredBudgetUSD,
			WebhookAddr:           dCfg.WebhookAddr,
			GCInterval:            dCfg.GCInterval,
			Triggers:              dCfg.Triggers,
		},
		DaemonRunning:  daemonRunning,
		TriggerLastRun: trigLast,
		Accounts:       accDTOs,
		Desktop:  desk,
		Tunnel:   tunnelDTO,
	}
}

func (s *Server) handleGetHarness(w http.ResponseWriter, r *http.Request) {
	writeJSONCompressed(w, r, http.StatusOK, s.buildHarnessDTO(r.Context()))
}

func (s *Server) handleSaveProvider(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProviderType        string `json:"provider_type"`
		APIBase             string `json:"api_base"`
		APIKey              string `json:"api_key"`
		ModelID             string `json:"model_id"`
		EmbedModel          string `json:"embed_model"`
		EmbedURL            string `json:"embed_url"`
		MaxTurns            int    `json:"max_turns"`
		WorkspaceStrategy   string `json:"workspace_strategy"`
		SandboxMode         string `json:"sandbox_mode"`
		AllowSandboxNetwork *bool  `json:"allow_sandbox_network"`
		AutonomyMode        string `json:"autonomy_mode"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	cfg := s.loadHarnessConfig()
	if req.ProviderType != "" {
		cfg.Provider.ProviderType = strings.TrimSpace(req.ProviderType)
	}
	cfg.Provider.APIBase = strings.TrimSpace(req.APIBase)
	if strings.TrimSpace(req.APIKey) != "" {
		cfg.Provider.APIKey = strings.TrimSpace(req.APIKey)
	}
	if strings.TrimSpace(req.ModelID) != "" {
		cfg.Provider.ModelID = strings.TrimSpace(req.ModelID)
	}
	cfg.Provider.EmbedModel = strings.TrimSpace(req.EmbedModel)
	cfg.Provider.EmbedURL = strings.TrimSpace(req.EmbedURL)
	if req.MaxTurns > 0 {
		cfg.Provider.MaxTurns = req.MaxTurns
	}
	if req.WorkspaceStrategy != "" {
		cfg.Provider.WorkspaceStrategy = req.WorkspaceStrategy
	}
	if req.SandboxMode != "" {
		cfg.Provider.SandboxMode = req.SandboxMode
	}
	if req.AllowSandboxNetwork != nil {
		cfg.Provider.AllowSandboxNetwork = *req.AllowSandboxNetwork
	}
	if req.AutonomyMode != "" {
		mode := strings.ToLower(strings.TrimSpace(req.AutonomyMode))
		if mode != "autonomous" {
			mode = "standard"
		}
		cfg.Provider.AutonomyMode = mode
	}

	if err := s.saveHarnessConfig(cfg); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// Dynamically apply provider changes to active ServerConfig when explicitly switched.
	s.mu.Lock()
	if cfg.Provider.ModelID != "" {
		s.cfg.ModelID = cfg.Provider.ModelID
	}
	switch cfg.Provider.ProviderType {
	case "mock":
		s.cfg.UseMock = true
		mName := s.cfg.ModelID
		if mName == "" {
			mName = "mock-worker-v1"
		}
		s.cfg.Provider = &dag.MockProvider{ModelName: mName}
	case "openai_compat":
		if cfg.Provider.APIBase != "" {
			s.cfg.UseMock = false
			s.cfg.Provider = dag.NewOpenAICompatProvider(cfg.Provider.APIBase, cfg.Provider.APIKey)
		}
	default:
		if base, key, model, ok := dag.ResolveProviderEndpoint(cfg.Provider.ProviderType, cfg.Provider.APIBase, cfg.Provider.APIKey, cfg.Provider.ModelID); ok {
			cfg.Provider.APIBase = base
			if cfg.Provider.ModelID == "" {
				cfg.Provider.ModelID = model
				s.cfg.ModelID = model
			}
			_ = s.saveHarnessConfig(cfg)
			s.cfg.UseMock = false
			s.cfg.Provider = dag.NewOpenAICompatProvider(base, key)
		}
	}
	s.mu.Unlock()

	if s.supervisor != nil {
		s.supervisor.Reload()
	}
	s.notify()
	writeJSON(w, http.StatusOK, s.buildHarnessDTO(r.Context()))
}

func (s *Server) handleStartProviderOAuth(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider  string `json:"provider"`
		AccountID string `json:"account_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	prov := strings.ToLower(strings.TrimSpace(req.Provider))
	if prov == "codex" {
		prov = "openai-codex"
	}
	accID := strings.TrimSpace(req.AccountID)
	if accID == "" {
		accID = prov
	}

	var authURL string
	var waitFn func(context.Context) (string, error)
	var cancelFn func()
	var err error

	switch prov {
	case "openrouter":
		authURL, waitFn, cancelFn, err = dag.BeginOpenRouterPKCE(nil)
	case "openai-codex":
		authURL, waitFn, cancelFn, err = dag.BeginCodexPKCE(nil)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("OAuth not supported for provider %q (supported: openrouter, openai-codex)", prov)})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	go func() {
		defer func() {
			if cancelFn != nil {
				cancelFn()
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		token, wErr := waitFn(ctx)
		if wErr != nil || token == "" {
			return
		}
		// Same lock as the config-saving API routes: this completes minutes later and must not
		// overwrite (or be overwritten by) a save made in the meantime.
		s.perf.harnessMu.Lock()
		defer s.perf.harnessMu.Unlock()
		cfg := s.loadHarnessConfig()
		if cfg.AccountConfigs == nil {
			cfg.AccountConfigs = map[string]AccountEndpointCfg{}
		}
		base, _, defModel, _ := dag.ResolveProviderEndpoint(prov, "", token, "")
		ac := cfg.AccountConfigs[accID]
		ac.APIBase = base
		ac.APIKey = token
		if ac.Role == "" {
			ac.Role = "default"
		}
		cfg.AccountConfigs[accID] = ac

		billing := "metered"
		if prov == "openai-codex" {
			billing = "subscription"
		}
		_ = s.store.UpsertAccount(ctx, store.Account{
			ID:            accID,
			Provider:      prov,
			ModelID:       defModel,
			BillingMode:   billing,
			MaxConcurrent: 2,
		})
		s.applyAccountToServerLocked(&cfg, accID, prov, base, token, defModel)
		_ = s.saveHarnessConfig(cfg)
		s.notify()
	}()

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"provider": prov,
		"auth_url": authURL,
	})
}

func (s *Server) handleStartMCPOAuth(w http.ResponseWriter, r *http.Request, name string) {
	cfg := s.loadHarnessConfig()
	var target *MCPServerCfg
	for i := range cfg.MCPServers {
		if cfg.MCPServers[i].Name == name {
			target = &cfg.MCPServers[i]
			break
		}
	}
	if target == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "connector not found"})
		return
	}
	if target.OAuth == nil || !target.OAuth.Enabled {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "connector does not have OAuth 2.1 enabled"})
		return
	}
	ep := mcp.ServerEndpoint{
		Name:      target.Name,
		Transport: target.Transport,
		URL:       target.URL,
		OAuth:     target.OAuth,
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	authURL, waitFn, _, err := mcp.BeginPKCEFlow(ctx, ep, nil)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	go func() {
		wCtx, wCancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer wCancel()
		if _, wErr := waitFn(wCtx); wErr == nil {
			s.notify()
		}
	}()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"server":   target.Name,
		"auth_url": authURL,
	})
}

func (s *Server) handleLogoutMCPOAuth(w http.ResponseWriter, r *http.Request, name string) {
	if err := mcp.DefaultTokenStore().Delete(name); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.notify()
	writeJSON(w, http.StatusOK, s.buildHarnessDTO(r.Context()))
}

func (s *Server) handleSaveTunnelConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode              string `json:"mode"`
		TunnelToken       string `json:"tunnel_token"`
		Hostname          string `json:"hostname"`
		AuthToken         string `json:"auth_token"`
		GenerateAuthToken bool   `json:"generate_auth_token"`
		AutoStart         *bool  `json:"auto_start,omitempty"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	cfg := s.loadHarnessConfig()
	if strings.TrimSpace(req.Mode) != "" {
		cfg.Tunnel.Mode = strings.TrimSpace(req.Mode)
	}
	if strings.TrimSpace(req.TunnelToken) != "" {
		cfg.Tunnel.TunnelToken = strings.TrimSpace(req.TunnelToken)
	}
	cfg.Tunnel.Hostname = strings.TrimSpace(req.Hostname)
	if req.AutoStart != nil {
		cfg.Tunnel.AutoStart = *req.AutoStart
	}

	var generated string
	if req.GenerateAuthToken {
		tok, err := generateRandomAuthToken()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		cfg.Tunnel.AuthToken = tok
		generated = tok
	} else if strings.TrimSpace(req.AuthToken) != "" {
		if len(strings.TrimSpace(req.AuthToken)) < 16 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "access token must be at least 16 characters (or use Generate)"})
			return
		}
		cfg.Tunnel.AuthToken = strings.TrimSpace(req.AuthToken)
	}

	if err := s.saveHarnessConfig(cfg); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if strings.TrimSpace(cfg.Tunnel.AuthToken) != "" {
		s.setAuthToken(cfg.Tunnel.AuthToken)
		s.issueSessionCookie(w, r, cfg.Tunnel.AuthToken)
	}
	s.notify()
	out := map[string]any{
		"harness": s.buildHarnessDTO(r.Context()),
	}
	if generated != "" {
		out["generated_auth_token"] = generated
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleStartTunnel(w http.ResponseWriter, r *http.Request) {
	cfg := s.loadHarnessConfig()
	var generated string
	if strings.TrimSpace(cfg.Tunnel.AuthToken) == "" {
		if active := s.activeAuthToken(); active != "" {
			cfg.Tunnel.AuthToken = active
		} else {
			tok, err := generateRandomAuthToken()
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			cfg.Tunnel.AuthToken = tok
			generated = tok
		}
		if err := s.saveHarnessConfig(cfg); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
	}
	s.setAuthToken(cfg.Tunnel.AuthToken)
	s.issueSessionCookie(w, r, cfg.Tunnel.AuthToken)

	localURL := "http://" + s.Addr()
	if s.tunnel == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "tunnel manager unavailable"})
		return
	}
	if err := s.tunnel.Start(context.Background(), localURL, cfg.Tunnel); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.notify()
	out := map[string]any{
		"ok":      true,
		"harness": s.buildHarnessDTO(r.Context()),
	}
	if generated != "" {
		out["generated_auth_token"] = generated
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleStopTunnel(w http.ResponseWriter, r *http.Request) {
	if s.tunnel != nil {
		s.tunnel.Stop()
	}
	s.notify()
	writeJSON(w, http.StatusOK, s.buildHarnessDTO(r.Context()))
}

func (s *Server) handleUpsertMCP(w http.ResponseWriter, r *http.Request) {
	var req MCPServerCfg
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	cfg := s.loadHarnessConfig()

	if req.PresetID != "" && req.Name == "" {
		desk := s.desktopStatusDTO(r.Context())
		for _, p := range builtinMCPPresetCatalog(desk.ContainerName) {
			if p.ID == req.PresetID {
				req.Name = p.ID
				req.Transport = p.Transport
				req.Command = p.Command
				req.Args = p.Args
				req.URL = p.URL
				if len(req.Env) == 0 && len(p.Env) > 0 {
					req.Env = p.Env
				}
				if p.OAuthEnabled && req.OAuth == nil {
					req.OAuth = &mcp.OAuthConfig{Enabled: true}
				}
				req.IsolatedDesktop = p.IsolatedDesktop
				req.Enabled = true
				break
			}
		}
	}

	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "connector name is required"})
		return
	}
	if req.Transport == "" {
		if req.URL != "" {
			req.Transport = "http"
		} else {
			req.Transport = "stdio"
		}
	}

	replaced := false
	for i := range cfg.MCPServers {
		if cfg.MCPServers[i].Name == req.Name {
			mergedEnv := make(map[string]string, len(cfg.MCPServers[i].Env))
			for k, v := range cfg.MCPServers[i].Env {
				mergedEnv[k] = v
			}
			for k, v := range req.Env {
				if v == "" {
					delete(mergedEnv, k)
				} else {
					mergedEnv[k] = v
				}
			}
			if len(mergedEnv) > 0 {
				req.Env = mergedEnv
			} else {
				req.Env = nil
			}

			mergedHdr := make(map[string]string, len(cfg.MCPServers[i].Headers))
			for k, v := range cfg.MCPServers[i].Headers {
				mergedHdr[k] = v
			}
			for k, v := range req.Headers {
				if v == "" {
					delete(mergedHdr, k)
				} else {
					mergedHdr[k] = v
				}
			}
			if len(mergedHdr) > 0 {
				req.Headers = mergedHdr
			} else {
				req.Headers = nil
			}

			if strings.TrimSpace(req.AuthToken) == "" {
				req.AuthToken = cfg.MCPServers[i].AuthToken
			}
			if req.OAuth != nil && cfg.MCPServers[i].OAuth != nil && strings.TrimSpace(req.OAuth.ClientSecret) == "" {
				req.OAuth.ClientSecret = cfg.MCPServers[i].OAuth.ClientSecret
			}
			cfg.MCPServers[i] = req
			replaced = true
			break
		}
	}
	if !replaced {
		cfg.MCPServers = append(cfg.MCPServers, req)
	}

	if err := s.saveHarnessConfig(cfg); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.notify()
	writeJSON(w, http.StatusOK, s.buildHarnessDTO(r.Context()))
}

func (s *Server) handleToggleMCP(w http.ResponseWriter, r *http.Request, name string) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	cfg := s.loadHarnessConfig()
	found := false
	for i := range cfg.MCPServers {
		if cfg.MCPServers[i].Name == name {
			cfg.MCPServers[i].Enabled = req.Enabled
			found = true
			break
		}
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "connector not found"})
		return
	}
	if err := s.saveHarnessConfig(cfg); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.notify()
	writeJSON(w, http.StatusOK, s.buildHarnessDTO(r.Context()))
}

func (s *Server) handleDeleteMCP(w http.ResponseWriter, r *http.Request, name string) {
	cfg := s.loadHarnessConfig()
	next := make([]MCPServerCfg, 0, len(cfg.MCPServers))
	for _, m := range cfg.MCPServers {
		if m.Name != name {
			next = append(next, m)
		}
	}
	cfg.MCPServers = next
	if err := s.saveHarnessConfig(cfg); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.notify()
	writeJSON(w, http.StatusOK, s.buildHarnessDTO(r.Context()))
}

func (s *Server) handleTestMCP(w http.ResponseWriter, r *http.Request, name string) {
	cfg := s.loadHarnessConfig()
	var target *MCPServerCfg
	for i := range cfg.MCPServers {
		if cfg.MCPServers[i].Name == name {
			target = &cfg.MCPServers[i]
			break
		}
	}
	if target == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "connector not found"})
		return
	}

	ep := mcp.ServerEndpoint{
		Name:            target.Name,
		Transport:       target.Transport,
		Command:         target.Command,
		Args:            target.Args,
		URL:             target.URL,
		AuthToken:       target.AuthToken,
		Env:             target.Env,
		Headers:         target.Headers,
		IncludeTools:    target.IncludeTools,
		ExcludeTools:    target.ExcludeTools,
		TimeoutSeconds:  target.TimeoutSeconds,
		OAuth:           target.OAuth,
		IsolatedDesktop: target.IsolatedDesktop,
		Enabled:         boolPtr(true),
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()

	mgr := mcp.NewManager()
	defer func() { _ = mgr.Close() }()
	if err := mgr.Connect(ctx, ep); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":    false,
			"error": err.Error(),
		})
		return
	}
	toolsMap := mgr.ListTools(target.Name)
	var toolNames []string
	for _, t := range toolsMap[target.Name] {
		toolNames = append(toolNames, t.Name)
	}
	var resURIs []string
	if resMap, rErr := mgr.ListResources(ctx, target.Name); rErr == nil {
		for _, res := range resMap[target.Name] {
			resURIs = append(resURIs, res.URI)
		}
	}
	var promptNames []string
	if pMap, pErr := mgr.ListPrompts(ctx, target.Name); pErr == nil {
		for _, p := range pMap[target.Name] {
			promptNames = append(promptNames, p.Name)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"tools":     toolNames,
		"resources": resURIs,
		"prompts":   promptNames,
	})
}

func (s *Server) handleSaveBridges(w http.ResponseWriter, r *http.Request) {
	var body struct {
		BridgesConfig
		ResetChannel string `json:"reset_channel,omitempty"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	req := body.BridgesConfig
	cfg := s.loadHarnessConfig()

	if ch := strings.ToLower(strings.TrimSpace(body.ResetChannel)); ch != "" {
		switch ch {
		case "discord":
			cfg.Bridges.Discord = DiscordBridgeCfg{}
		case "telegram":
			cfg.Bridges.Telegram = TelegramBridgeCfg{}
		case "slack":
			cfg.Bridges.Slack = SlackBridgeCfg{}
		case "matrix":
			cfg.Bridges.Matrix = MatrixBridgeCfg{}
		case "mattermost":
			cfg.Bridges.Mattermost = MattermostBridgeCfg{}
		case "teams":
			cfg.Bridges.Teams = WebhookChannelCfg{}
		case "google_chat", "google-chat":
			cfg.Bridges.GoogleChat = WebhookChannelCfg{}
		case "email":
			cfg.Bridges.Email = EmailBridgeCfg{}
		case "whatsapp":
			cfg.Bridges.WhatsApp = SidecarBridgeCfg{}
		case "signal":
			cfg.Bridges.Signal = SidecarBridgeCfg{}
		case "sms":
			cfg.Bridges.SMS = SidecarBridgeCfg{}
		default:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown channel " + ch})
			return
		}
		if err := s.saveHarnessConfig(cfg); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if s.supervisor != nil {
			s.supervisor.Reload()
		}
		s.notify()
		writeJSON(w, http.StatusOK, s.buildHarnessDTO(r.Context()))
		return
	}

	// Preserve existing tokens/secrets when left blank in the UI.
	if strings.TrimSpace(req.Discord.BotToken) == "" {
		req.Discord.BotToken = cfg.Bridges.Discord.BotToken
	}
	if strings.TrimSpace(req.Telegram.BotToken) == "" {
		req.Telegram.BotToken = cfg.Bridges.Telegram.BotToken
	}
	if strings.TrimSpace(req.Slack.BotToken) == "" {
		req.Slack.BotToken = cfg.Bridges.Slack.BotToken
	}
	if strings.TrimSpace(req.Slack.SigningSecret) == "" {
		req.Slack.SigningSecret = cfg.Bridges.Slack.SigningSecret
	}
	if strings.TrimSpace(req.Matrix.AccessToken) == "" {
		req.Matrix.AccessToken = cfg.Bridges.Matrix.AccessToken
	}
	if strings.TrimSpace(req.Mattermost.BotToken) == "" {
		req.Mattermost.BotToken = cfg.Bridges.Mattermost.BotToken
	}
	if strings.TrimSpace(req.Mattermost.OutgoingToken) == "" {
		req.Mattermost.OutgoingToken = cfg.Bridges.Mattermost.OutgoingToken
	}
	if strings.TrimSpace(req.Email.Password) == "" {
		req.Email.Password = cfg.Bridges.Email.Password
	}
	preserveSidecarSecrets(&req.WhatsApp, cfg.Bridges.WhatsApp)
	preserveSidecarSecrets(&req.Signal, cfg.Bridges.Signal)
	preserveSidecarSecrets(&req.SMS, cfg.Bridges.SMS)
	cfg.Bridges = req

	if err := s.saveHarnessConfig(cfg); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if s.supervisor != nil {
		s.supervisor.Reload()
	}
	s.notify()
	writeJSON(w, http.StatusOK, s.buildHarnessDTO(r.Context()))
}

func (s *Server) handleSaveTriggers(w http.ResponseWriter, r *http.Request) {
	var req TriggersConfigDTO
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	for _, tr := range req.Triggers {
		tp := strings.ToLower(strings.TrimSpace(tr.Type))
		if tp == "cron" || tp == "watch" || tp == "sentinel" {
			if _, sErr := daemon.ParseSchedule(tr.Interval); sErr != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("automation %q: %v", tr.ID, sErr)})
				return
			}
		}
		if tp == "webhook" && strings.TrimSpace(tr.Filter) != "" {
			if fErr := daemon.ValidateFilter(tr.Filter); fErr != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("automation %q: %v", tr.ID, fErr)})
				return
			}
		}
	}

	dCfg, loadErr := daemon.LoadConfig(s.triggersPath())
	if loadErr != nil {
		dCfg = daemon.DefaultConfig()
	}
	dCfg.QuietHours = strings.TrimSpace(req.QuietHours)
	if req.MaxPendingInbox > 0 {
		dCfg.MaxPendingInbox = req.MaxPendingInbox
	}
	dCfg.DailyMeteredBudgetUSD = req.DailyMeteredBudgetUSD
	if strings.TrimSpace(req.WebhookAddr) != "" {
		dCfg.WebhookAddr = strings.TrimSpace(req.WebhookAddr)
	}
	if strings.TrimSpace(req.GCInterval) != "" {
		dCfg.GCInterval = strings.TrimSpace(req.GCInterval)
	}
	dCfg.Triggers = req.Triggers
	if vErr := daemon.ValidateConfig(dCfg); vErr != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": vErr.Error()})
		return
	}

	raw, err := yaml.Marshal(dCfg)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if err := os.MkdirAll(s.store.SomaDir(), 0o755); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if err := os.WriteFile(s.triggersPath(), raw, 0o600); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if s.supervisor != nil {
		s.supervisor.Reload()
	}
	s.notify()
	writeJSON(w, http.StatusOK, s.buildHarnessDTO(r.Context()))
}

func (s *Server) handleRunTrigger(w http.ResponseWriter, r *http.Request, id string) {
	id = strings.TrimSpace(id)
	dCfg, err := daemon.LoadConfig(s.triggersPath())
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no triggers configured"})
		return
	}
	var found *daemon.TriggerSpec
	for i := range dCfg.Triggers {
		if dCfg.Triggers[i].ID == id {
			found = &dCfg.Triggers[i]
			break
		}
	}
	if found == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("trigger %q not found", id)})
		return
	}
	goal := strings.TrimSpace(found.Prompt)
	if goal == "" && strings.TrimSpace(found.Command) != "" {
		goal = fmt.Sprintf("Execute sentinel trigger %q check command: %s", found.ID, found.Command)
	}
	if goal == "" {
		goal = fmt.Sprintf("Run automation trigger %q (%s)", found.ID, found.Type)
	}
	runID, branchID, lErr := s.launchAsyncRun(r.Context(), "Automation: "+found.ID, goal, "worker", found.MaxTurns)
	if lErr != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": lErr.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"ok":        true,
		"run_id":    runID,
		"branch_id": branchID,
	})
}

func (s *Server) handleUpsertAccount(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID             string  `json:"ID"`
		Provider       string  `json:"Provider"`
		ModelID        string  `json:"ModelID"`
		BillingMode    string  `json:"BillingMode"`
		MaxConcurrent  int     `json:"MaxConcurrent"`
		DailyBudgetUSD float64 `json:"DailyBudgetUSD"`
		APIBase        string  `json:"api_base,omitempty"`
		APIKey         string  `json:"api_key,omitempty"`
		Role           string  `json:"role,omitempty"`
		SetDefault     bool    `json:"set_default,omitempty"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	req.ID = strings.TrimSpace(req.ID)
	if req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "account id is required"})
		return
	}
	if req.MaxConcurrent <= 0 {
		req.MaxConcurrent = 2
	}
	if req.BillingMode == "" {
		req.BillingMode = "subscription"
	}
	if req.BillingMode != "metered" {
		req.DailyBudgetUSD = 0
	}

	cfg := s.loadHarnessConfig()
	if cfg.AccountConfigs == nil {
		cfg.AccountConfigs = map[string]AccountEndpointCfg{}
	}
	prev := cfg.AccountConfigs[req.ID]
	apiKey := strings.TrimSpace(req.APIKey)
	if apiKey == "" {
		apiKey = prev.APIKey
	}
	base, _, defModel, ok := dag.ResolveProviderEndpoint(req.Provider, req.APIBase, apiKey, req.ModelID)
	if !ok {
		base = strings.TrimSpace(req.APIBase)
	}
	modelID := strings.TrimSpace(req.ModelID)
	if modelID == "" {
		modelID = defModel
	}
	role := strings.TrimSpace(req.Role)
	if role == "" {
		role = prev.Role
		if role == "" {
			if req.SetDefault {
				role = "default"
			} else {
				role = "worker"
			}
		}
	}
	if role == "default" {
		req.SetDefault = true
	}

	if err := s.store.UpsertAccount(r.Context(), store.Account{
		ID:             req.ID,
		Provider:       strings.TrimSpace(req.Provider),
		ModelID:        modelID,
		BillingMode:    req.BillingMode,
		MaxConcurrent:  req.MaxConcurrent,
		DailyBudgetUSD: req.DailyBudgetUSD,
	}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	cfg.AccountConfigs[req.ID] = AccountEndpointCfg{
		APIBase: base,
		APIKey:  apiKey,
		Role:    role,
	}
	if req.SetDefault || cfg.DefaultAccountID == "" {
		s.applyAccountToServerLocked(&cfg, req.ID, req.Provider, base, apiKey, modelID)
	}
	_ = s.saveHarnessConfig(cfg)
	if s.supervisor != nil {
		s.supervisor.Reload()
	}
	s.notify()
	writeJSON(w, http.StatusOK, s.buildHarnessDTO(r.Context()))
}

func (s *Server) applyAccountToServerLocked(cfg *HarnessConfig, id, provider, apiBase, apiKey, modelID string) {
	cfg.DefaultAccountID = id
	cfg.Provider.ProviderType = provider
	cfg.Provider.APIBase = apiBase
	if apiKey != "" {
		cfg.Provider.APIKey = apiKey
	}
	if modelID != "" {
		cfg.Provider.ModelID = modelID
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if modelID != "" {
		s.cfg.ModelID = modelID
	}
	if strings.EqualFold(provider, "mock") {
		s.cfg.UseMock = true
		mName := s.cfg.ModelID
		if mName == "" {
			mName = "mock-worker-v1"
		}
		s.cfg.Provider = &dag.MockProvider{ModelName: mName}
		return
	}
	base, key, _, ok := dag.ResolveProviderEndpoint(provider, apiBase, apiKey, modelID)
	if !ok {
		base = apiBase
		key = apiKey
	}
	if base != "" {
		s.cfg.UseMock = false
		s.cfg.Provider = dag.NewOpenAICompatProvider(base, key)
	}
}

func (s *Server) handleActivateAccount(w http.ResponseWriter, r *http.Request, id string) {
	id = strings.TrimSpace(id)
	acc, err := s.store.GetAccount(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "account not found"})
		return
	}
	cfg := s.loadHarnessConfig()
	ac := cfg.AccountConfigs[id]
	s.applyAccountToServerLocked(&cfg, id, acc.Provider, ac.APIBase, ac.APIKey, acc.ModelID)
	_ = s.saveHarnessConfig(cfg)
	if s.supervisor != nil {
		s.supervisor.Reload()
	}
	s.notify()
	writeJSON(w, http.StatusOK, s.buildHarnessDTO(r.Context()))
}

func (s *Server) handleDeleteAccount(w http.ResponseWriter, r *http.Request, id string) {
	id = strings.TrimSpace(id)
	deleted, err := s.store.DeleteAccount(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if !deleted {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "account not found"})
		return
	}
	cfg := s.loadHarnessConfig()
	delete(cfg.AccountConfigs, id)
	if cfg.DefaultAccountID == id {
		cfg.DefaultAccountID = ""
		remaining, _ := s.store.ListAccounts(r.Context())
		if len(remaining) > 0 {
			nxt := remaining[0]
			ac := cfg.AccountConfigs[nxt.ID]
			s.applyAccountToServerLocked(&cfg, nxt.ID, nxt.Provider, ac.APIBase, ac.APIKey, nxt.ModelID)
		} else {
			cfg.Provider.APIBase = ""
			cfg.Provider.APIKey = ""
			cfg.Provider.ModelID = ""
			cfg.Provider.ProviderType = "auto"
			s.mu.Lock()
			s.cfg.Provider = nil
			s.cfg.UseMock = false
			s.cfg.ModelID = ""
			s.mu.Unlock()
		}
	}
	_ = s.saveHarnessConfig(cfg)
	if s.supervisor != nil {
		s.supervisor.Reload()
	}
	s.notify()
	writeJSON(w, http.StatusOK, s.buildHarnessDTO(r.Context()))
}

func (s *Server) handleFetchProviderModels(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider  string `json:"provider"`
		APIBase   string `json:"api_base"`
		APIKey    string `json:"api_key"`
		AccountID string `json:"account_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	apiBase := strings.TrimSpace(req.APIBase)
	apiKey := strings.TrimSpace(req.APIKey)
	if req.AccountID != "" {
		cfg := s.loadHarnessConfig()
		if ac, ok := cfg.AccountConfigs[req.AccountID]; ok {
			if apiBase == "" {
				apiBase = ac.APIBase
			}
			if apiKey == "" {
				apiKey = ac.APIKey
			}
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	res := dag.FetchProviderModels(ctx, nil, req.Provider, apiBase, apiKey)
	writeJSON(w, http.StatusOK, res)
}

// handleDesktopVNCProxy proxies HTTP and WebSocket traffic on /desktop-vnc/* to the local noVNC server
// so the Virtual Desktop embeds cleanly on the same origin without relaxing CSP frame-src.
func (s *Server) handleDesktopVNCProxy(w http.ResponseWriter, r *http.Request) {
	st, running := s.cachedDesktopStatus(r.Context())
	if !running || st.NoVNCPort <= 0 {
		http.Error(w, "virtual desktop is not running", http.StatusServiceUnavailable)
		return
	}

	// WebSocket upgrades are not covered by the browser same-origin policy: refuse cross-site pages.
	if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" {
		if u, err := url.Parse(origin); err != nil || !strings.EqualFold(u.Host, r.Host) {
			http.Error(w, "cross-origin desktop access blocked", http.StatusForbidden)
			return
		}
	}
	// Never forward dashboard credentials into the agent-controlled desktop container.
	r = r.Clone(r.Context())
	r.Header.Del("Cookie")
	r.Header.Del("Authorization")

	targetAddr := fmt.Sprintf("127.0.0.1:%d", st.NoVNCPort)
	trimmedPath := strings.TrimPrefix(r.URL.Path, "/desktop-vnc")
	if trimmedPath == "" {
		trimmedPath = "/"
	}

	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		upstream, err := net.DialTimeout("tcp", targetAddr, 5*time.Second)
		if err != nil {
			http.Error(w, "failed to reach desktop VNC", http.StatusBadGateway)
			return
		}
		defer func() { _ = upstream.Close() }()

		hijacker, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "websocket hijack unsupported", http.StatusInternalServerError)
			return
		}
		clientConn, buf, err := hijacker.Hijack()
		if err != nil {
			return
		}
		defer func() { _ = clientConn.Close() }()

		reqClone := r.Clone(r.Context())
		reqClone.URL.Path = trimmedPath
		reqClone.Host = targetAddr
		if err := reqClone.Write(upstream); err != nil {
			return
		}
		if buf.Reader.Buffered() > 0 {
			pending := make([]byte, buf.Reader.Buffered())
			_, _ = buf.Read(pending)
			_, _ = upstream.Write(pending)
		}

		errCh := make(chan error, 2)
		go func() {
			_, e := io.Copy(upstream, clientConn)
			errCh <- e
		}()
		go func() {
			_, e := io.Copy(clientConn, upstream)
			errCh <- e
		}()
		<-errCh
		return
	}

	targetURL := &url.URL{Scheme: "http", Host: targetAddr}
	proxy := httputil.NewSingleHostReverseProxy(targetURL)
	proxy.Director = func(req *http.Request) {
		req.URL.Scheme = "http"
		req.URL.Host = targetAddr
		req.URL.Path = trimmedPath
		req.URL.RawQuery = r.URL.RawQuery
		req.Host = targetAddr
	}
	proxy.ErrorHandler = func(rw http.ResponseWriter, _ *http.Request, err error) {
		if !errors.Is(err, context.Canceled) {
			http.Error(rw, "desktop proxy error: "+err.Error(), http.StatusBadGateway)
		}
	}
	proxy.ServeHTTP(w, r)
}

func (s *Server) connectConfiguredMCP(ctx context.Context) *mcp.Manager {
	cfg := s.loadHarnessConfig()
	var eps []mcp.ServerEndpoint
	explicitDesktopMCP := false
	for _, m := range cfg.MCPServers {
		if m.Name == "docker-desktop" || m.PresetID == "docker-desktop" {
			explicitDesktopMCP = true
		}
		if !m.Enabled {
			continue
		}
		eps = append(eps, mcp.ServerEndpoint{
			Name:            m.Name,
			Transport:       m.Transport,
			Command:         m.Command,
			Args:            m.Args,
			URL:             m.URL,
			AuthToken:       m.AuthToken,
			Env:             m.Env,
			Headers:         m.Headers,
			IncludeTools:    m.IncludeTools,
			ExcludeTools:    m.ExcludeTools,
			TimeoutSeconds:  m.TimeoutSeconds,
			OAuth:           m.OAuth,
			IsolatedDesktop: m.IsolatedDesktop,
			Enabled:         boolPtr(true),
		})
	}
	if !explicitDesktopMCP {
		if desk := s.desktopStatusDTO(ctx); desk.Running && desk.ContainerName != "" {
			dd := mcp.DockerDesktopPreset(desk.ContainerName)
			dd.Name = "docker-desktop"
			dd.Enabled = boolPtr(true)
			eps = append(eps, dd)
		}
	}
	if len(eps) == 0 {
		return nil
	}
	mgr := mcp.NewManager()
	_ = mgr.ConnectAll(ctx, eps)
	return mgr
}

func sidecarToDTO(c SidecarBridgeCfg) SidecarBridgeDTO {
	return SidecarBridgeDTO{
		Enabled:        c.Enabled,
		OutboundURL:    c.OutboundURL,
		HasBearerToken: strings.TrimSpace(c.BearerToken) != "",
		HasHMACSecret:  strings.TrimSpace(c.HMACSecret) != "",
		AllowedSenders: c.AllowedSenders,
	}
}

func preserveSidecarSecrets(dst *SidecarBridgeCfg, prev SidecarBridgeCfg) {
	if strings.TrimSpace(dst.BearerToken) == "" {
		dst.BearerToken = prev.BearerToken
	}
	if strings.TrimSpace(dst.HMACSecret) == "" {
		dst.HMACSecret = prev.HMACSecret
	}
}

func (s *Server) pairingDTO() PairingDTO {
	ps := bridge.NewPairingStore(s.store.SomaDir())
	approved, _ := ps.ListApproved()
	pending, _ := ps.ListPending()
	if approved == nil {
		approved = []bridge.PairedSender{}
	}
	if pending == nil {
		pending = []bridge.PendingPairing{}
	}
	return PairingDTO{Approved: approved, Pending: pending}
}

func (s *Server) handleImportMCP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Source  string `json:"source"`  // claude-code | cursor | codex | hermes | auto
		Path    string `json:"path"`    // optional override file path
		Content string `json:"content"` // optional raw config content
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	src := strings.TrimSpace(req.Source)
	if src == "" {
		src = "auto"
	}

	var eps []mcp.ServerEndpoint
	var err error
	if strings.TrimSpace(req.Content) != "" {
		ext := ".json"
		if src == "codex" {
			ext = ".toml"
		} else if src == "hermes" {
			ext = ".yaml"
		}
		tmpFile, tErr := os.CreateTemp("", "soma-mcp-import-*"+ext)
		if tErr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": tErr.Error()})
			return
		}
		tmpPath := tmpFile.Name()
		_, _ = tmpFile.WriteString(req.Content)
		_ = tmpFile.Close()
		defer func() { _ = os.Remove(tmpPath) }()
		eps, err = mcp.ImportMCPFromFile(src, tmpPath)
	} else {
		p := strings.TrimSpace(req.Path)
		if p == "" {
			p, err = mcp.DefaultAgentConfigPath(src, s.cfg.WorkspaceDir)
		}
		if err == nil {
			eps, err = mcp.ImportMCPFromFile(src, p)
		}
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	cfg := s.loadHarnessConfig()
	byName := make(map[string]int, len(cfg.MCPServers))
	for i, m := range cfg.MCPServers {
		byName[m.Name] = i
	}
	for _, ep := range eps {
		item := MCPServerCfg{
			Name:            ep.Name,
			Transport:       ep.EffectiveTransport(),
			Command:         ep.Command,
			Args:            ep.Args,
			URL:             ep.URL,
			AuthToken:       ep.AuthToken,
			Env:             ep.Env,
			Headers:         ep.Headers,
			IncludeTools:    ep.IncludeTools,
			ExcludeTools:    ep.ExcludeTools,
			TimeoutSeconds:  ep.TimeoutSeconds,
			OAuth:           ep.OAuth,
			IsolatedDesktop: ep.IsolatedDesktop,
			Enabled:         ep.IsEnabled(),
		}
		if idx, ok := byName[item.Name]; ok {
			cfg.MCPServers[idx] = item
		} else {
			byName[item.Name] = len(cfg.MCPServers)
			cfg.MCPServers = append(cfg.MCPServers, item)
		}
	}
	if err := s.saveHarnessConfig(cfg); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.notify()
	writeJSON(w, http.StatusOK, map[string]any{
		"imported": len(eps),
		"harness":  s.buildHarnessDTO(r.Context()),
	})
}

func (s *Server) handleApprovePairing(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	ps := bridge.NewPairingStore(s.store.SomaDir())
	if _, err := ps.ApproveCode(req.Code); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.notify()
	writeJSON(w, http.StatusOK, s.buildHarnessDTO(r.Context()))
}

func (s *Server) handleRevokePairing(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Channel  string `json:"channel"`
		SenderID string `json:"sender_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	ps := bridge.NewPairingStore(s.store.SomaDir())
	ok, err := ps.Revoke(req.Channel, req.SenderID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "paired sender not found"})
		return
	}
	s.notify()
	writeJSON(w, http.StatusOK, s.buildHarnessDTO(r.Context()))
}

func boolPtr(b bool) *bool { return &b }
