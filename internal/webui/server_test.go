package webui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ra1ncandy/soma/internal/dag"
	"github.com/ra1ncandy/soma/internal/store"
	"github.com/ra1ncandy/soma/internal/vmem"
)

func newTestWebServer(t *testing.T, cfgMod func(*ServerConfig)) (*Server, *store.Store, string) {
	t.Helper()
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "README.md"), []byte("# soma test workspace\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(ws, ".soma"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.UpsertAccount(context.Background(), store.Account{
		ID:            "mock-acc",
		Provider:      "local",
		ModelID:       "mock-worker-v1",
		BillingMode:   "local",
		MaxConcurrent: 2,
	}); err != nil {
		t.Fatal(err)
	}

	cfg := ServerConfig{
		WorkspaceDir: ws,
		Addr:         "127.0.0.1:0",
		Provider:     &dag.MockProvider{ModelName: "mock-worker-v1"},
		ModelID:      "mock-worker-v1",
		UseMock:      true,
	}
	if cfgMod != nil {
		cfgMod(&cfg)
	}
	srv, err := NewServer(st, cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv, st, ws
}

func TestServer_LoopbackEnforcement(t *testing.T) {
	ws := t.TempDir()
	st, err := store.Open(filepath.Join(ws, ".soma"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	for _, badAddr := range []string{"0.0.0.0:7420", ":7420", "[::]:7420"} {
		if _, err := NewServer(st, ServerConfig{WorkspaceDir: ws, Addr: badAddr}); !errors.Is(err, ErrRemoteBindRefused) {
			t.Fatalf("expected ErrRemoteBindRefused for %q, got %v", badAddr, err)
		}
		if _, err := NewServer(st, ServerConfig{WorkspaceDir: ws, Addr: badAddr, AllowRemote: true}); !errors.Is(err, ErrRemoteBindRefused) {
			t.Fatalf("expected ErrRemoteBindRefused for %q with empty token, got %v", badAddr, err)
		}
	}

	if _, err := NewServer(st, ServerConfig{
		WorkspaceDir: ws,
		Addr:         "0.0.0.0:0",
		AllowRemote:  true,
		AuthToken:    "secret-web-token",
	}); err != nil {
		t.Fatalf("expected remote bind allowed with explicit AllowRemote + AuthToken, got %v", err)
	}
}

func TestServer_CSRFOriginAndEndToEndFlow(t *testing.T) {
	ctx := context.Background()
	srv, st, ws := newTestWebServer(t, nil)

	ts := httptest.NewServer(srv.Handler())
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Stop(shutCtx)
		ts.Close()
	}()

	// 1. GET / serves HTML with injected CSRF token and strict CSP headers.
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	var htmlBuf bytes.Buffer
	_, _ = htmlBuf.ReadFrom(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(htmlBuf.String(), srv.CSRFToken()) {
		t.Fatalf("GET / unexpected status=%d or missing CSRF token", resp.StatusCode)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "script-src 'self'") {
		t.Fatalf("missing security headers: %+v", resp.Header)
	}

	// 2. Mutations without valid CSRF or with foreign Origin are rejected with 403.
	reqNoCSRF, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/runs", strings.NewReader(`{"goal":"hi"}`))
	if r, err := ts.Client().Do(reqNoCSRF); err != nil || r.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 without CSRF, got resp=%v err=%v", r, err)
	} else {
		_ = r.Body.Close()
	}

	reqBadOrigin, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/runs", strings.NewReader(`{"goal":"hi"}`))
	reqBadOrigin.Header.Set("X-Soma-CSRF", srv.CSRFToken())
	reqBadOrigin.Header.Set("Origin", "https://evil.example")
	if r, err := ts.Client().Do(reqBadOrigin); err != nil || r.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for foreign Origin, got resp=%v err=%v", r, err)
	} else {
		_ = r.Body.Close()
	}

	postJSON := func(path string, body any) (int, map[string]any) {
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, ts.URL+path, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Soma-CSRF", srv.CSRFToken())
		req.Header.Set("Origin", ts.URL)
		r, err := ts.Client().Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		defer func() { _ = r.Body.Close() }()
		var out map[string]any
		_ = json.NewDecoder(r.Body).Decode(&out)
		return r.StatusCode, out
	}

	// 3. Open SSE stream briefly and confirm initial event frames arrive cleanly.
	sseCtx, cancelSSE := context.WithCancel(ctx)
	sseReq, _ := http.NewRequestWithContext(sseCtx, http.MethodGet, ts.URL+"/api/events", nil)
	sseResp, err := ts.Client().Do(sseReq)
	if err != nil {
		t.Fatalf("GET /api/events: %v", err)
	}
	sc := bufio.NewScanner(sseResp.Body)
	if !sc.Scan() || !strings.Contains(sc.Text(), "event: state") {
		t.Fatalf("expected initial SSE event line, got %q (err=%v)", sc.Text(), sc.Err())
	}
	cancelSSE()
	_ = sseResp.Body.Close()

	// 4. POST /api/runs (including XSS payload string to verify safe JSON handling & sandboxed completion).
	xssGoal := `Write verification note <script>alert("xss")</script>`
	code, startOut := postJSON("/api/runs", map[string]any{"goal": xssGoal, "max_turns": 6})
	if code != http.StatusAccepted || startOut["run_id"] == "" || startOut["branch_id"] == "" {
		t.Fatalf("POST /api/runs failed: code=%d out=%+v", code, startOut)
	}
	runID := startOut["run_id"].(string)
	branchID := startOut["branch_id"].(string)

	// Poll /api/state until background MockProvider completes.
	var state StateDTO
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r, err := ts.Client().Get(ts.URL + "/api/state")
		if err != nil {
			t.Fatalf("GET /api/state: %v", err)
		}
		_ = json.NewDecoder(r.Body).Decode(&state)
		_ = r.Body.Close()
		if len(state.Runs) > 0 && state.Runs[0].Status == "completed" {
			break
		}
		time.Sleep(40 * time.Millisecond)
	}
	if len(state.Runs) == 0 || state.Runs[0].Status != "completed" || len(state.Runs[0].Branches) == 0 {
		t.Fatalf("expected completed run in state, got %+v", state.Runs)
	}
	if len(state.Runs[0].Branches[0].Steps) != 3 {
		t.Fatalf("expected 3 completed steps from MockProvider, got %d", len(state.Runs[0].Branches[0].Steps))
	}
	// Verify MockProvider wrote its verification note inside the isolated branch workspace.
	notePath := filepath.Join(state.Runs[0].Branches[0].WorkspaceDir, "artifacts", "soma_mock_note.txt")
	if _, err := os.Stat(notePath); err != nil {
		t.Fatalf("expected mock note written in workspace %s: %v", ws, err)
	}

	// 5. Seed a large @page and verify GET /api/pages/{id}.
	obs, err := vmem.NewManager(st).Observe(ctx, vmem.ObserveRequest{
		RunID:                runID,
		NameHint:             "big.log",
		Content:              []byte(strings.Repeat("line ok\n", 300)),
		TrustTier:            "trusted_repo",
		InlineThresholdBytes: 512,
	})
	if err != nil || !obs.Paged {
		t.Fatalf("Observe page: %v", err)
	}
	pgResp, err := ts.Client().Get(ts.URL + "/api/pages/" + obs.PageID + "?start=1&end=5")
	if err != nil || pgResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/pages failed: resp=%v err=%v", pgResp, err)
	}
	var pgOut map[string]any
	_ = json.NewDecoder(pgResp.Body).Decode(&pgOut)
	_ = pgResp.Body.Close()
	if !strings.Contains(pgOut["content"].(string), "line ok") {
		t.Fatalf("unexpected page slice content: %+v", pgOut)
	}

	// 6. Fork and Replay branch.
	fCode, fOut := postJSON("/api/branches/"+branchID+"/fork", map[string]string{"label": "ui-fork"})
	if fCode != http.StatusCreated || fOut["branch_id"] == "" {
		t.Fatalf("POST fork failed: code=%d out=%+v", fCode, fOut)
	}
	repCode, repOut := postJSON("/api/branches/"+branchID+"/replay", map[string]any{})
	if repCode != http.StatusOK || repOut["diverged"] != false || int(repOut["verified_count"].(float64)) != 3 {
		t.Fatalf("POST replay unexpected: code=%d out=%+v", repCode, repOut)
	}

	// 7. Inbox approve & Memory toggle / activate.
	_ = st.InsertMemoryNode(ctx, store.MemoryNode{
		ID: "M-ui-prop", Scope: "global", Kind: "rule", Content: "UI candidate rule", Status: "proposed", TrustTier: "untrusted_external", Enabled: true,
	})
	_ = st.InsertInboxItem(ctx, store.InboxItem{
		ID: "inb_ui_1", RunID: runID, BranchID: branchID, ItemKind: "memory_proposal",
		Title: "Approve M-ui-prop", Summary: "Candidate", ArtifactRef: "M-ui-prop", Urgency: "normal",
	})
	inbCode, _ := postJSON("/api/inbox/inb_ui_1/approve", map[string]any{})
	if inbCode != http.StatusOK {
		t.Fatalf("POST inbox approve failed: %d", inbCode)
	}
	if node, err := st.GetMemoryNode(ctx, "M-ui-prop"); err != nil || node.Status != "active" {
		t.Fatalf("expected M-ui-prop activated via inbox approve, got %+v err=%v", node, err)
	}
	togCode, _ := postJSON("/api/memory/M-ui-prop/toggle", map[string]bool{"enabled": false})
	if togCode != http.StatusOK {
		t.Fatalf("POST memory toggle failed: %d", togCode)
	}
	if node, err := st.GetMemoryNode(ctx, "M-ui-prop"); err != nil || node.Enabled {
		t.Fatalf("expected M-ui-prop disabled after toggle, got %+v err=%v", node, err)
	}
}
