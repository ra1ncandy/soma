package webui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ra1ncandy/soma/internal/compiler"
	"github.com/ra1ncandy/soma/internal/dag"
	"github.com/ra1ncandy/soma/internal/desktop"
	somaembed "github.com/ra1ncandy/soma/internal/embed"
	"github.com/ra1ncandy/soma/internal/inbox"
	"github.com/ra1ncandy/soma/internal/mcp"
	"github.com/ra1ncandy/soma/internal/memory"
	"github.com/ra1ncandy/soma/internal/oracle"
	"github.com/ra1ncandy/soma/internal/replay"
	"github.com/ra1ncandy/soma/internal/sandbox"
	"github.com/ra1ncandy/soma/internal/skills"
	"github.com/ra1ncandy/soma/internal/store"
	"github.com/ra1ncandy/soma/internal/vmem"
	"github.com/ra1ncandy/soma/internal/workspace"
)

//go:embed static/*
var staticAssets embed.FS

var (
	// ErrRemoteBindRefused is returned when binding a non-loopback address without AllowRemote + AuthToken.
	ErrRemoteBindRefused = errors.New("webui: refusing to bind non-loopback address without --allow-remote and SOMA_WEB_TOKEN")
)

// ServerConfig configures the embedded local web UI HTTP server.
type ServerConfig struct {
	WorkspaceDir string
	Addr         string
	Version      string
	Provider     dag.ModelProvider
	ModelID      string
	UseMock      bool
	AllowRemote  bool
	AuthToken    string
}

// Server serves the minimal embedded soma web dashboard backed by store.Store.
type Server struct {
	store      *store.Store
	cfg        ServerConfig
	inbox      *inbox.Service
	vm         *vmem.Manager
	wsMgr      *workspace.Manager
	csrf       string
	static     fs.FS
	tunnel     *TunnelManager
	supervisor *RuntimeSupervisor

	mu                sync.Mutex
	listener          net.Listener
	server            *http.Server
	wg                sync.WaitGroup
	activeCancels     map[string]context.CancelFunc
	activeTools       map[string]string
	desktopLastActive time.Time
	desktopTakeOver   bool

	stopOnce sync.Once
	stopCh   chan struct{}
	subMu    sync.Mutex
	subs     map[chan struct{}]struct{}
}

// StateDTO is the clean, UI-friendly JSON snapshot returned by GET /api/state.
type StateDTO struct {
	WorkspaceDir     string       `json:"workspace_dir"`
	Version          string       `json:"version"`
	SandboxName      string       `json:"sandbox_name"`
	SandboxReady     bool         `json:"sandbox_ready"`
	ProviderMode     string       `json:"provider_mode"` // "live" | "mock" | "read-only"
	ModelID          string       `json:"model_id"`
	PendingApprovals int          `json:"pending_approvals"`
	Runs             []RunDTO     `json:"runs"`
	Inbox            []InboxDTO   `json:"inbox"`
	Memory           []MemoryDTO  `json:"memory"`
	Skills           []SkillDTO   `json:"skills"`
	Accounts         []AccountDTO `json:"accounts"`
	LoadedAt         int64        `json:"loaded_at"`
}

// RunDTO summarizes a single user task and its branches.
type RunDTO struct {
	ID             string      `json:"id"`
	Title          string      `json:"title"`
	Goal           string      `json:"goal"`
	Status         string      `json:"status"`
	CreatedAt      int64       `json:"created_at"`
	UpdatedAt      int64       `json:"updated_at"`
	ContractsCount int         `json:"contracts_count"`
	Branches       []BranchDTO `json:"branches"`
	Pages          []PageDTO   `json:"pages"`
}

// FrameTokensDTO holds Layer 0-3 token counts for the collapsible Details strip.
type FrameTokensDTO struct {
	L0    int `json:"l0"`
	L1    int `json:"l1"`
	L2    int `json:"l2"`
	L3    int `json:"l3"`
	Total int `json:"total"`
}

// BranchDTO represents one execution trajectory inside a run.
type BranchDTO struct {
	ID                string         `json:"id"`
	RunID             string         `json:"run_id"`
	ParentBranchID    string         `json:"parent_branch_id,omitempty"`
	Label             string         `json:"label"`
	Status            string         `json:"status"`
	ActiveTool        string         `json:"active_tool,omitempty"`
	ChangedFilesCount int            `json:"changed_files_count"`
	Epoch             int            `json:"epoch"`
	WorkspaceDir      string         `json:"workspace_dir"`
	WorkspaceStrategy string         `json:"workspace_strategy"`
	AccountID         string         `json:"account_id"`
	StallCount        int            `json:"stall_count"`
	ContractsPassed   int            `json:"contracts_passed"`
	ContractsTotal    int            `json:"contracts_total"`
	PromptTokens      int            `json:"prompt_tokens"`
	CachedTokens      int            `json:"cached_tokens"`
	CacheHitPct       float64        `json:"cache_hit_pct"`
	FrameTokens       FrameTokensDTO `json:"frame_tokens"`
	Steps             []StepDTO      `json:"steps"`
}

// StepDTO represents a human-readable step card in the task timeline.
type StepDTO struct {
	ID                string        `json:"id"`
	StepType          string        `json:"step_type,omitempty"`
	TurnIndex         int           `json:"turn_index"`
	Epoch             int           `json:"epoch"`
	ModelID           string        `json:"model_id"`
	AccountID         string        `json:"account_id"`
	LatencyMS         int64         `json:"latency_ms"`
	CreatedAt         int64         `json:"created_at"`
	Title             string        `json:"title"`
	Reasoning         string        `json:"reasoning"`
	FinalSummary      string        `json:"final_summary,omitempty"`
	Done              bool          `json:"done"`
	ObservationPageID string        `json:"observation_page_id,omitempty"`
	ToolCalls         []ToolStepDTO `json:"tool_calls,omitempty"`
}

// ToolStepDTO captures a single tool invocation & observation cleanly.
type ToolStepDTO struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	ArgsJSON    string `json:"args_json,omitempty"`
	Observation string `json:"observation,omitempty"`
	PageID      string `json:"page_id,omitempty"`
}

// PageDTO describes a spilled @page virtual memory handle.
type PageDTO struct {
	ID          string `json:"id"`
	ContentType string `json:"content_type"`
	TrustTier   string `json:"trust_tier"`
	ByteSize    int64  `json:"byte_size"`
	LineCount   int    `json:"line_count"`
	Preview     string `json:"preview"`
}

// InboxDTO represents an approval, memory proposal, or reconciliation card.
type InboxDTO struct {
	ID              string `json:"id"`
	RunID           string `json:"run_id,omitempty"`
	RunTitle        string `json:"run_title,omitempty"`
	BranchID        string `json:"branch_id,omitempty"`
	ItemKind        string `json:"item_kind"`
	Title           string `json:"title"`
	Summary         string `json:"summary"`
	ArtifactRef     string `json:"artifact_ref,omitempty"`
	ActionTool      string `json:"action_tool,omitempty"`
	ActionArgs      string `json:"action_args,omitempty"`
	ProposedContent string `json:"proposed_content,omitempty"`
	Urgency         string `json:"urgency"`
	Status          string `json:"status"`
	CreatedAt       int64  `json:"created_at"`
}

// MemoryDTO represents a learned memory rule/fact with human-readable replacement info.
type MemoryDTO struct {
	ID         string `json:"id"`
	Scope      string `json:"scope"`
	Kind       string `json:"kind"`
	Content    string `json:"content"`
	Status     string `json:"status"`
	TrustTier  string `json:"trust_tier"`
	Enabled    bool   `json:"enabled"`
	ReplacesID string `json:"replaces_id,omitempty"`
	IngestedAt int64  `json:"ingested_at"`
}

// SkillDTO summarizes an installed AAIF skill and its lint badge.
type SkillDTO struct {
	Name          string `json:"name"`
	Description   string `json:"description"`
	Badge         string `json:"badge"` // CLEAN | WARN | BLOCKED
	LintCount     int    `json:"lint_count"`
	CriticalCount int    `json:"critical_count"`
}

// AccountDTO summarizes model subscription/quota state without secrets.
type AccountDTO struct {
	ID             string  `json:"id"`
	Provider       string  `json:"provider"`
	ModelID        string  `json:"model_id"`
	APIBase        string  `json:"api_base,omitempty"`
	HasAPIKey      bool    `json:"has_api_key"`
	APIKeyMasked   string  `json:"api_key_masked,omitempty"`
	Role           string  `json:"role,omitempty"`
	IsDefault      bool    `json:"is_default"`
	BillingMode    string  `json:"billing_mode"`
	MaxConcurrent  int     `json:"max_concurrent"`
	ActiveLeases   int     `json:"active_leases"`
	CooldownSec    int64   `json:"cooldown_sec"`
	DailyBudgetUSD float64 `json:"daily_budget_usd"`
	DailySpentUSD  float64 `json:"daily_spent_usd"`
}

