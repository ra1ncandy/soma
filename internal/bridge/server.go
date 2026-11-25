package bridge

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ra1ncandy/soma/internal/compiler"
	"github.com/ra1ncandy/soma/internal/dag"
	"github.com/ra1ncandy/soma/internal/inbox"
	"github.com/ra1ncandy/soma/internal/store"
)

// RunStarter starts (and optionally executes) a soma run in response to a bridge RPC call.
type RunStarter func(ctx context.Context, req StartRunRequest) (StartRunResponse, error)

// ServerConfig configures the authenticated Unix Domain Socket kernel bridge server.
type ServerConfig struct {
	SomaDir      string
	WorkspaceDir string
	SocketPath   string
	TokenPath    string
	Version      string
	Provider     dag.ModelProvider
	RunStarter   RunStarter
	// BranchResumer continues a queued branch after an inbox approval. Defaults to executing it
	// with the server's own provider.
	BranchResumer func(ctx context.Context, branchID string) error
}

// KernelServer exposes narrow authenticated JSON endpoints over a Unix Domain Socket (0600).
type KernelServer struct {
	store *store.Store
	cfg   ServerConfig
	token string
	inbox *inbox.Service

	// runCtx scopes asynchronous runs to the server lifetime so Stop does not wait on them forever.
	runCtx     context.Context
	cancelRuns context.CancelFunc

	mu       sync.Mutex
	listener net.Listener
	server   *http.Server
	wg       sync.WaitGroup
}

// NewKernelServer constructs a KernelServer backed by st and cfg.
func NewKernelServer(st *store.Store, cfg ServerConfig) (*KernelServer, error) {
	somaDir := cfg.SomaDir
	if somaDir == "" && st != nil {
		somaDir = st.SomaDir()
	}
	if somaDir == "" {
		return nil, errors.New("bridge: SomaDir is required")
	}
	if err := os.MkdirAll(somaDir, 0o700); err != nil {
		return nil, fmt.Errorf("create somaDir: %w", err)
	}
	if cfg.SocketPath == "" {
		cfg.SocketPath = filepath.Join(somaDir, "kernel.sock")
	}
	if cfg.TokenPath == "" {
		cfg.TokenPath = filepath.Join(somaDir, "bridge.token")
	}
	if cfg.Version == "" {
		cfg.Version = "v0.3.0"
	}

	tok, err := loadOrCreateBridgeToken(cfg.TokenPath)
	if err != nil {
		return nil, err
	}

	ks := &KernelServer{
		store: st,
		cfg:   cfg,
		token: tok,
		inbox: inbox.NewService(st),
	}
	ks.runCtx, ks.cancelRuns = context.WithCancel(context.Background())
	if ks.cfg.RunStarter == nil {
		ks.cfg.RunStarter = ks.defaultRunStarter
	}
	if ks.cfg.BranchResumer == nil {
		ks.cfg.BranchResumer = ks.defaultBranchResumer
	}
	return ks, nil
}

// SocketPath returns the bound Unix socket filesystem path.
func (s *KernelServer) SocketPath() string { return s.cfg.SocketPath }

// TokenPath returns the bridge bearer token file path.
func (s *KernelServer) TokenPath() string { return s.cfg.TokenPath }

// Token returns the active bearer token for authenticated IPC.
func (s *KernelServer) Token() string { return s.token }

// Start binds the 0600 Unix socket and begins serving until Stop or context cancellation.
func (s *KernelServer) Start(ctx context.Context) error {
	_ = os.Remove(s.cfg.SocketPath)
	ln, err := net.Listen("unix", s.cfg.SocketPath)
	if err != nil {
		return fmt.Errorf("listen unix %s: %w", s.cfg.SocketPath, err)
	}
	if err := os.Chmod(s.cfg.SocketPath, 0o600); err != nil {
		_ = ln.Close()
		return fmt.Errorf("chmod socket %s: %w", s.cfg.SocketPath, err)
	}

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
		_ = os.Remove(s.cfg.SocketPath)
		return err
	}
}

// Stop shuts down the socket server, waits for background runs, and removes the socket file.
func (s *KernelServer) Stop(ctx context.Context) error {
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
	if s.cancelRuns != nil {
		s.cancelRuns()
	}
	s.wg.Wait()
	_ = os.Remove(s.cfg.SocketPath)
	return err
}

