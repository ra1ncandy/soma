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
	"github.com/ra1ncandy/soma/internal/inbox"
	"github.com/ra1ncandy/soma/internal/memory"
	"github.com/ra1ncandy/soma/internal/oracle"
	"github.com/ra1ncandy/soma/internal/replay"
	"github.com/ra1ncandy/soma/internal/sandbox"
	"github.com/ra1ncandy/soma/internal/skills"
	"github.com/ra1ncandy/soma/internal/store"
	"github.com/ra1ncandy/soma/internal/vmem"
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
	store  *store.Store
	cfg    ServerConfig
	inbox  *inbox.Service
	vm     *vmem.Manager
	csrf   string
	static fs.FS

	mu       sync.Mutex
	listener net.Listener
	server   *http.Server
	wg       sync.WaitGroup

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
	ID          string `json:"id"`
	RunID       string `json:"run_id,omitempty"`
	BranchID    string `json:"branch_id,omitempty"`
	ItemKind    string `json:"item_kind"`
	Title       string `json:"title"`
	Summary     string `json:"summary"`
	ArtifactRef string `json:"artifact_ref,omitempty"`
	Urgency     string `json:"urgency"`
	Status      string `json:"status"`
	CreatedAt   int64  `json:"created_at"`
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

	return &Server{
		store:  st,
		cfg:    cfg,
		inbox:  inbox.NewService(st),
		vm:     vmem.NewManager(st),
		csrf:   hex.EncodeToString(csrfBytes),
		static: subFS,
		stopCh: make(chan struct{}),
		subs:   make(map[chan struct{}]struct{}),
	}, nil
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
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; img-src 'self' data:; connect-src 'self'")

		path := r.URL.Path
		if r.Method == http.MethodGet && (path == "/" || path == "/index.html") {
			s.serveIndex(w)
			return
		}
		if r.Method == http.MethodGet && strings.HasPrefix(path, "/static/") {
			http.StripPrefix("/static/", staticSrv).ServeHTTP(w, r)
			return
		}

		if strings.HasPrefix(path, "/api/") {
			if s.cfg.AuthToken != "" && !s.validBearer(r) {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
				return
			}
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				if err := s.validateMutation(r); err != nil {
					writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
					return
				}
			}
		}

		switch {
		case r.Method == http.MethodGet && path == "/api/state":
			s.handleState(w, r)
		case r.Method == http.MethodGet && path == "/api/events":
			s.handleEvents(w, r)
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/pages/"):
			pageID := strings.TrimPrefix(path, "/api/pages/")
			s.handleGetPage(w, r, pageID)
		case r.Method == http.MethodPost && path == "/api/runs":
			s.handleStartRun(w, r)
		case r.Method == http.MethodPost && strings.HasPrefix(path, "/api/branches/") && strings.HasSuffix(path, "/resume"):
			brID := strings.TrimSuffix(strings.TrimPrefix(path, "/api/branches/"), "/resume")
			s.handleResumeBranch(w, r, brID)
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
		case r.Method == http.MethodPost && strings.HasPrefix(path, "/api/memory/") && strings.HasSuffix(path, "/toggle"):
			nodeID := strings.TrimSuffix(strings.TrimPrefix(path, "/api/memory/"), "/toggle")
			s.handleMemoryToggle(w, r, nodeID)
		case r.Method == http.MethodPost && strings.HasPrefix(path, "/api/memory/") && strings.HasSuffix(path, "/activate"):
			nodeID := strings.TrimSuffix(strings.TrimPrefix(path, "/api/memory/"), "/activate")
			s.handleMemoryActivate(w, r, nodeID)
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

func (s *Server) validBearer(r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return false
	}
	got := strings.TrimPrefix(auth, "Bearer ")
	if len(got) != len(s.cfg.AuthToken) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.AuthToken)) == 1
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
	if s.cfg.Provider == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "no model provider configured (start `soma web --mock` or set SOMA_API_BASE)",
		})
		return
	}
	var req struct {
		Goal     string `json:"goal"`
		Title    string `json:"title"`
		Profile  string `json:"profile"`
		MaxTurns int    `json:"max_turns"`
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
		req.MaxTurns = 15
	}
	prof := compiler.WorkerProfile()
	if strings.EqualFold(req.Profile, "frontier") {
		prof = compiler.FrontierProfile()
	}

	exec := s.newExecutor(prof)
	accs, _ := s.store.ListAccounts(r.Context())
	var accIDs []string
	for _, a := range accs {
		accIDs = append(accIDs, a.ID)
	}
	exec.AccountIDs = accIDs

	runID, branchID, err := exec.StartRun(r.Context(), dag.RunRequest{
		Title:         strings.TrimSpace(req.Title),
		WorkspacePath: s.cfg.WorkspaceDir,
		GoalSpec:      goal,
		Profile:       prof,
		AccountIDs:    accIDs,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.notify()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		runCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		go func() {
			select {
			case <-s.stopCh:
				cancel()
			case <-runCtx.Done():
			}
		}()
		_, _ = exec.ExecuteBranch(runCtx, branchID, req.MaxTurns)
		s.notify()
	}()

	writeJSON(w, http.StatusAccepted, map[string]string{
		"run_id":    runID,
		"branch_id": branchID,
		"status":    "running",
	})
}