// NewServer constructs a web UI Server enforcing loopback-only binding unless explicitly unlocked.
func NewServer(st *store.Store, cfg ServerConfig) (*Server, error) {
	if st == nil {
		return nil, errors.New("webui: non-nil store is required")
	}
	if cfg.WorkspaceDir == "" {
		cfg.WorkspaceDir = filepath.Dir(st.SomaDir())
	}
	if cfg.Addr == "" {
		cfg.Addr = "127.0.0.1:7420"
	}
	if !isLoopbackAddr(cfg.Addr) && (!cfg.AllowRemote || strings.TrimSpace(cfg.AuthToken) == "") {
		return nil, ErrRemoteBindRefused
	}
	if cfg.Version == "" {
		cfg.Version = "v0.5.0"
	}

	csrfBytes := make([]byte, 16)
	if _, err := rand.Read(csrfBytes); err != nil {
		return nil, fmt.Errorf("webui generate csrf token: %w", err)
	}
	subFS, err := fs.Sub(staticAssets, "static")
	if err != nil {
		return nil, err
	}

	srv := &Server{
		store:         st,
		cfg:           cfg,
		inbox:         inbox.NewService(st),
		vm:            vmem.NewManager(st),
		wsMgr:         workspace.NewManager(st.SomaDir()),
		csrf:          hex.EncodeToString(csrfBytes),
		static:        subFS,
		tunnel:        NewTunnelManager(),
		activeCancels: make(map[string]context.CancelFunc),
		activeTools:   make(map[string]string),
		stopCh:        make(chan struct{}),
		subs:          make(map[chan struct{}]struct{}),
	}
	if strings.TrimSpace(srv.cfg.AuthToken) == "" {
		if hCfg := srv.loadHarnessConfig(); strings.TrimSpace(hCfg.Tunnel.AuthToken) != "" {
			srv.cfg.AuthToken = strings.TrimSpace(hCfg.Tunnel.AuthToken)
		}
	}
	if srv.cfg.Provider == nil && !srv.cfg.UseMock {
		srv.restoreProviderFromHarness(context.Background())
	}
	srv.supervisor = newRuntimeSupervisor(srv)
	return srv, nil
}

// CSRFToken returns the active server CSRF token (exposed for local integration tests).
func (s *Server) CSRFToken() string { return s.csrf }

// Addr returns the bound listener address once started, or the configured address beforehand.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener != nil {
		return s.listener.Addr().String()
	}
	return s.cfg.Addr
}

// Start binds the HTTP listener and serves until ctx cancellation or Stop.
func (s *Server) Start(ctx context.Context) error {
	ln, err := s.Listen()
	if err != nil {
		return err
	}
	return s.ServeListener(ctx, ln)
}

// Listen binds the configured TCP address after validating loopback/remote safety.
func (s *Server) Listen() (net.Listener, error) {
	if !isLoopbackAddr(s.cfg.Addr) && (!s.cfg.AllowRemote || strings.TrimSpace(s.cfg.AuthToken) == "") {
		return nil, ErrRemoteBindRefused
	}
	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("webui listen %s: %w", s.cfg.Addr, err)
	}
	s.mu.Lock()
	s.listener = ln
	s.mu.Unlock()
	return ln, nil
}

// ServeListener serves HTTP traffic on an existing listener until ctx is canceled or Stop is called.
func (s *Server) ServeListener(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	s.mu.Lock()
	s.listener = ln
	s.server = srv
	s.mu.Unlock()

	if s.supervisor != nil {
		s.supervisor.Start(ctx)
	}

	if hCfg := s.loadHarnessConfig(); hCfg.Tunnel.AutoStart && strings.TrimSpace(hCfg.Tunnel.AuthToken) != "" && s.tunnel != nil {
		s.setAuthToken(hCfg.Tunnel.AuthToken)
		localURL := "http://" + ln.Addr().String()
		go func() {
			if err := s.tunnel.Start(ctx, localURL, hCfg.Tunnel); err != nil {
				s.tunnel.mu.Lock()
				s.tunnel.lastErr = err.Error()
				s.tunnel.mu.Unlock()
			}
			s.notify()
		}()
	}

	errCh := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = s.Stop(shutCtx)
		return <-errCh
	case err := <-errCh:
		return err
	}
}

// Stop closes active SSE streams, shuts down the HTTP server, and waits for background runs.
func (s *Server) Stop(ctx context.Context) error {
	s.stopOnce.Do(func() { close(s.stopCh) })
	if s.supervisor != nil {
		s.supervisor.Stop()
	}
	if s.tunnel != nil {
		s.tunnel.Stop()
	}
	if strings.TrimSpace(s.cfg.WorkspaceDir) != "" {
		deskCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = desktop.NewManager().Stop(deskCtx, s.cfg.WorkspaceDir)
		cancel()
	}

	s.mu.Lock()
	srv := s.server
	ln := s.listener
	s.mu.Unlock()

	var err error
	if srv != nil {
		err = srv.Shutdown(ctx)
	} else if ln != nil {
		err = ln.Close()
	}
	s.wg.Wait()
	return err
}

