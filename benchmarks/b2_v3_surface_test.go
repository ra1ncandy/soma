package benchmarks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ra1ncandy/soma/internal/bridge"
	"github.com/ra1ncandy/soma/internal/compiler"
	"github.com/ra1ncandy/soma/internal/daemon"
	"github.com/ra1ncandy/soma/internal/dag"
	"github.com/ra1ncandy/soma/internal/store"
	"github.com/ra1ncandy/soma/internal/taint"
	"github.com/ra1ncandy/soma/internal/tui"
)

type v3FailoverProvider struct {
	failPrimaryOnce atomic.Bool
}

func (p *v3FailoverProvider) Generate(_ context.Context, req dag.GenerateRequest) (*dag.GenerateResponse, error) {
	if req.AccountID == "codex-plus" && req.TurnIndex == 2 && p.failPrimaryOnce.CompareAndSwap(true, false) {
		return nil, &dag.RateLimitError{
			AccountID:  req.AccountID,
			RetryAfter: 50 * time.Millisecond,
			Err:        errors.New("429 primary quota exhausted"),
		}
	}
	if req.TurnIndex == 1 {
		return &dag.GenerateResponse{
			Reasoning: "collect workspace telemetry",
			ToolCall: &dag.ToolCallSpec{
				Name:        "collect_stats",
				ArgsJSON:    `{}`,
				EffectClass: taint.EffectReadOnly,
			},
			StateHash:       "v3_s1",
			InputTokens:     180,
			CacheReadTokens: 1600,
			OutputTokens:    35,
		}, nil
	}
	return &dag.GenerateResponse{
		Reasoning:       "finish verified task",
		Done:            true,
		FinalSummary:    "v3 surface completed on " + req.AccountID,
		StateHash:       "v3_done_" + req.AccountID,
		InputTokens:     120,
		CacheReadTokens: 1700,
		OutputTokens:    30,
	}, nil
}