// Handler returns the HTTP mux protected by constant-time bearer auth.
func (s *KernelServer) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			writeJSON(w, http.StatusUnauthorized, ErrorResponse{Error: "unauthorized"})
			return
		}
		path := r.URL.Path
		switch {
		case r.Method == http.MethodGet && path == "/v1/health":
			writeJSON(w, http.StatusOK, HealthResponse{OK: true, Version: s.cfg.Version})

		case r.Method == http.MethodGet && path == "/v1/status":
			s.handleStatus(w, r)

		case r.Method == http.MethodGet && path == "/v1/inbox":
			s.handleListInbox(w, r)

		case r.Method == http.MethodPost && strings.HasPrefix(path, "/v1/inbox/") && strings.HasSuffix(path, "/approve"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/inbox/"), "/approve")
			s.handleInboxAction(w, r, id, true)

		case r.Method == http.MethodPost && strings.HasPrefix(path, "/v1/inbox/") && strings.HasSuffix(path, "/dismiss"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/inbox/"), "/dismiss")
			s.handleInboxAction(w, r, id, false)

		case r.Method == http.MethodPost && path == "/v1/runs":
			s.handleStartRun(w, r)

		case r.Method == http.MethodPost && strings.HasPrefix(path, "/v1/branches/") && strings.HasSuffix(path, "/reconcile"):
			branchID := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/branches/"), "/reconcile")
			s.handleReconcile(w, r, branchID)

		default:
			writeJSON(w, http.StatusNotFound, ErrorResponse{Error: "not found"})
		}
	})
}

func (s *KernelServer) authorized(r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return false
	}
	got := strings.TrimPrefix(auth, "Bearer ")
	if len(got) != len(s.token) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) == 1
}