// Handler returns the HTTP router protected by CSP, Origin check, optional Bearer auth, and CSRF verification.
func (s *Server) Handler() http.Handler {
	staticSrv := http.FileServer(http.FS(s.static))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; img-src 'self' data:; connect-src 'self' ws: wss:; frame-src 'self'")

		path := r.URL.Path
		if strings.HasPrefix(path, "/webhooks/triggers/") {
			if s.supervisor != nil {
				s.supervisor.ServeTriggerWebhook(w, r)
				return
			}
			http.NotFound(w, r)
			return
		}
		if strings.HasPrefix(path, "/webhooks/bridges/") {
			if s.supervisor != nil {
				s.supervisor.ServeBridgeWebhook(w, r)
				return
			}
			http.NotFound(w, r)
			return
		}
		if path == "/desktop-vnc" || strings.HasPrefix(path, "/desktop-vnc/") {
			if s.activeAuthToken() != "" && !s.validAuth(r) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			s.handleDesktopVNCProxy(w, r)
			return
		}
		if r.Method == http.MethodGet && (path == "/" || path == "/index.html") {
			s.serveIndex(w)
			return
		}
		if r.Method == http.MethodGet && strings.HasPrefix(path, "/static/") {
			http.StripPrefix("/static/", staticSrv).ServeHTTP(w, r)
			return
		}

		if strings.HasPrefix(path, "/api/") {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				if err := s.validateMutation(r); err != nil {
					writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
					return
				}
			}
			if path == "/api/auth/status" && r.Method == http.MethodGet {
				s.handleAuthStatus(w, r)
				return
			}
			if path == "/api/auth/login" && r.Method == http.MethodPost {
				s.handleAuthLogin(w, r)
				return
			}
			if path == "/api/auth/logout" && r.Method == http.MethodPost {
				s.handleAuthLogout(w, r)
				return
			}
			if s.activeAuthToken() != "" && !s.validAuth(r) {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
				return
			}
		}

		switch {
		case r.Method == http.MethodGet && path == "/api/desktop/status":
			s.handleDesktopStatus(w, r)
		case r.Method == http.MethodPost && path == "/api/desktop/start":
			s.handleDesktopStart(w, r)
		case r.Method == http.MethodPost && path == "/api/desktop/stop":
			s.handleDesktopStop(w, r)
		case r.Method == http.MethodPost && path == "/api/desktop/takeover":
			s.handleDesktopTakeOver(w, r)
		case r.Method == http.MethodGet && path == "/api/harness":
			s.handleGetHarness(w, r)
		case r.Method == http.MethodPost && path == "/api/harness/provider":
			s.handleSaveProvider(w, r)
		case r.Method == http.MethodPost && path == "/api/harness/provider/oauth/start":
			s.handleStartProviderOAuth(w, r)
		case r.Method == http.MethodPost && path == "/api/harness/triggers":
			s.handleSaveTriggers(w, r)
		case r.Method == http.MethodPost && strings.HasPrefix(path, "/api/harness/triggers/") && strings.HasSuffix(path, "/run"):
			trID := strings.TrimSuffix(strings.TrimPrefix(path, "/api/harness/triggers/"), "/run")
			s.handleRunTrigger(w, r, trID)
		case r.Method == http.MethodPost && path == "/api/harness/models":
			s.handleFetchProviderModels(w, r)
		case r.Method == http.MethodPost && path == "/api/harness/accounts":
			s.handleUpsertAccount(w, r)
		case r.Method == http.MethodPost && strings.HasPrefix(path, "/api/harness/accounts/") && strings.HasSuffix(path, "/activate"):
			accID := strings.TrimSuffix(strings.TrimPrefix(path, "/api/harness/accounts/"), "/activate")
			s.handleActivateAccount(w, r, accID)
		case r.Method == http.MethodPost && strings.HasPrefix(path, "/api/harness/accounts/") && strings.HasSuffix(path, "/delete"):
			accID := strings.TrimSuffix(strings.TrimPrefix(path, "/api/harness/accounts/"), "/delete")
			s.handleDeleteAccount(w, r, accID)
		case r.Method == http.MethodPost && path == "/api/tunnel/config":
			s.handleSaveTunnelConfig(w, r)
		case r.Method == http.MethodPost && path == "/api/tunnel/start":
			s.handleStartTunnel(w, r)
		case r.Method == http.MethodPost && path == "/api/tunnel/stop":
			s.handleStopTunnel(w, r)
		case r.Method == http.MethodPost && path == "/api/connectors/mcp":
			s.handleUpsertMCP(w, r)
		case r.Method == http.MethodPost && path == "/api/connectors/mcp/import":
			s.handleImportMCP(w, r)
		case r.Method == http.MethodPost && strings.HasPrefix(path, "/api/connectors/mcp/") && strings.HasSuffix(path, "/toggle"):
			name := strings.TrimSuffix(strings.TrimPrefix(path, "/api/connectors/mcp/"), "/toggle")
			s.handleToggleMCP(w, r, name)
		case r.Method == http.MethodPost && strings.HasPrefix(path, "/api/connectors/mcp/") && strings.HasSuffix(path, "/test"):
			name := strings.TrimSuffix(strings.TrimPrefix(path, "/api/connectors/mcp/"), "/test")
			s.handleTestMCP(w, r, name)
		case r.Method == http.MethodPost && strings.HasPrefix(path, "/api/connectors/mcp/") && strings.HasSuffix(path, "/delete"):
			name := strings.TrimSuffix(strings.TrimPrefix(path, "/api/connectors/mcp/"), "/delete")
			s.handleDeleteMCP(w, r, name)
		case r.Method == http.MethodPost && strings.HasPrefix(path, "/api/connectors/mcp/") && strings.HasSuffix(path, "/oauth/start"):
			name := strings.TrimSuffix(strings.TrimPrefix(path, "/api/connectors/mcp/"), "/oauth/start")
			s.handleStartMCPOAuth(w, r, name)
		case r.Method == http.MethodPost && strings.HasPrefix(path, "/api/connectors/mcp/") && strings.HasSuffix(path, "/oauth/logout"):
			name := strings.TrimSuffix(strings.TrimPrefix(path, "/api/connectors/mcp/"), "/oauth/logout")
			s.handleLogoutMCPOAuth(w, r, name)
		case r.Method == http.MethodPost && path == "/api/connectors/bridges":
			s.handleSaveBridges(w, r)
		case r.Method == http.MethodPost && path == "/api/connectors/bridges/pair":
			s.handleApprovePairing(w, r)
		case r.Method == http.MethodPost && path == "/api/connectors/bridges/unpair":
			s.handleRevokePairing(w, r)
		case r.Method == http.MethodGet && path == "/api/state":
			s.handleState(w, r)
		case r.Method == http.MethodGet && path == "/api/events":
			s.handleEvents(w, r)
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/pages/"):
			pageID := strings.TrimPrefix(path, "/api/pages/")
			s.handleGetPage(w, r, pageID)
		case r.Method == http.MethodPost && path == "/api/runs":
			s.handleStartRun(w, r)
		case r.Method == http.MethodPost && strings.HasPrefix(path, "/api/runs/") && strings.HasSuffix(path, "/rename"):
			runID := strings.TrimSuffix(strings.TrimPrefix(path, "/api/runs/"), "/rename")
			s.handleRenameRun(w, r, runID)
		case r.Method == http.MethodPost && strings.HasPrefix(path, "/api/runs/") && strings.HasSuffix(path, "/delete"):
			runID := strings.TrimSuffix(strings.TrimPrefix(path, "/api/runs/"), "/delete")
			s.handleDeleteRun(w, r, runID)
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/branches/") && strings.HasSuffix(path, "/diff"):
			brID := strings.TrimSuffix(strings.TrimPrefix(path, "/api/branches/"), "/diff")
			s.handleBranchDiff(w, r, brID)
		case r.Method == http.MethodPost && strings.HasPrefix(path, "/api/branches/") && strings.HasSuffix(path, "/apply"):
			brID := strings.TrimSuffix(strings.TrimPrefix(path, "/api/branches/"), "/apply")
			s.handleBranchApply(w, r, brID)
		case r.Method == http.MethodPost && strings.HasPrefix(path, "/api/branches/") && strings.HasSuffix(path, "/resume"):
			brID := strings.TrimSuffix(strings.TrimPrefix(path, "/api/branches/"), "/resume")
			s.handleResumeBranch(w, r, brID)
		case r.Method == http.MethodPost && strings.HasPrefix(path, "/api/branches/") && strings.HasSuffix(path, "/continue"):
			brID := strings.TrimSuffix(strings.TrimPrefix(path, "/api/branches/"), "/continue")
			s.handleContinueBranch(w, r, brID)
		case r.Method == http.MethodPost && strings.HasPrefix(path, "/api/branches/") && strings.HasSuffix(path, "/stop"):
			brID := strings.TrimSuffix(strings.TrimPrefix(path, "/api/branches/"), "/stop")
			s.handleStopBranch(w, r, brID)
		case r.Method == http.MethodPost && strings.HasPrefix(path, "/api/branches/") && strings.HasSuffix(path, "/fork"):
			brID := strings.TrimSuffix(strings.TrimPrefix(path, "/api/branches/"), "/fork")
			s.handleForkBranch(w, r, brID)
		case r.Method == http.MethodPost && strings.HasPrefix(path, "/api/branches/") && strings.HasSuffix(path, "/replay"):
			brID := strings.TrimSuffix(strings.TrimPrefix(path, "/api/branches/"), "/replay")
			s.handleReplayBranch(w, r, brID)
		case r.Method == http.MethodPost && strings.HasPrefix(path, "/api/inbox/") && strings.HasSuffix(path, "/approve"):
			itemID := strings.TrimSuffix(strings.TrimPrefix(path, "/api/inbox/"), "/approve")
			s.handleInboxAction(w, r, itemID, true)
		case r.Method == http.MethodPost && strings.HasPrefix(path, "/api/inbox/") && strings.HasSuffix(path, "/dismiss"):
			itemID := strings.TrimSuffix(strings.TrimPrefix(path, "/api/inbox/"), "/dismiss")
			s.handleInboxAction(w, r, itemID, false)
		case r.Method == http.MethodPost && path == "/api/memory":
			s.handleCreateMemory(w, r)
		case r.Method == http.MethodPost && strings.HasPrefix(path, "/api/memory/") && strings.HasSuffix(path, "/toggle"):
			nodeID := strings.TrimSuffix(strings.TrimPrefix(path, "/api/memory/"), "/toggle")
			s.handleMemoryToggle(w, r, nodeID)
		case r.Method == http.MethodPost && strings.HasPrefix(path, "/api/memory/") && strings.HasSuffix(path, "/activate"):
			nodeID := strings.TrimSuffix(strings.TrimPrefix(path, "/api/memory/"), "/activate")
			s.handleMemoryActivate(w, r, nodeID)
		case r.Method == http.MethodPost && strings.HasPrefix(path, "/api/memory/") && strings.HasSuffix(path, "/update"):
			nodeID := strings.TrimSuffix(strings.TrimPrefix(path, "/api/memory/"), "/update")
			s.handleMemoryUpdate(w, r, nodeID)
		case r.Method == http.MethodPost && strings.HasPrefix(path, "/api/memory/") && strings.HasSuffix(path, "/delete"):
			nodeID := strings.TrimSuffix(strings.TrimPrefix(path, "/api/memory/"), "/delete")
			s.handleMemoryDelete(w, r, nodeID)
		case r.Method == http.MethodPost && path == "/api/skills":
			s.handleCreateSkill(w, r)
		case r.Method == http.MethodPost && strings.HasPrefix(path, "/api/skills/") && strings.HasSuffix(path, "/delete"):
			name := strings.TrimSuffix(strings.TrimPrefix(path, "/api/skills/"), "/delete")
			s.handleDeleteSkill(w, r, name)
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/skills/"):
			name := strings.TrimPrefix(path, "/api/skills/")
			s.handleGetSkill(w, r, name)
		default:
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		}
	})
}

func (s *Server) serveIndex(w http.ResponseWriter) {
	raw, err := fs.ReadFile(s.static, "index.html")
	if err != nil {
		http.Error(w, "missing embedded index.html", http.StatusInternalServerError)
		return
	}
	rendered := strings.Replace(string(raw), "__SOMA_CSRF__", s.csrf, 1)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(rendered))
}

func (s *Server) activeAuthToken() string {
	s.mu.Lock()
	tok := strings.TrimSpace(s.cfg.AuthToken)
	s.mu.Unlock()
	return tok
}

func (s *Server) setAuthToken(tok string) {
	s.mu.Lock()
	s.cfg.AuthToken = strings.TrimSpace(tok)
	s.mu.Unlock()
}

func (s *Server) validAuth(r *http.Request) bool {
	expected := s.activeAuthToken()
	if expected == "" {
		return true
	}
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		got := strings.TrimPrefix(auth, "Bearer ")
		if len(got) == len(expected) && subtle.ConstantTimeCompare([]byte(got), []byte(expected)) == 1 {
			return true
		}
	}
	if c, err := r.Cookie("soma_session"); err == nil && c != nil {
		got := strings.TrimSpace(c.Value)
		if len(got) == len(expected) && subtle.ConstantTimeCompare([]byte(got), []byte(expected)) == 1 {
			return true
		}
	}
	return false
}

