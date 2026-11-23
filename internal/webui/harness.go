package webui

import (
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

	"github.com/ra1ncandy/soma/internal/daemon"
	"github.com/ra1ncandy/soma/internal/dag"
	"github.com/ra1ncandy/soma/internal/desktop"
	"github.com/ra1ncandy/soma/internal/mcp"
	"github.com/ra1ncandy/soma/internal/store"
	"gopkg.in/yaml.v3"
)

// HarnessConfig persists dashboard-managed harness, connector, and bridge settings in .soma/harness.json.
type HarnessConfig struct {
	Provider   ProviderConfig `json:"provider"`
	MCPServers []MCPServerCfg `json:"mcp_servers"`
	Bridges    BridgesConfig  `json:"bridges"`
}

// ProviderConfig configures the active LLM provider, embedder, and execution defaults.
type ProviderConfig struct {
	ProviderType      string `json:"provider_type"` // "auto" | "openai_compat" | "mock"
	APIBase           string `json:"api_base"`
	APIKey            string `json:"api_key,omitempty"`
	ModelID           string `json:"model_id"`
	EmbedModel        string `json:"embed_model"`
	EmbedURL          string `json:"embed_url"`
	MaxTurns          int    `json:"max_turns"`
	WorkspaceStrategy string `json:"workspace_strategy"` // "worktree" | "copy" | "direct"
	SandboxMode       string `json:"sandbox_mode"`       // "auto" | "bwrap" | "landlock" | "none"
}

// MCPServerCfg stores an MCP connector endpoint in .soma/harness.json.
type MCPServerCfg struct {
	Name            string            `json:"name"`
	PresetID        string            `json:"preset_id,omitempty"`
	Transport       string            `json:"transport"` // "stdio" | "http"
	Command         string            `json:"command,omitempty"`
	Args            []string          `json:"args,omitempty"`
	URL             string            `json:"url,omitempty"`
	Env             map[string]string `json:"env,omitempty"`
	IsolatedDesktop bool              `json:"isolated_desktop"`
	Enabled         bool              `json:"enabled"`
}

// BridgesConfig stores Discord and Telegram channel bridge settings.
type BridgesConfig struct {
	Discord  DiscordBridgeCfg  `json:"discord"`
	Telegram TelegramBridgeCfg `json:"telegram"`
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
	Provider   ProviderDTO        `json:"provider"`
	MCPServers []MCPServerDTO     `json:"mcp_servers"`
	Presets    []MCPPresetDTO     `json:"presets"`
	Bridges    BridgesDTO         `json:"bridges"`
	Triggers   TriggersConfigDTO  `json:"triggers"`
	Accounts   []AccountDTO       `json:"accounts"`
	Desktop    DesktopStatusDTO   `json:"desktop"`
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
	MaxTurns          int    `json:"max_turns"`
	WorkspaceStrategy string `json:"workspace_strategy"`
	SandboxMode       string `json:"sandbox_mode"`
}

// MCPServerDTO is the secret-safe MCP connector view.
type MCPServerDTO struct {
	Name            string            `json:"name"`
	PresetID        string            `json:"preset_id,omitempty"`
	Transport       string            `json:"transport"`
	Command         string            `json:"command,omitempty"`
	Args            []string          `json:"args,omitempty"`
	URL             string            `json:"url,omitempty"`
	EnvKeys         []string          `json:"env_keys,omitempty"`
	IsolatedDesktop bool              `json:"isolated_desktop"`
	Enabled         bool              `json:"enabled"`
}

// MCPPresetDTO describes a one-click MCP connector preset.
type MCPPresetDTO struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Category        string   `json:"category"`
	Description     string   `json:"description"`
	Transport       string   `json:"transport"`
	Command         string   `json:"command,omitempty"`
	Args            []string `json:"args,omitempty"`
	IsolatedDesktop bool     `json:"isolated_desktop"`
	Installed       bool     `json:"installed"`
}