func (s *KernelServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	runs, err := s.store.ListRuns(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	branches, err := s.store.ListBranches(ctx, "")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	accounts, err := s.store.ListAccounts(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	pendingCount, err := s.store.CountPendingInbox(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, StatusResponse{
		Runs:              runs,
		Branches:          branches,
		Accounts:          accounts,
		PendingInboxCount: pendingCount,
	})
}

func (s *KernelServer) handleListInbox(w http.ResponseWriter, r *http.Request) {
	statusFilter := r.URL.Query().Get("status")
	if statusFilter == "" {
		statusFilter = "pending"
	} else if statusFilter == "all" {
		statusFilter = ""
	}
	items, err := s.store.ListInboxItems(r.Context(), statusFilter)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, InboxListResponse{Items: items})
}

func (s *KernelServer) handleInboxAction(w http.ResponseWriter, r *http.Request, id string, approve bool) {
	if id == "" || strings.Contains(id, "/") {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "invalid inbox id"})
		return
	}
	var req InboxActionRequest
	_ = decodeJSON(r.Body, &req)
	actor := req.Actor
	if actor == "" {
		actor = "bridge"
	}
	var msg string
	var err error
	prior, _ := s.store.GetInboxItem(r.Context(), id)
	if approve {
		msg, err = s.inbox.Approve(r.Context(), id, actor)
	} else {
		msg, err = s.inbox.Dismiss(r.Context(), id, actor)
	}
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, store.ErrNotFound) {
			code = http.StatusNotFound
		}
		writeJSON(w, code, ErrorResponse{Error: err.Error()})
		return
	}
	// An approval only queues the branch; continue it so approving from a chat channel actually
	// lets the task proceed.
	if approve && (prior.ItemKind == "approval" || prior.ItemKind == "reconcile") {
		branchID := prior.BranchID
		if branchID == "" && prior.ArtifactRef != "" {
			if act, aErr := s.store.GetAction(r.Context(), prior.ArtifactRef); aErr == nil {
				branchID = act.BranchID
			}
		}
		if branchID != "" {
			if br, bErr := s.store.GetBranch(r.Context(), branchID); bErr == nil && br.Status == string(dag.StatusQueued) {
				if rErr := s.cfg.BranchResumer(r.Context(), branchID); rErr != nil {
					msg += "; resume failed: " + rErr.Error()
				} else {
					msg += "; resumed"
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, InboxActionResponse{OK: true, Message: msg})
}

func (s *KernelServer) defaultBranchResumer(_ context.Context, branchID string) error {
	if s.cfg.Provider == nil {
		return errors.New("bridge server has no ModelProvider configured")
	}
	exec := dag.NewExecutor(s.store, s.cfg.Provider, nil, compiler.WorkerProfile())
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		_, _ = exec.ExecuteBranch(s.runCtx, branchID, 12)
	}()
	return nil
}

func (s *KernelServer) handleStartRun(w http.ResponseWriter, r *http.Request) {
	var req StartRunRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
		return
	}
	if strings.TrimSpace(req.Goal) == "" {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "goal is required"})
		return
	}
	resp, err := s.cfg.RunStarter(r.Context(), req)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *KernelServer) defaultRunStarter(ctx context.Context, req StartRunRequest) (StartRunResponse, error) {
	if s.cfg.Provider == nil {
		return StartRunResponse{}, errors.New("bridge server has no ModelProvider configured")
	}
	prof := compiler.WorkerProfile()
	if strings.EqualFold(req.Profile, "frontier") {
		prof = compiler.FrontierProfile()
	}
	title := req.Title
	if title == "" {
		src := req.Source
		if src == "" {
			src = "bridge"
		}
		title = fmt.Sprintf("[%s] %s", src, req.Goal)
		if len(title) > 64 {
			title = title[:64]
		}
	}
	wsDir := s.cfg.WorkspaceDir
	if wsDir == "" {
		wsDir = filepath.Dir(s.cfg.SomaDir)
	}
	exec := dag.NewExecutor(s.store, s.cfg.Provider, nil, prof)
	runID, branchID, err := exec.StartRun(ctx, dag.RunRequest{
		Title:         title,
		WorkspacePath: wsDir,
		GoalSpec:      req.Goal,
		Profile:       prof,
	})
	if err != nil {
		return StartRunResponse{}, err
	}
	maxTurns := req.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 12
	}
	if req.Sync {
		res, err := exec.ExecuteBranch(ctx, branchID, maxTurns)
		if err != nil {
			status := string(dag.StatusFailed)
			if res != nil && res.Status != "" {
				status = string(res.Status)
			}
			return StartRunResponse{
				RunID:    runID,
				BranchID: branchID,
				Status:   status,
				Summary:  err.Error(),
			}, nil
		}
		return StartRunResponse{
			RunID:    runID,
			BranchID: branchID,
			Status:   string(res.Status),
			Summary:  res.FinalSummary,
		}, nil
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		_, _ = exec.ExecuteBranch(s.runCtx, branchID, maxTurns)
	}()
	return StartRunResponse{
		RunID:    runID,
		BranchID: branchID,
		Status:   "queued",
		Summary:  "started asynchronously",
	}, nil
}

func (s *KernelServer) handleReconcile(w http.ResponseWriter, r *http.Request, branchID string) {
	if branchID == "" {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "branch_id is required"})
		return
	}
	var req ReconcileRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
		return
	}
	out := dag.ReconcileOutcome(req.Outcome)
	if out != dag.OutcomeSucceeded && out != dag.OutcomeFailed {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "outcome must be succeeded or failed"})
		return
	}
	exec := dag.NewExecutor(s.store, nil, nil, compiler.WorkerProfile())
	if err := exec.ReconcileAction(r.Context(), req.ActionID, out, req.Summary); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, ReconcileResponse{
		OK:      true,
		Message: fmt.Sprintf("reconciled %s (%s)", req.ActionID, req.Outcome),
	})
}

func loadOrCreateBridgeToken(path string) (string, error) {
	if raw, err := os.ReadFile(path); err == nil {
		tok := strings.TrimSpace(string(raw))
		if tok != "" {
			_ = os.Chmod(path, 0o600)
			return tok, nil
		}
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate bridge token: %w", err)
	}
	tok := hex.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write bridge token: %w", err)
	}
	return tok, nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func decodeJSON(r io.Reader, dst any) error {
	if r == nil {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil
	}
	return json.Unmarshal(data, dst)
}