func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	required := s.activeAuthToken() != ""
	writeJSON(w, http.StatusOK, map[string]any{
		"auth_required": required,
		"authenticated": !required || s.validAuth(r),
	})
}

func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	expected := s.activeAuthToken()
	if expected == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "auth_required": false})
		return
	}
	got := strings.TrimSpace(req.Token)
	if len(got) != len(expected) || subtle.ConstantTimeCompare([]byte(got), []byte(expected)) != 1 {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid access token"})
		return
	}
	s.issueSessionCookie(w, expected)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "authenticated": true})
}

func (s *Server) issueSessionCookie(w http.ResponseWriter, token string) {
	if strings.TrimSpace(token) == "" {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "soma_session",
		Value:    strings.TrimSpace(token),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   86400 * 7,
	})
}

func (s *Server) handleAuthLogout(w http.ResponseWriter, _ *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     "soma_session",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) validateMutation(r *http.Request) error {
	if sfs := strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")); sfs == "cross-site" {
		return errors.New("cross-site mutation blocked")
	}
	if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || !strings.EqualFold(u.Host, r.Host) {
			return errors.New("origin mismatch on mutation")
		}
	}
	gotCSRF := strings.TrimSpace(r.Header.Get("X-Soma-CSRF"))
	if len(gotCSRF) != len(s.csrf) || subtle.ConstantTimeCompare([]byte(gotCSRF), []byte(s.csrf)) != 1 {
		return errors.New("invalid or missing X-Soma-CSRF token")
	}
	return nil
}

func (s *Server) handleDesktopStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.desktopStatusDTO(r.Context()))
}

func (s *Server) handleDesktopStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Resolution  string `json:"resolution"`
		NetworkMode string `json:"network_mode"`
		Image       string `json:"image"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req)
	}
	mgr := desktop.NewManager()
	_, err := mgr.Start(r.Context(), desktop.Config{
		WorkspaceDir: s.cfg.WorkspaceDir,
		Resolution:   strings.TrimSpace(req.Resolution),
		NetworkMode:  strings.TrimSpace(req.NetworkMode),
		Image:        strings.TrimSpace(req.Image),
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.touchDesktop()
	s.notify()
	writeJSON(w, http.StatusOK, s.desktopStatusDTO(r.Context()))
}

func (s *Server) handleDesktopStop(w http.ResponseWriter, r *http.Request) {
	mgr := desktop.NewManager()
	if err := mgr.Stop(r.Context(), s.cfg.WorkspaceDir); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.mu.Lock()
	s.desktopTakeOver = false
	s.desktopLastActive = time.Time{}
	s.mu.Unlock()
	s.notify()
	writeJSON(w, http.StatusOK, s.desktopStatusDTO(r.Context()))
}

func (s *Server) handleDesktopTakeOver(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Active bool `json:"active"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req)
	}
	s.mu.Lock()
	s.desktopTakeOver = req.Active
	s.desktopLastActive = time.Now()
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "takeover": req.Active})
}

func (s *Server) touchDesktop() {
	s.mu.Lock()
	s.desktopLastActive = time.Now()
	s.mu.Unlock()
}

func (s *Server) maybeAutoStopDesktop(ctx context.Context, idleTimeout time.Duration) {
	if strings.TrimSpace(s.cfg.WorkspaceDir) == "" || idleTimeout <= 0 {
		return
	}
	mgr := desktop.NewManager()
	_, running, err := mgr.Status(ctx, s.cfg.WorkspaceDir)
	if err != nil || !running {
		return
	}
	s.mu.Lock()
	busy := len(s.activeCancels) > 0 || s.desktopTakeOver
	if busy {
		s.desktopLastActive = time.Now()
		s.mu.Unlock()
		return
	}
	last := s.desktopLastActive
	if last.IsZero() {
		s.desktopLastActive = time.Now()
		s.mu.Unlock()
		return
	}
	expired := time.Since(last) >= idleTimeout
	s.mu.Unlock()
	if expired {
		_ = mgr.Stop(ctx, s.cfg.WorkspaceDir)
		s.mu.Lock()
		s.desktopLastActive = time.Time{}
		s.mu.Unlock()
		s.notify()
	}
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	state, err := s.buildState(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan struct{}, 1)
	s.subMu.Lock()
	s.subs[ch] = struct{}{}
	s.subMu.Unlock()
	defer func() {
		s.subMu.Lock()
		delete(s.subs, ch)
		s.subMu.Unlock()
	}()

	_, _ = fmt.Fprint(w, "event: state\ndata: init\n\n")
	flusher.Flush()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.stopCh:
			return
		case <-ch:
			_, _ = fmt.Fprint(w, "event: state\ndata: refresh\n\n")
			flusher.Flush()
		case <-ticker.C:
			_, _ = fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

func (s *Server) notify() {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	for ch := range s.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (s *Server) handleGetPage(w http.ResponseWriter, r *http.Request, rawID string) {
	pageID := strings.TrimPrefix(strings.TrimSpace(rawID), "@")
	if pageID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "page_id required"})
		return
	}
	startLine := 1
	endLine := 120
	if raw := strings.TrimSpace(r.URL.Query().Get("start")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			startLine = n
		}
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("end")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= startLine {
			endLine = n
		}
	}

	pg, err := s.store.GetPage(r.Context(), pageID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	content, err := s.vm.Slice(r.Context(), pageID, startLine, endLine)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"page_id":    pg.ID,
		"content":    content,
		"trust_tier": pg.TrustTier,
		"byte_size":  pg.ByteSize,
		"line_count": pg.LineCount,
		"start_line": startLine,
		"end_line":   endLine,
	})
}