// BridgesDTO is the secret-safe bridge view.
type BridgesDTO struct {
	Discord  DiscordBridgeDTO  `json:"discord"`
	Telegram TelegramBridgeDTO `json:"telegram"`
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

func (s *Server) loadHarnessConfig() HarnessConfig {
	cfg := HarnessConfig{
		Provider: ProviderConfig{
			ProviderType:      "auto",
			MaxTurns:          15,
			WorkspaceStrategy: "worktree",
			SandboxMode:       "auto",
		},
		MCPServers: []MCPServerCfg{},
	}
	raw, err := os.ReadFile(s.harnessPath())
	if err != nil {
		return cfg
	}
	_ = json.Unmarshal(raw, &cfg)
	if cfg.Provider.MaxTurns <= 0 {
		cfg.Provider.MaxTurns = 15
	}
	if cfg.Provider.WorkspaceStrategy == "" {
		cfg.Provider.WorkspaceStrategy = "worktree"
	}
	if cfg.Provider.SandboxMode == "" {
		cfg.Provider.SandboxMode = "auto"
	}
	if cfg.MCPServers == nil {
		cfg.MCPServers = []MCPServerCfg{}
	}
	return cfg
}

func (s *Server) saveHarnessConfig(cfg HarnessConfig) error {
	if err := os.MkdirAll(s.store.SomaDir(), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.harnessPath(), b, 0o600)
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
	return []MCPPresetDTO{
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
}

func (s *Server) desktopStatusDTO(ctx context.Context) DesktopStatusDTO {
	mgr := desktop.NewManager()
	st, running, _ := mgr.Status(ctx, s.cfg.WorkspaceDir)
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

	provMode := "read-only"
	if s.cfg.UseMock || cfg.Provider.ProviderType == "mock" {
		provMode = "mock"
	} else if s.cfg.Provider != nil || strings.TrimSpace(cfg.Provider.APIBase) != "" {
		provMode = "live"
	}
	modelID := cfg.Provider.ModelID
	if modelID == "" {
		modelID = s.cfg.ModelID
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
		mcpDTOs = append(mcpDTOs, MCPServerDTO{
			Name:            m.Name,
			PresetID:        m.PresetID,
			Transport:       m.Transport,
			Command:         m.Command,
			Args:            m.Args,
			URL:             m.URL,
			EnvKeys:         keys,
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
		accDTOs = append(accDTOs, AccountDTO{
			ID:             a.ID,
			Provider:       a.Provider,
			ModelID:        a.ModelID,
			BillingMode:    a.BillingMode,
			MaxConcurrent:  a.MaxConcurrent,
			ActiveLeases:   a.ActiveLeases,
			CooldownSec:    cooldownSec,
			DailyBudgetUSD: a.DailyBudgetUSD,
			DailySpentUSD:  a.DailySpentUSD,
		})
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
			MaxTurns:          cfg.Provider.MaxTurns,
			WorkspaceStrategy: cfg.Provider.WorkspaceStrategy,
			SandboxMode:       cfg.Provider.SandboxMode,
		},
		MCPServers: mcpDTOs,
		Presets:    presets,
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
		},
		Triggers: TriggersConfigDTO{
			QuietHours:            dCfg.QuietHours,
			MaxPendingInbox:       dCfg.MaxPendingInbox,
			DailyMeteredBudgetUSD: dCfg.DailyMeteredBudgetUSD,
			WebhookAddr:           dCfg.WebhookAddr,
			GCInterval:            dCfg.GCInterval,
			Triggers:              dCfg.Triggers,
		},
		Accounts: accDTOs,
		Desktop:  desk,
	}
}

func (s *Server) handleGetHarness(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.buildHarnessDTO(r.Context()))
}

func (s *Server) handleSaveProvider(w http.ResponseWriter, r *http.Request) {
	var req ProviderConfig
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
	}
	s.mu.Unlock()

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
			if len(req.Env) == 0 && len(cfg.MCPServers[i].Env) > 0 {
				req.Env = cfg.MCPServers[i].Env
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
		Env:             target.Env,
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
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":    true,
		"tools": toolNames,
	})
}

func (s *Server) handleSaveBridges(w http.ResponseWriter, r *http.Request) {
	var req BridgesConfig
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	cfg := s.loadHarnessConfig()

	// Preserve existing tokens when left blank in the UI.
	if strings.TrimSpace(req.Discord.BotToken) == "" {
		req.Discord.BotToken = cfg.Bridges.Discord.BotToken
	}
	if strings.TrimSpace(req.Telegram.BotToken) == "" {
		req.Telegram.BotToken = cfg.Bridges.Telegram.BotToken
	}
	cfg.Bridges = req

	if err := s.saveHarnessConfig(cfg); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
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
	dCfg := daemon.DefaultConfig()
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
	s.notify()
	writeJSON(w, http.StatusOK, s.buildHarnessDTO(r.Context()))
}

func (s *Server) handleUpsertAccount(w http.ResponseWriter, r *http.Request) {
	var req store.Account
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
	if err := s.store.UpsertAccount(r.Context(), req); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.notify()
	writeJSON(w, http.StatusOK, s.buildHarnessDTO(r.Context()))
}

// handleDesktopVNCProxy proxies HTTP and WebSocket traffic on /desktop-vnc/* to the local noVNC server
// so the Virtual Desktop embeds cleanly on the same origin without relaxing CSP frame-src.
func (s *Server) handleDesktopVNCProxy(w http.ResponseWriter, r *http.Request) {
	mgr := desktop.NewManager()
	st, running, _ := mgr.Status(r.Context(), s.cfg.WorkspaceDir)
	if !running || st.NoVNCPort <= 0 {
		http.Error(w, "virtual desktop is not running", http.StatusServiceUnavailable)
		return
	}

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
	for _, m := range cfg.MCPServers {
		if !m.Enabled {
			continue
		}
		eps = append(eps, mcp.ServerEndpoint{
			Name:            m.Name,
			Transport:       m.Transport,
			Command:         m.Command,
			Args:            m.Args,
			URL:             m.URL,
			Env:             m.Env,
			IsolatedDesktop: m.IsolatedDesktop,
			Enabled:         boolPtr(true),
		})
	}
	if len(eps) == 0 {
		return nil
	}
	mgr := mcp.NewManager()
	_ = mgr.ConnectAll(ctx, eps)
	return mgr
}

func boolPtr(b bool) *bool { return &b }