func (s *Server) handleResumeBranch(w http.ResponseWriter, r *http.Request, branchID string) {
	if s.cfg.Provider == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "no model provider configured (start `soma web --mock` or set SOMA_API_BASE)",
		})
		return
	}
	br, err := s.store.GetBranch(r.Context(), strings.TrimSpace(branchID))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	var req struct {
		MaxTurns int `json:"max_turns"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req)
	}
	if req.MaxTurns <= 0 {
		req.MaxTurns = 15
	}

	exec := s.newExecutor(compiler.WorkerProfile())
	if br.ActiveAccountID != "" {
		exec.AccountIDs = []string{br.ActiveAccountID}
	}
	_ = s.store.UpdateBranchStatus(r.Context(), br.ID, string(dag.StatusRunning), br.ActiveAccountID, br.StallCount)
	s.notify()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		runCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		go func() {
			select {
			case <-s.stopCh:
				cancel()
			case <-runCtx.Done():
			}
		}()
		_, _ = exec.ExecuteBranch(runCtx, br.ID, req.MaxTurns)
		s.notify()
	}()

	writeJSON(w, http.StatusAccepted, map[string]string{
		"branch_id": br.ID,
		"status":    "running",
	})
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
		label = fmt.Sprintf("fork-%d", time.Now().Unix()%1000)
	}

	exec := dag.NewExecutor(s.store, s.cfg.Provider, nil, compiler.WorkerProfile())
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
	s.notify()
	writeJSON(w, http.StatusOK, map[string]string{"status": out})
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
	sbx := sandbox.NewManager()
	vm := vmem.NewManager(s.store)
	tools := dag.NewSandboxedToolRunner(vm, sbx)
	exec := dag.NewExecutor(s.store, s.cfg.Provider, tools, prof)
	exec.VM = vm
	exec.Verifier = oracle.NewVerifier(s.store, sbx)
	exec.AllowFailover = true
	return exec
}

func (s *Server) buildState(ctx context.Context) (StateDTO, error) {
	sbx := sandbox.NewManager()
	sbxName := "unavailable"
	sbxReady := false
	if b := sbx.ActiveBackend(); b != nil {
		sbxName = b.Name()
		sbxReady = true
	}
	provMode := "read-only"
	if s.cfg.UseMock {
		provMode = "mock"
	} else if s.cfg.Provider != nil {
		provMode = "live"
	}

	out := StateDTO{
		WorkspaceDir: s.cfg.WorkspaceDir,
		Version:      s.cfg.Version,
		SandboxName:  sbxName,
		SandboxReady: sbxReady,
		ProviderMode: provMode,
		ModelID:      s.cfg.ModelID,
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
			brDTOs = append(brDTOs, BranchDTO{
				ID:                b.ID,
				RunID:             b.RunID,
				ParentBranchID:    b.ParentBranchID,
				Label:             b.Label,
				Status:            b.Status,
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
		out.Inbox = append(out.Inbox, InboxDTO{
			ID:          it.ID,
			RunID:       it.RunID,
			BranchID:    it.BranchID,
			ItemKind:    it.ItemKind,
			Title:       it.Title,
			Summary:     it.Summary,
			ArtifactRef: it.ArtifactRef,
			Urgency:     it.Urgency,
			Status:      it.Status,
			CreatedAt:   it.CreatedAt,
		})
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

	if s.cfg.WorkspaceDir != "" {
		out.Skills = loadWebSkillStatuses([]string{
			filepath.Join(s.cfg.WorkspaceDir, ".soma", "skills"),
			filepath.Join(s.cfg.WorkspaceDir, ".skills"),
		})
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
	dto := StepDTO{
		ID:                st.ID,
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