func (s *Server) handleStartRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Goal      string `json:"goal"`
		Title     string `json:"title"`
		Profile   string `json:"profile"`
		AccountID string `json:"account_id"`
		MaxTurns  int    `json:"max_turns"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	goal := strings.TrimSpace(req.Goal)
	if goal == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "goal is required"})
		return
	}
	if req.MaxTurns <= 0 {
		req.MaxTurns = s.defaultMaxTurns()
	}
	runID, branchID, err := s.launchAsyncRunWithAccount(r.Context(), req.Title, goal, req.Profile, req.AccountID, req.MaxTurns)
	if err != nil {
		status := http.StatusInternalServerError
		if strings.Contains(err.Error(), "no model provider") {
			status = http.StatusBadRequest
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{
		"run_id":    runID,
		"branch_id": branchID,
		"status":    "running",
	})
}

type accountRouterProvider struct {
	server   *Server
	fallback dag.ModelProvider
}

func (arp *accountRouterProvider) Generate(ctx context.Context, req dag.GenerateRequest) (*dag.GenerateResponse, error) {
	if req.AccountID != "" {
		if prov := arp.server.providerForAccount(ctx, req.AccountID); prov != nil {
			return prov.Generate(ctx, req)
		}
	}
	if arp.fallback != nil {
		return arp.fallback.Generate(ctx, req)
	}
	return nil, errors.New("no model provider configured for account " + req.AccountID)
}

func (s *Server) defaultMaxTurns() int {
	if mt := s.loadHarnessConfig().Provider.MaxTurns; mt > 0 {
		return mt
	}
	return 15
}

func (s *Server) activeProvider() dag.ModelProvider {
	s.mu.Lock()
	prov := s.cfg.Provider
	s.mu.Unlock()
	return prov
}

func (s *Server) restoreProviderFromHarness(ctx context.Context) {
	cfg := s.loadHarnessConfig()
	accs, _ := s.store.ListAccounts(ctx)
	targetID := strings.TrimSpace(cfg.DefaultAccountID)
	if targetID == "" && len(accs) > 0 {
		targetID = accs[0].ID
	}
	if targetID != "" {
		for _, a := range accs {
			if a.ID == targetID {
				ac := cfg.AccountConfigs[a.ID]
				s.applyAccountToServerLocked(&cfg, a.ID, a.Provider, ac.APIBase, ac.APIKey, a.ModelID)
				if s.activeProvider() != nil {
					return
				}
			}
		}
	}
	for _, a := range accs {
		ac := cfg.AccountConfigs[a.ID]
		s.applyAccountToServerLocked(&cfg, a.ID, a.Provider, ac.APIBase, ac.APIKey, a.ModelID)
		if s.activeProvider() != nil {
			return
		}
	}
	if strings.EqualFold(cfg.Provider.ProviderType, "mock") {
		mName := cfg.Provider.ModelID
		if mName == "" {
			mName = "mock-worker-v1"
		}
		s.mu.Lock()
		s.cfg.UseMock = true
		s.cfg.ModelID = mName
		s.cfg.Provider = &dag.MockProvider{ModelName: mName}
		s.mu.Unlock()
		return
	}
	if base, key, model, ok := dag.ResolveProviderEndpoint(cfg.Provider.ProviderType, cfg.Provider.APIBase, cfg.Provider.APIKey, cfg.Provider.ModelID); ok && base != "" {
		s.mu.Lock()
		s.cfg.UseMock = false
		if s.cfg.ModelID == "" {
			s.cfg.ModelID = model
		}
		s.cfg.Provider = dag.NewOpenAICompatProvider(base, key)
		s.mu.Unlock()
		return
	}
	if strings.TrimSpace(cfg.Provider.APIBase) != "" {
		s.mu.Lock()
		s.cfg.UseMock = false
		if cfg.Provider.ModelID != "" {
			s.cfg.ModelID = cfg.Provider.ModelID
		}
		s.cfg.Provider = dag.NewOpenAICompatProvider(strings.TrimSpace(cfg.Provider.APIBase), cfg.Provider.APIKey)
		s.mu.Unlock()
	}
}

func (s *Server) executorForRequest(ctx context.Context, profile, accountID string) (*dag.Executor, []string, error) {
	prof := compiler.WorkerProfile()
	isFrontier := strings.EqualFold(profile, "frontier")
	if isFrontier {
		prof = compiler.FrontierProfile()
	}
	exec := s.newExecutor(prof)
	cfg := s.loadHarnessConfig()
	accs, _ := s.store.ListAccounts(ctx)

	chosen := strings.TrimSpace(accountID)
	if chosen == "" && len(accs) > 0 {
		// Role-aware automatic model selection: prefer frontier role for frontier profile,
		// worker role for worker profile, then DefaultAccountID.
		wantRole := "worker"
		if isFrontier {
			wantRole = "frontier"
		}
		for _, a := range accs {
			if strings.EqualFold(cfg.AccountConfigs[a.ID].Role, wantRole) {
				chosen = a.ID
				break
			}
		}
		if chosen == "" && cfg.DefaultAccountID != "" {
			for _, a := range accs {
				if a.ID == cfg.DefaultAccountID {
					chosen = a.ID
					break
				}
			}
		}
	}

	var primaryIDs, fallbackIDs []string
	if chosen != "" {
		primaryIDs = append(primaryIDs, chosen)
	}
	for _, a := range accs {
		if a.ID == chosen {
			continue
		}
		if strings.EqualFold(cfg.AccountConfigs[a.ID].Role, "fallback") {
			fallbackIDs = append(fallbackIDs, a.ID)
		} else {
			primaryIDs = append(primaryIDs, a.ID)
		}
	}
	accIDs := append(primaryIDs, fallbackIDs...)

	var primaryProv dag.ModelProvider
	if chosen != "" {
		primaryProv = s.providerForAccount(ctx, chosen)
	}
	if primaryProv == nil {
		primaryProv = exec.Provider
	}
	if primaryProv == nil && len(accIDs) > 0 {
		primaryProv = s.providerForAccount(ctx, accIDs[0])
	}
	if primaryProv == nil {
		return nil, nil, errors.New("no model provider configured (start `soma web --mock` or add a model in Settings)")
	}
	if len(accIDs) > 0 {
		exec.Provider = &accountRouterProvider{server: s, fallback: primaryProv}
	} else {
		exec.Provider = primaryProv
	}
	exec.AccountIDs = accIDs
	return exec, accIDs, nil
}

func (s *Server) providerForAccount(ctx context.Context, accountID string) dag.ModelProvider {
	acc, err := s.store.GetAccount(ctx, accountID)
	if err != nil {
		return nil
	}
	if strings.EqualFold(acc.Provider, "mock") {
		mName := acc.ModelID
		if mName == "" {
			mName = "mock-worker-v1"
		}
		return &dag.MockProvider{ModelName: mName}
	}
	cfg := s.loadHarnessConfig()
	ac := cfg.AccountConfigs[accountID]
	base, key, _, ok := dag.ResolveProviderEndpoint(acc.Provider, ac.APIBase, ac.APIKey, acc.ModelID)
	if !ok {
		base = ac.APIBase
		key = ac.APIKey
	}
	if base == "" {
		return nil
	}
	return dag.NewOpenAICompatProvider(base, key)
}

func (s *Server) runBranchAsync(exec *dag.Executor, branchID string, maxTurns int) {
	if maxTurns <= 0 {
		maxTurns = s.defaultMaxTurns()
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		runCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		s.mu.Lock()
		if prev := s.activeCancels[branchID]; prev != nil {
			prev()
		}
		s.activeCancels[branchID] = cancel
		s.mu.Unlock()
		defer func() {
			cancel()
			s.mu.Lock()
			delete(s.activeCancels, branchID)
			delete(s.activeTools, branchID)
			s.mu.Unlock()
		}()
		go func() {
			select {
			case <-s.stopCh:
				cancel()
			case <-runCtx.Done():
			}
		}()
		_, _ = exec.ExecuteBranch(runCtx, branchID, maxTurns)
		s.mu.Lock()
		delete(s.activeTools, branchID)
		s.mu.Unlock()
		s.notify()
	}()
}

func (s *Server) buildRunRequest(title, goal string, prof compiler.ModelProfile, accIDs []string) dag.RunRequest {
	hCfg := s.loadHarnessConfig()
	return dag.RunRequest{
		Title:             strings.TrimSpace(title),
		WorkspacePath:     s.cfg.WorkspaceDir,
		WorkspaceStrategy: workspace.NormalizeStrategy(hCfg.Provider.WorkspaceStrategy),
		GoalSpec:          goal,
		Profile:           prof,
		AccountIDs:        accIDs,
	}
}

func (s *Server) launchAsyncRun(ctx context.Context, title, goal, profile string, maxTurns int) (string, string, error) {
	return s.launchAsyncRunWithAccount(ctx, title, goal, profile, "", maxTurns)
}

func (s *Server) launchAsyncRunWithAccount(ctx context.Context, title, goal, profile, accountID string, maxTurns int) (string, string, error) {
	exec, accIDs, err := s.executorForRequest(ctx, profile, accountID)
	if err != nil {
		return "", "", err
	}
	prof := compiler.WorkerProfile()
	if strings.EqualFold(profile, "frontier") {
		prof = compiler.FrontierProfile()
	}

	runID, branchID, err := exec.StartRun(ctx, s.buildRunRequest(title, goal, prof, accIDs))
	if err != nil {
		return "", "", err
	}
	s.notify()
	s.runBranchAsync(exec, branchID, maxTurns)
	return runID, branchID, nil
}

func (s *Server) handleRenameRun(w http.ResponseWriter, r *http.Request, runID string) {
	var req struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if err := s.store.UpdateRunTitle(r.Context(), strings.TrimSpace(runID), req.Title); err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, store.ErrNotFound) {
			code = http.StatusNotFound
		}
		writeJSON(w, code, map[string]string{"error": err.Error()})
		return
	}
	s.notify()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDeleteRun(w http.ResponseWriter, r *http.Request, runID string) {
	id := strings.TrimSpace(runID)
	run, err := s.store.GetRun(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
		return
	}
	branches, _ := s.store.ListBranches(r.Context(), id)
	for _, b := range branches {
		s.mu.Lock()
		cancel := s.activeCancels[b.ID]
		delete(s.activeCancels, b.ID)
		delete(s.activeTools, b.ID)
		s.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		if s.wsMgr != nil {
			_ = s.wsMgr.CleanupBranchWorkspace(r.Context(), run, b)
		}
	}
	deleted, err := s.store.DeleteRun(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if !deleted {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
		return
	}
	s.notify()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleBranchDiff(w http.ResponseWriter, r *http.Request, branchID string) {
	br, err := s.store.GetBranch(r.Context(), strings.TrimSpace(branchID))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	run, err := s.store.GetRun(r.Context(), br.RunID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	baseDir := run.WorkspacePath
	if baseDir == "" {
		baseDir = s.cfg.WorkspaceDir
	}
	rep, err := s.wsMgr.DiffBranchWorkspace(r.Context(), baseDir, br.WorkspaceDir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func (s *Server) handleBranchApply(w http.ResponseWriter, r *http.Request, branchID string) {
	br, err := s.store.GetBranch(r.Context(), strings.TrimSpace(branchID))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	run, err := s.store.GetRun(r.Context(), br.RunID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	baseDir := run.WorkspacePath
	if baseDir == "" {
		baseDir = s.cfg.WorkspaceDir
	}
	rep, err := s.wsMgr.ApplyBranchWorkspace(r.Context(), baseDir, br.WorkspaceDir)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, workspace.ErrMergeConflict) {
			status = http.StatusConflict
		}
		writeJSON(w, status, map[string]any{"error": err.Error(), "report": rep})
		return
	}
	s.notify()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "report": rep})
}

func (s *Server) handleResumeBranch(w http.ResponseWriter, r *http.Request, branchID string) {
	br, err := s.store.GetBranch(r.Context(), strings.TrimSpace(branchID))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	var req struct {
		MaxTurns  int    `json:"max_turns"`
		Profile   string `json:"profile"`
		AccountID string `json:"account_id"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req)
	}
	accID := strings.TrimSpace(req.AccountID)
	if accID == "" {
		accID = br.ActiveAccountID
	}
	exec, _, err := s.executorForRequest(r.Context(), req.Profile, accID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	_ = s.store.UpdateBranchStatus(r.Context(), br.ID, string(dag.StatusRunning), accID, br.StallCount)
	s.notify()
	s.runBranchAsync(exec, br.ID, req.MaxTurns)

	writeJSON(w, http.StatusAccepted, map[string]string{
		"branch_id": br.ID,
		"status":    "running",
	})
}