func TestB2_V3SurfaceQuotasDaemonBridgeAndTUI(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	st, err := store.Open(filepath.Join(ws, ".soma"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	for _, acc := range []store.Account{
		{ID: "codex-plus", Provider: "openai-codex", ModelID: "gpt-6.2-sol", BillingMode: "subscription", MaxConcurrent: 2},
		{ID: "xai-premium", Provider: "xai-oauth", ModelID: "grok-4.6", BillingMode: "subscription", MaxConcurrent: 2},
	} {
		if err := st.UpsertAccount(ctx, acc); err != nil {
			t.Fatalf("UpsertAccount %s: %v", acc.ID, err)
		}
	}

	// 1. Multi-subscription 429 failover + clean Epoch 2 Handoff Packet + primary recovery.
	prov := &v3FailoverProvider{}
	prov.failPrimaryOnce.Store(true)
	exec := dag.NewExecutor(st, prov, nil, compiler.WorkerProfile())
	exec.AllowFailover = true
	exec.AccountIDs = []string{"codex-plus", "xai-premium"}

	_, br1, err := exec.StartRun(ctx, dag.RunRequest{
		WorkspacePath: ws,
		GoalSpec:      "v3 multi-sub failover",
		AccountIDs:    []string{"codex-plus", "xai-premium"},
	})
	if err != nil {
		t.Fatalf("StartRun br1: %v", err)
	}
	res1, err := exec.ExecuteBranch(ctx, br1, 10)
	if err != nil || res1.Status != dag.StatusCompleted || res1.Rollovers < 1 {
		t.Fatalf("failover ExecuteBranch failed: res=%+v err=%v", res1, err)
	}
	steps1, err := st.ListBranchSteps(ctx, br1)
	if err != nil || len(steps1) != 2 || steps1[0].AccountID != "codex-plus" || steps1[1].AccountID != "xai-premium" || steps1[1].Epoch != 2 {
		t.Fatalf("unexpected failover steps: %+v err=%v", steps1, err)
	}
	time.Sleep(65 * time.Millisecond)

	// New run prefers codex-plus again after cooldown expires.
	_, br2, err := exec.StartRun(ctx, dag.RunRequest{
		WorkspacePath: ws,
		GoalSpec:      "v3 primary return after cooldown",
		AccountIDs:    []string{"codex-plus", "xai-premium"},
	})
	if err != nil {
		t.Fatalf("StartRun br2: %v", err)
	}
	res2, err := exec.ExecuteBranch(ctx, br2, 10)
	if err != nil || res2.Status != dag.StatusCompleted {
		t.Fatalf("primary return ExecuteBranch failed: res=%+v err=%v", res2, err)
	}
	steps2, err := st.ListBranchSteps(ctx, br2)
	if err != nil || len(steps2) != 2 || steps2[0].AccountID != "codex-plus" {
		t.Fatalf("expected primary preference restored on br2, got %+v err=%v", steps2, err)
	}

	// 2. Tier-0 Daemon: 100 unchanged sentinel ticks (0 tokens) + 1 changed tick + 20 burst webhooks -> 1 execution + TTL GC.
	cfg := daemon.DefaultConfig()
	d := daemon.New(st, cfg, ws)
	now := time.Unix(1700001000, 0)
	d.Now = func() time.Time { return now }

	var sentMu sync.Mutex
	sentPayload := "export_v1"
	d.CommandRunner = func(_ context.Context, _, _ string) ([]byte, int, error) {
		sentMu.Lock()
		defer sentMu.Unlock()
		return []byte(sentPayload), 0, nil
	}
	var handlerCalls atomic.Int64
	d.Handler = func(_ context.Context, _ daemon.TriggerSpec, _ daemon.CoalescedEvent, _ string) (string, float64, error) {
		handlerCalls.Add(1)
		return "", 0, nil
	}
	sentSpec := daemon.TriggerSpec{
		ID:           "lab-sentinel",
		Type:         "sentinel",
		Command:      "check",
		WakeOn:       "output_hash_changed",
		TTLRetention: "1h",
	}
	for i := 0; i < 100; i++ {
		out, err := d.EvaluateSentinel(ctx, sentSpec)
		if err != nil || out.Status != "skipped" {
			t.Fatalf("tick %d expected zero-token skipped, got %+v err=%v", i, out, err)
		}
	}
	if handlerCalls.Load() != 0 {
		t.Fatalf("expected 0 handler calls on 100 unchanged sentinel ticks, got %d", handlerCalls.Load())
	}
	sentMu.Lock()
	sentPayload = "export_v2"
	sentMu.Unlock()
	if out, err := d.EvaluateSentinel(ctx, sentSpec); err != nil || out.Status != "done" || handlerCalls.Load() != 1 {
		t.Fatalf("expected 1 trigger on changed sentinel output, got %+v calls=%d err=%v", out, handlerCalls.Load(), err)
	}

	whSpec := daemon.TriggerSpec{
		ID:             "ci-burst",
		Type:           "webhook",
		Debounce:       "30ms",
		IdempotencyKey: "ci:{{ .repo }}:{{ .sha }}",
		TTLRetention:   "1h",
	}
	whDone := make(chan daemon.TriggerOutcome, 1)
	for i := 0; i < 20; i++ {
		d.Coalescer.Submit(ctx, whSpec, daemon.Event{
			TriggerID:  whSpec.ID,
			Source:     "webhook",
			Payload:    []byte(`{"sha":"s99"}`),
			Fields:     map[string]any{"repo": "soma", "sha": "s99"},
			ReceivedAt: now,
		}, func(ce daemon.CoalescedEvent) {
			out, _ := d.HandleEvent(ctx, whSpec, ce)
			whDone <- out
		})
	}
	if out := <-whDone; out.Status != "done" || handlerCalls.Load() != 2 {
		t.Fatalf("expected 20 burst webhooks coalesced to 1 execution (total 2), got %+v calls=%d", out, handlerCalls.Load())
	}
	now = now.Add(2 * time.Hour)
	if purged, err := d.RunGC(ctx); err != nil || purged < 2 {
		t.Fatalf("expected TTL GC to purge expired trigger_runs, got purged=%d err=%v", purged, err)
	}

	// 3. Out-of-process Bridge (KernelServer + DiscordAdapter + TelegramAdapter) + Concurrent TUI snapshots under -race.
	sockDir, err := os.MkdirTemp("/tmp", "soma-b2v3-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer func() { _ = os.RemoveAll(sockDir) }()

	srv, err := bridge.NewKernelServer(st, bridge.ServerConfig{
		SomaDir:      st.SomaDir(),
		WorkspaceDir: ws,
		SocketPath:   filepath.Join(sockDir, "kernel.sock"),
		TokenPath:    filepath.Join(sockDir, "bridge.token"),
		Provider:     &dag.MockProvider{ModelName: "mock-worker-v1"},
	})
	if err != nil {
		t.Fatalf("NewKernelServer: %v", err)
	}
	srvCtx, cancelSrv := context.WithCancel(ctx)
	srvDone := make(chan error, 1)
	go func() {
		srvDone <- srv.Start(srvCtx)
	}()
	defer func() {
		cancelSrv()
		<-srvDone
	}()

	client := bridge.NewClient(srv.SocketPath(), srv.Token())
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := client.Health(ctx); err == nil {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}

	if err := st.InsertMemoryNode(ctx, store.MemoryNode{
		ID: "M-v3-prop", Scope: "global", Kind: "rule", Content: "bridge memory approval",
		Status: "proposed", TrustTier: "untrusted_external", Enabled: true,
	}); err != nil {
		t.Fatalf("InsertMemoryNode: %v", err)
	}
	if err := st.InsertInboxItem(ctx, store.InboxItem{
		ID: "inb_v3_mem", ItemKind: "memory_proposal", Title: "Approve M-v3-prop",
		Summary: "Check candidate", ArtifactRef: "M-v3-prop", Urgency: "normal",
	}); err != nil {
		t.Fatalf("InsertInboxItem: %v", err)
	}

	// Concurrent TUI snapshot/render loop running alongside Discord/Telegram bridge writes.
	var tuiErr atomic.Value
	var tuiWG sync.WaitGroup
	tuiWG.Add(1)
	go func() {
		defer tuiWG.Done()
		for i := 0; i < 8; i++ {
			snap, err := tui.LoadSnapshot(ctx, st, ws)
			if err != nil {
				tuiErr.Store(err)
				return
			}
			_ = snap
			m := tui.New(st, ws)
			v := m.View()
			if !strings.Contains(v.Content, br1) {
				tuiErr.Store(fmt.Errorf("TUI View missing branch %s", br1))
				return
			}
		}
	}()

	disc := bridge.NewDiscordAdapter(client, bridge.DiscordConfig{
		AllowedUserIDs: []string{"op_1"},
	})
	appReply, handled, err := disc.HandleMessage(ctx, "op_1", "chan_1", "!soma approve inb_v3_mem")
	if err != nil || !handled || !strings.Contains(appReply, "approved") {
		t.Fatalf("Discord approve failed: reply=%q handled=%v err=%v", appReply, handled, err)
	}
	mNode, err := st.GetMemoryNode(ctx, "M-v3-prop")
	if err != nil || mNode.Status != "active" {
		t.Fatalf("expected M-v3-prop active after Discord approval, got %+v err=%v", mNode, err)
	}

	var tgReplies []string
	var tgMu sync.Mutex
	mockTG := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/getUpdates"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"result": []map[string]any{{
					"update_id": 1,
					"message": map[string]any{
						"message_id": 1,
						"from":       map[string]any{"id": 77, "username": "alice"},
						"chat":       map[string]any{"id": 88},
						"text":       "/run verify bridge e2e",
					},
				}},
			})
		case strings.Contains(r.URL.Path, "/sendMessage"):
			var body struct {
				Text string `json:"text"`
			}
			var buf bytes.Buffer
			_, _ = buf.ReadFrom(r.Body)
			_ = json.Unmarshal(buf.Bytes(), &body)
			tgMu.Lock()
			tgReplies = append(tgReplies, body.Text)
			tgMu.Unlock()
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer mockTG.Close()

	tg := bridge.NewTelegramAdapter(client, bridge.TelegramConfig{
		BotToken:       "tg-tok",
		APIBaseURL:     mockTG.URL,
		AllowedUserIDs: []int64{77},
		AllowedChatIDs: []int64{88},
	})
	if processed, err := tg.PollUpdatesOnce(ctx); err != nil || processed != 1 {
		t.Fatalf("Telegram PollUpdatesOnce failed: processed=%d err=%v", processed, err)
	}
	tgMu.Lock()
	if len(tgReplies) != 1 || !strings.Contains(tgReplies[0], "started run") {
		t.Fatalf("unexpected Telegram replies: %+v", tgReplies)
	}
	tgMu.Unlock()

	tuiWG.Wait()
	if v := tuiErr.Load(); v != nil {
		t.Fatalf("concurrent TUI WAL reader error: %v", v)
	}

	t.Logf("=== SUITE B2/v0.3 SURFACE PASSED: 429 Failover + Tier-0 Daemon + Unix/Discord/Telegram Bridge + Concurrent 4-Tab TUI ===")
}