func (s *Server) handleContinueBranch(w http.ResponseWriter, r *http.Request, branchID string) {
	br, err := s.store.GetBranch(r.Context(), strings.TrimSpace(branchID))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	var req struct {
		Goal      string `json:"goal"`
		Profile   string `json:"profile"`
		AccountID string `json:"account_id"`
		MaxTurns  int    `json:"max_turns"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	msg := strings.TrimSpace(req.Goal)
	if msg == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "follow-up message is required"})
		return
	}
	accID := strings.TrimSpace(req.AccountID)
	if accID == "" {
		accID = br.ActiveAccountID
	}
	exec, _, err := s.executorForRequest(r.Context(), req.Profile, accID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := exec.AppendFollowUpUserTurn(r.Context(), br.ID, msg); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	_ = s.store.UpdateBranchStatus(r.Context(), br.ID, string(dag.StatusRunning), accID, 0)
	s.notify()
	s.runBranchAsync(exec, br.ID, req.MaxTurns)

	writeJSON(w, http.StatusAccepted, map[string]string{
		"run_id":    br.RunID,
		"branch_id": br.ID,
		"status":    "running",
	})
}

func (s *Server) handleStopBranch(w http.ResponseWriter, r *http.Request, branchID string) {
	id := strings.TrimSpace(branchID)
	br, err := s.store.GetBranch(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	s.mu.Lock()
	cancel := s.activeCancels[id]
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	_ = s.store.UpdateBranchStatus(r.Context(), id, "stopped", br.ActiveAccountID, br.StallCount)
	_ = s.store.UpdateRunStatus(r.Context(), br.RunID, "stopped")
	s.notify()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "branch_id": id, "status": "stopped"})
}

func (s *Server) handleForkBranch(w http.ResponseWriter, r *http.Request, branchID string) {
	br, err := s.store.GetBranch(r.Context(), strings.TrimSpace(branchID))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	var req struct {
		StepID string `json:"step_id"`
		Label  string `json:"label"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req)
	}
	stepID := strings.TrimSpace(req.StepID)
	if stepID == "" {
		stepID = br.HeadStepID
	}
	if stepID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "branch has no steps to fork from"})
		return
	}
	label := strings.TrimSpace(req.Label)
	if label == "" {
		existingBrs, _ := s.store.ListBranches(r.Context(), br.RunID)
		label = fmt.Sprintf("branch-%d", len(existingBrs)+1)
	}

	exec := dag.NewExecutor(s.store, s.activeProvider(), nil, compiler.WorkerProfile())
	newID, err := exec.ForkBranch(r.Context(), br.RunID, stepID, label)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.notify()
	writeJSON(w, http.StatusCreated, map[string]string{
		"branch_id": newID,
		"run_id":    br.RunID,
	})
}

func (s *Server) handleReplayBranch(w http.ResponseWriter, r *http.Request, branchID string) {
	rep, err := replay.NewReplayer(s.store).Replay(r.Context(), strings.TrimSpace(branchID), replay.ReplayOptions{Mode: replay.ModeExact})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"branch_id":      strings.TrimSpace(branchID),
		"verified_count": rep.HashVerifiedCount,
		"steps_checked":  rep.StepsChecked,
		"diverged":       rep.Diverged,
	})
}

func (s *Server) handleInboxAction(w http.ResponseWriter, r *http.Request, itemID string, approve bool) {
	itemID = strings.TrimSpace(itemID)
	priorItem, _ := s.store.GetInboxItem(r.Context(), itemID)
	var out string
	var err error
	if approve {
		out, err = s.inbox.Approve(r.Context(), itemID, "webui")
	} else {
		out, err = s.inbox.Dismiss(r.Context(), itemID, "webui")
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	resumed := false
	if approve && (priorItem.ItemKind == "approval" || priorItem.ItemKind == "reconcile") {
		targetBranch := priorItem.BranchID
		if targetBranch == "" && priorItem.ArtifactRef != "" {
			if act, aErr := s.store.GetAction(r.Context(), priorItem.ArtifactRef); aErr == nil {
				targetBranch = act.BranchID
			}
		}
		if targetBranch != "" {
			if br, bErr := s.store.GetBranch(r.Context(), targetBranch); bErr == nil && br.Status == string(dag.StatusQueued) {
				if exec, _, eErr := s.executorForRequest(r.Context(), "worker", br.ActiveAccountID); eErr == nil {
					_ = s.store.UpdateBranchStatus(r.Context(), br.ID, string(dag.StatusRunning), br.ActiveAccountID, br.StallCount)
					s.runBranchAsync(exec, br.ID, s.defaultMaxTurns())
					resumed = true
				}
			}
		}
	}
	s.notify()
	writeJSON(w, http.StatusOK, map[string]any{"status": out, "resumed": resumed})
}

func (s *Server) handleMemoryToggle(w http.ResponseWriter, r *http.Request, nodeID string) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if err := s.store.SetMemoryEnabled(r.Context(), strings.TrimSpace(nodeID), req.Enabled); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.notify()
	writeJSON(w, http.StatusOK, map[string]any{"id": strings.TrimSpace(nodeID), "enabled": req.Enabled})
}

func (s *Server) handleMemoryActivate(w http.ResponseWriter, r *http.Request, nodeID string) {
	if err := memory.NewService(s.store).Activate(r.Context(), strings.TrimSpace(nodeID)); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.notify()
	writeJSON(w, http.StatusOK, map[string]string{"id": strings.TrimSpace(nodeID), "status": "active"})
}

func (s *Server) newExecutor(prof compiler.ModelProfile) *dag.Executor {
	s.mu.Lock()
	prov := s.cfg.Provider
	modelID := s.cfg.ModelID
	s.mu.Unlock()

	hCfg := s.loadHarnessConfig()
	sbx := sandbox.NewManagerForMode(hCfg.Provider.SandboxMode)
	vm := vmem.NewManager(s.store)
	tools := dag.NewSandboxedToolRunner(vm, sbx)
	tools.AllowSandboxNetwork = hCfg.Provider.AllowSandboxNetwork
	if mcpMgr := s.connectConfiguredMCP(context.Background()); mcpMgr != nil {
		tools.MCPManager = mcpMgr
	}
	exec := dag.NewExecutor(s.store, prov, tools, prof)
	exec.VM = vm
	exec.Verifier = oracle.NewVerifier(s.store, sbx)
	exec.AllowFailover = true
	exec.AutonomyMode = hCfg.Provider.AutonomyMode

	// Wire configured vector embedder for hybrid memory & @page search.
	embedURL := strings.TrimSpace(hCfg.Provider.EmbedURL)
	embedModel := strings.TrimSpace(hCfg.Provider.EmbedModel)
	if embedURL != "" || embedModel != "" {
		if embedURL == "" {
			embedURL = "http://127.0.0.1:11434/v1"
		}
		if embedModel == "" {
			embedModel = "qwen3-embedding:0.6b"
		}
		embedKey := strings.TrimSpace(os.Getenv("SOMA_EMBED_KEY"))
		if embedKey == "" {
			embedKey = hCfg.Provider.APIKey
		}
		exec.SetEmbedder(somaembed.NewOpenAICompatEmbedder(embedURL, embedKey, embedModel, somaembed.DefaultMatryoshkaDims))
	}

	// Stream live tool dispatch and step completion events over SSE.
	exec.AfterDispatchHook = func(_ context.Context, entry store.ActionJournalEntry) error {
		label := friendlyToolTitle(entry.ToolName, entry.ArgsJSON, "")
		s.mu.Lock()
		s.activeTools[entry.BranchID] = label
		s.desktopLastActive = time.Now()
		s.mu.Unlock()
		s.notify()
		return nil
	}
	exec.OnStep = func(st store.Step) {
		s.mu.Lock()
		delete(s.activeTools, st.BranchID)
		s.desktopLastActive = time.Now()
		s.mu.Unlock()
		s.notify()
	}

	guiProv := prov
	guiModel := modelID
	if accs, aErr := s.store.ListAccounts(context.Background()); aErr == nil {
		for _, a := range accs {
			if strings.EqualFold(hCfg.AccountConfigs[a.ID].Role, "gui") {
				if p := s.providerForAccount(context.Background(), a.ID); p != nil {
					guiProv = p
					guiModel = a.ModelID
					break
				}
			}
		}
	}
	exec.GUIConfig = &dag.GUISubagentConfig{
		Provider:   guiProv,
		ModelID:    guiModel,
		MCPManager: tools.MCPManager,
		EnsureReady: func(ctx context.Context, mode string) (*mcp.Manager, error) {
			if mode == "desktop" && strings.TrimSpace(s.cfg.WorkspaceDir) != "" {
				mgr := desktop.NewManager()
				_, running, err := mgr.Status(ctx, s.cfg.WorkspaceDir)
				if err != nil || !running {
					if _, startErr := mgr.Start(ctx, desktop.Config{
						WorkspaceDir: s.cfg.WorkspaceDir,
						NetworkMode:  "bridge",
					}); startErr != nil {
						return nil, startErr
					}
				}
				s.touchDesktop()
				s.notify()
				if connected := s.connectConfiguredMCP(ctx); connected != nil {
					return connected, nil
				}
			}
			if tools.MCPManager != nil && len(tools.MCPManager.ListServers()) > 0 {
				return tools.MCPManager, nil
			}
			return s.connectConfiguredMCP(ctx), nil
		},
	}
	return exec
}

func (s *Server) buildState(ctx context.Context) (StateDTO, error) {
	hCfg := s.loadHarnessConfig()
	sbx := sandbox.NewManagerForMode(hCfg.Provider.SandboxMode)
	sbxName := "unavailable"
	sbxReady := false
	if b := sbx.ActiveBackend(); b != nil {
		sbxName = b.Name()
		sbxReady = true
	}
	s.mu.Lock()
	useMock := s.cfg.UseMock
	hasProv := s.cfg.Provider != nil
	curModelID := s.cfg.ModelID
	activeToolsSnap := make(map[string]string, len(s.activeTools))
	for k, v := range s.activeTools {
		activeToolsSnap[k] = v
	}
	s.mu.Unlock()

	provMode := "read-only"
	if useMock {
		provMode = "mock"
	} else if hasProv {
		provMode = "live"
	}

	out := StateDTO{
		WorkspaceDir: s.cfg.WorkspaceDir,
		Version:      s.cfg.Version,
		SandboxName:  sbxName,
		SandboxReady: sbxReady,
		ProviderMode: provMode,
		ModelID:      curModelID,
		LoadedAt:     time.Now().Unix(),
		Runs:         []RunDTO{},
		Inbox:        []InboxDTO{},
		Memory:       []MemoryDTO{},
		Skills:       []SkillDTO{},
		Accounts:     []AccountDTO{},
	}

	runs, err := s.store.ListRuns(ctx)
	if err != nil {
		return out, err
	}
	branches, err := s.store.ListBranches(ctx, "")
	if err != nil {
		return out, err
	}
	brByRun := make(map[string][]store.Branch)
	for _, b := range branches {
		brByRun[b.RunID] = append(brByRun[b.RunID], b)
	}

	runTitles := make(map[string]string, len(runs))
	for _, r := range runs {
		ctrs, _ := s.store.ListRunContracts(ctx, r.ID)
		pages, _ := s.store.ListRunPages(ctx, r.ID, 30)
		pageDTOs := make([]PageDTO, 0, len(pages))
		for _, p := range pages {
			pageDTOs = append(pageDTOs, PageDTO{
				ID:          p.ID,
				ContentType: p.ContentType,
				TrustTier:   p.TrustTier,
				ByteSize:    p.ByteSize,
				LineCount:   p.LineCount,
				Preview:     p.PreviewText,
			})
		}

		baseWS := r.WorkspacePath
		if baseWS == "" {
			baseWS = s.cfg.WorkspaceDir
		}

		brList := brByRun[r.ID]
		brDTOs := make([]BranchDTO, 0, len(brList))
		for _, b := range brList {
			steps, _ := s.store.ListBranchSteps(ctx, b.ID)
			cruns, _ := s.store.ListBranchContractRuns(ctx, b.ID)
			passed := 0
			for _, cr := range cruns {
				if cr.Passed {
					passed++
				}
			}
			var inToks, cacheToks int
			stepDTOs := make([]StepDTO, 0, len(steps))
			for _, st := range steps {
				inToks += st.InputTokens + st.CacheReadTokens
				cacheToks += st.CacheReadTokens
				stepDTOs = append(stepDTOs, s.decodeStepDTO(st))
			}
			hitPct := 0.0
			if inToks > 0 {
				hitPct = 100.0 * float64(cacheToks) / float64(inToks)
			}
			var ft FrameTokensDTO
			if b.HeadStepID != "" {
				if frame, fErr := compiler.ReconstructFrame(ctx, s.store, b.HeadStepID, compiler.WorkerProfile()); fErr == nil {
					ft = FrameTokensDTO{
						L0:    frame.L0Tokens,
						L1:    frame.L1Tokens,
						L2:    frame.L2Tokens,
						L3:    frame.L3Tokens,
						Total: frame.TotalTokens,
					}
				}
			}
			changedCount := 0
			if s.wsMgr != nil && b.WorkspaceDir != "" && b.WorkspaceStrategy != workspace.StrategyDirect && len(steps) > 0 {
				changedCount = s.wsMgr.CountWorkspaceChanges(ctx, baseWS, b.WorkspaceDir)
			}
			brDTOs = append(brDTOs, BranchDTO{
				ID:                b.ID,
				RunID:             b.RunID,
				ParentBranchID:    b.ParentBranchID,
				Label:             b.Label,
				Status:            b.Status,
				ActiveTool:        activeToolsSnap[b.ID],
				ChangedFilesCount: changedCount,
				Epoch:             b.Epoch,
				WorkspaceDir:      b.WorkspaceDir,
				WorkspaceStrategy: b.WorkspaceStrategy,
				AccountID:         b.ActiveAccountID,
				StallCount:        b.StallCount,
				ContractsPassed:   passed,
				ContractsTotal:    len(ctrs),
				PromptTokens:      inToks,
				CachedTokens:      cacheToks,
				CacheHitPct:       hitPct,
				FrameTokens:       ft,
				Steps:             stepDTOs,
			})
		}

		title := strings.TrimSpace(r.Title)
		if title == "" {
			title = firstLine(r.GoalSpec, 64)
		}
		runTitles[r.ID] = title
		out.Runs = append(out.Runs, RunDTO{
			ID:             r.ID,
			Title:          title,
			Goal:           r.GoalSpec,
			Status:         r.Status,
			CreatedAt:      r.CreatedAt,
			UpdatedAt:      r.UpdatedAt,
			ContractsCount: len(ctrs),
			Branches:       brDTOs,
			Pages:          pageDTOs,
		})
	}

	inboxItems, _ := s.store.ListInboxItems(ctx, "")
	for _, it := range inboxItems {
		if it.Status == "pending" {
			out.PendingApprovals++
		}
		dto := InboxDTO{
			ID:          it.ID,
			RunID:       it.RunID,
			RunTitle:    runTitles[it.RunID],
			BranchID:    it.BranchID,
			ItemKind:    it.ItemKind,
			Title:       it.Title,
			Summary:     it.Summary,
			ArtifactRef: it.ArtifactRef,
			Urgency:     it.Urgency,
			Status:      it.Status,
			CreatedAt:   it.CreatedAt,
		}
		if it.ArtifactRef != "" {
			switch it.ItemKind {
			case "approval", "reconcile":
				if act, aErr := s.store.GetAction(ctx, it.ArtifactRef); aErr == nil {
					dto.ActionTool = act.ToolName
					dto.ActionArgs = act.ArgsJSON
					if dto.RunID == "" {
						dto.RunID = act.RunID
						dto.RunTitle = runTitles[act.RunID]
					}
					if dto.BranchID == "" {
						dto.BranchID = act.BranchID
					}
				}
			case "memory_proposal":
				if mn, mErr := s.store.GetMemoryNode(ctx, it.ArtifactRef); mErr == nil {
					dto.ProposedContent = mn.Content
				}
			}
		}
		out.Inbox = append(out.Inbox, dto)
	}

	memNodes, _ := s.store.ListAllMemoryNodes(ctx, "")
	replacedBy := make(map[string]string)
	for _, m := range memNodes {
		edges, _ := s.store.ListMemoryEdges(ctx, m.ID)
		for _, e := range edges {
			if e.Relation == "SUPERSEDES" && e.FromNodeID == m.ID {
				replacedBy[m.ID] = e.ToNodeID
			}
		}
	}
	for _, m := range memNodes {
		out.Memory = append(out.Memory, MemoryDTO{
			ID:         m.ID,
			Scope:      m.Scope,
			Kind:       m.Kind,
			Content:    m.Content,
			Status:     m.Status,
			TrustTier:  m.TrustTier,
			Enabled:    m.Enabled,
			ReplacesID: replacedBy[m.ID],
			IngestedAt: m.IngestedAt,
		})
	}

	if roots := s.skillRoots(); len(roots) > 0 {
		out.Skills = loadWebSkillStatuses(roots)
	}

	accs, _ := s.store.ListAccounts(ctx)
	nowMs := time.Now().UnixMilli()
	for _, a := range accs {
		var cool int64
		if a.CooldownUntil > nowMs {
			cool = (a.CooldownUntil - nowMs) / 1000
		}
		out.Accounts = append(out.Accounts, AccountDTO{
			ID:             a.ID,
			Provider:       a.Provider,
			ModelID:        a.ModelID,
			BillingMode:    a.BillingMode,
			MaxConcurrent:  a.MaxConcurrent,
			ActiveLeases:   a.ActiveLeases,
			CooldownSec:    cool,
			DailyBudgetUSD: a.DailyBudgetUSD,
			DailySpentUSD:  a.DailySpentUSD,
		})
	}

	return out, nil
}

func (s *Server) decodeStepDTO(st store.Step) StepDTO {
	stepType := ""
	if st.CompiledFrameHash == "user_turn" {
		stepType = "user_message"
	}
	dto := StepDTO{
		ID:                st.ID,
		StepType:          stepType,
		TurnIndex:         st.TurnIndex,
		Epoch:             st.Epoch,
		ModelID:           st.ModelID,
		AccountID:         st.AccountID,
		LatencyMS:         st.LatencyMS,
		CreatedAt:         st.CreatedAt,
		Title:             fmt.Sprintf("Step %d", st.TurnIndex),
		ObservationPageID: st.ObservationPageID,
	}
	if st.EmittedBlobHash == "" {
		return dto
	}
	raw, err := s.store.Blobs().Get(st.EmittedBlobHash)
	if err != nil {
		return dto
	}
	var ep dag.EmittedPayload
	if json.Unmarshal(raw, &ep) != nil {
		return dto
	}
	dto.Reasoning = strings.TrimSpace(ep.Response.Reasoning)
	dto.Done = ep.Response.Done
	dto.FinalSummary = strings.TrimSpace(ep.Response.FinalSummary)

	for _, le := range ep.ResultingLogEntries {
		if le.Kind != "turn" {
			continue
		}
		if le.Action == "done" {
			continue
		}
		dto.ToolCalls = append(dto.ToolCalls, ToolStepDTO{
			Name:        le.ToolName,
			Label:       friendlyToolTitle(le.ToolName, le.ArgsJSON, le.Action),
			ArgsJSON:    le.ArgsJSON,
			Observation: le.Observation,
			PageID:      le.PageID,
		})
	}
	if len(dto.ToolCalls) > 0 {
		if len(dto.ToolCalls) == 1 {
			dto.Title = fmt.Sprintf("Step %d · %s", st.TurnIndex, dto.ToolCalls[0].Label)
		} else {
			dto.Title = fmt.Sprintf("Step %d · %s (+%d more)", st.TurnIndex, dto.ToolCalls[0].Label, len(dto.ToolCalls)-1)
		}
	} else if dto.Done {
		dto.Title = fmt.Sprintf("Step %d · Finished task", st.TurnIndex)
	}
	return dto
}

func friendlyToolTitle(name, argsJSON, action string) string {
	var args map[string]any
	if argsJSON != "" {
		_ = json.Unmarshal([]byte(argsJSON), &args)
	}
	strArg := func(k string) string {
		v, _ := args[k].(string)
		return strings.TrimSpace(v)
	}
	switch name {
	case "fs_read":
		if p := strArg("path"); p != "" {
			return "Read " + p
		}
	case "fs_write":
		if p := strArg("path"); p != "" {
			return "Wrote " + p
		}
	case "fs_patch":
		if p := strArg("path"); p != "" {
			return "Patched " + p
		}
	case "fs_list":
		return "Listed workspace files"
	case "exec", "bg_exec":
		if c := strArg("command"); c != "" {
			return "Ran " + firstLine(c, 42)
		}
	case "page_grep":
		return "Searched spilled log"
	case "page_slice", "page_search":
		return "Inspected spilled output"
	case "web_search":
		if q := strArg("query"); q != "" {
			return "Searched web: " + firstLine(q, 36)
		}
	case "web_extract":
		if u := strArg("url"); u != "" {
			return "Fetched " + firstLine(u, 42)
		}
	case "vision_analyze":
		if p := strArg("image_path"); p != "" {
			return "Inspected image " + p
		}
	case "memory_search":
		return "Searched memory"
	}
	if name != "" {
		return name
	}
	if action != "" {
		return firstLine(action, 48)
	}
	return "Action"
}

func (s *Server) skillRoots() []string {
	if s.cfg.WorkspaceDir == "" {
		return nil
	}
	return []string{
		filepath.Join(s.cfg.WorkspaceDir, ".soma", "skills"),
		filepath.Join(s.cfg.WorkspaceDir, ".agents", "skills"),
		filepath.Join(s.cfg.WorkspaceDir, ".skills"),
	}
}

func (s *Server) handleCreateMemory(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Scope   string `json:"scope"`
		Kind    string `json:"kind"`
		Content string `json:"content"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	content := strings.TrimSpace(req.Content)
	if content == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "memory content is required"})
		return
	}
	scope := strings.TrimSpace(req.Scope)
	if scope == "" {
		scope = "global"
	}
	kind := strings.TrimSpace(req.Kind)
	if kind == "" {
		kind = "rule"
	}
	svc := memory.NewService(s.store)
	node, err := svc.Propose(r.Context(), memory.ProposeRequest{
		Scope:     scope,
		Kind:      kind,
		Content:   content,
		TrustTier: "trusted_user",
		Status:    "active",
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.notify()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "node": node})
}

func (s *Server) handleGetSkill(w http.ResponseWriter, _ *http.Request, name string) {
	name = strings.TrimSpace(name)
	if name == "" || strings.Contains(name, "/") || strings.Contains(name, "..") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid skill name"})
		return
	}
	for _, root := range s.skillRoots() {
		dir := filepath.Join(root, name)
		body, findings, err := skills.ActivateBody(dir)
		if err == nil {
			rawBytes, _ := os.ReadFile(filepath.Join(dir, "SKILL.md"))
			writeJSON(w, http.StatusOK, map[string]any{
				"ok":            true,
				"skill":         body,
				"raw_content":   string(rawBytes),
				"lint_findings": findings,
				"path":          dir,
			})
			return
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("skill %q not found", name)})
}

func (s *Server) handleCreateSkill(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name    string `json:"name"`
		Content string `json:"content"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if s.cfg.WorkspaceDir == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "workspace_dir is not configured"})
		return
	}
	targetRoot := filepath.Join(s.cfg.WorkspaceDir, ".soma", "skills")
	cleanName := strings.TrimSpace(req.Name)
	if cleanName != "" && !strings.Contains(cleanName, "/") && !strings.Contains(cleanName, "..") {
		for _, root := range s.skillRoots() {
			if st, statErr := os.Stat(filepath.Join(root, cleanName, "SKILL.md")); statErr == nil && !st.IsDir() {
				targetRoot = root
				break
			}
		}
	}
	sk, findings, err := skills.CreateOrPatchSkill(targetRoot, req.Name, req.Content)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.notify()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"name":          sk.Frontmatter.Name,
		"lint_findings": findings,
	})
}

func (s *Server) handleMemoryUpdate(w http.ResponseWriter, r *http.Request, nodeID string) {
	var req struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	content := strings.TrimSpace(req.Content)
	if content == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "content is required"})
		return
	}
	if err := s.store.UpdateMemoryContent(r.Context(), strings.TrimSpace(nodeID), content); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	s.notify()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleMemoryDelete(w http.ResponseWriter, r *http.Request, nodeID string) {
	ok, err := s.store.DeleteMemoryNode(r.Context(), strings.TrimSpace(nodeID))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "memory node not found"})
		return
	}
	s.notify()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDeleteSkill(w http.ResponseWriter, _ *http.Request, name string) {
	name = strings.TrimSpace(name)
	if name == "" || strings.Contains(name, "/") || strings.Contains(name, "..") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid skill name"})
		return
	}
	if s.cfg.WorkspaceDir == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "workspace_dir is not configured"})
		return
	}
	removed := false
	for _, root := range s.skillRoots() {
		dir := filepath.Join(root, name)
		if _, err := os.Stat(dir); err == nil {
			if rErr := os.RemoveAll(dir); rErr != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": rErr.Error()})
				return
			}
			removed = true
			break
		}
	}
	if !removed {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("skill %q not found", name)})
		return
	}
	s.notify()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func loadWebSkillStatuses(roots []string) []SkillDTO {
	seen := make(map[string]SkillDTO)
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, ent := range entries {
			if !ent.IsDir() {
				continue
			}
			dir := filepath.Join(root, ent.Name())
			sk, err := skills.ParseSkillMD(dir)
			if err != nil {
				continue
			}
			findings, _ := skills.LintSkillDir(dir)
			crit := 0
			for _, f := range findings {
				if f.Severity == "critical" {
					crit++
				}
			}
			badge := "CLEAN"
			if crit > 0 {
				badge = "BLOCKED"
			} else if len(findings) > 0 {
				badge = "WARN"
			}
			seen[sk.Frontmatter.Name] = SkillDTO{
				Name:          sk.Frontmatter.Name,
				Description:   sk.Frontmatter.Description,
				Badge:         badge,
				LintCount:     len(findings),
				CriticalCount: crit,
			}
		}
	}
	out := make([]SkillDTO, 0, len(seen))
	for _, s := range seen {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host = strings.TrimSpace(host)
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func firstLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if idx := strings.IndexAny(s, "\r\n"); idx != -1 {
		s = s[:idx]
	}
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
