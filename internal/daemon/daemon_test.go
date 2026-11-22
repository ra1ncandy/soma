package daemon

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ra1ncandy/soma/internal/store"
)

func openTestDaemonStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), ".soma"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestDaemon_ZeroTokenSentinelAndWatchStop(t *testing.T) {
	ctx := context.Background()
	st := openTestDaemonStore(t)
	cfg := DefaultConfig()
	d := New(st, cfg, t.TempDir())

	var outputMu sync.Mutex
	curOutput := "state=unchanged\n"
	stopReady := false
	d.CommandRunner = func(_ context.Context, cmd, _ string) ([]byte, int, error) {
		outputMu.Lock()
		defer outputMu.Unlock()
		if cmd == "check_stop" {
			if stopReady {
				return []byte("stopped"), 0, nil
			}
			return []byte("keep_going"), 1, fmt.Errorf("not ready")
		}
		return []byte(curOutput), 0, nil
	}

	var calls atomic.Int64
	d.Handler = func(_ context.Context, _ TriggerSpec, _ CoalescedEvent, _ string) (string, float64, error) {
		calls.Add(1)
		return "", 0, nil
	}

	spec := TriggerSpec{
		ID:          "lab-export",
		Type:        "watch",
		Command:     "list_exports",
		StopCommand: "check_stop",
		WakeOn:      "output_hash_changed",
	}

	// 10 unchanged probes -> 0 handler invocations!
	for i := 0; i < 10; i++ {
		out, err := d.EvaluateSentinel(ctx, spec)
		if err != nil {
			t.Fatalf("probe %d error: %v", i, err)
		}
		if out.Status != "skipped" {
			t.Fatalf("probe %d expected skipped, got %+v", i, out)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("expected 0 handler calls on unchanged probes, got %d", calls.Load())
	}

	// Mutate output -> 1 invocation.
	outputMu.Lock()
	curOutput = "state=new_export_v2\n"
	outputMu.Unlock()

	outChg, err := d.EvaluateSentinel(ctx, spec)
	if err != nil || outChg.Status != "done" {
		t.Fatalf("changed probe expected done, got %+v err=%v", outChg, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected 1 handler call after output change, got %d", calls.Load())
	}

	// Subsequent unchanged probe -> 0 new invocations.
	outAfter, err := d.EvaluateSentinel(ctx, spec)
	if err != nil || outAfter.Status != "skipped" || calls.Load() != 1 {
		t.Fatalf("subsequent unchanged probe expected skipped with calls=1, got %+v calls=%d", outAfter, calls.Load())
	}

	// StopCommand returns 0 -> watch stops cleanly.
	outputMu.Lock()
	stopReady = true
	outputMu.Unlock()
	outStop, err := d.EvaluateSentinel(ctx, spec)
	if err != nil || outStop.Status != "stopped" {
		t.Fatalf("expected stopped outcome, got %+v err=%v", outStop, err)
	}
}

func TestDaemon_CoalescerIdempotencyWarmBranchAndTTLGC(t *testing.T) {
	ctx := context.Background()
	st := openTestDaemonStore(t)
	cfg := DefaultConfig()
	d := New(st, cfg, t.TempDir())

	now := time.Unix(1700000000, 0)
	d.Now = func() time.Time { return now }

	var fires atomic.Int64
	var seenCounts []int
	var mu sync.Mutex
	branchSeq := atomic.Int64{}
	d.Handler = func(_ context.Context, _ TriggerSpec, ev CoalescedEvent, warmID string) (string, float64, error) {
		fires.Add(1)
		mu.Lock()
		seenCounts = append(seenCounts, ev.Count)
		mu.Unlock()
		if warmID != "" {
			return warmID, 0.01, nil
		}
		return fmt.Sprintf("br_w_%d", branchSeq.Add(1)), 0.01, nil
	}

	spec := TriggerSpec{
		ID:             "ci-triage",
		Type:           "webhook",
		Debounce:       "35ms",
		IdempotencyKey: "ci:{{ .repo }}:{{ .sha }}",
		SessionMode:    "warm_branch",
		TTL:            "45m",
		TTLRetention:   "1h",
	}

	// Submit 20 rapid burst events inside debounce window -> collapses into 1 dispatch with Count == 20.
	doneCh := make(chan TriggerOutcome, 2)
	for i := 0; i < 20; i++ {
		d.Coalescer.Submit(ctx, spec, Event{
			TriggerID:  spec.ID,
			Source:     "webhook",
			Payload:    []byte(`{"sha":"abc123"}`),
			Fields:     map[string]any{"repo": "soma", "sha": "abc123"},
			ReceivedAt: now,
		}, func(ce CoalescedEvent) {
			out, _ := d.HandleEvent(ctx, spec, ce)
			doneCh <- out
		})
	}
	out1 := <-doneCh
	if out1.Status != "done" || fires.Load() != 1 || seenCounts[0] != 20 {
		t.Fatalf("expected 1 coalesced fire with count 20, got out=%+v fires=%d counts=%v", out1, fires.Load(), seenCounts)
	}

	// Duplicate idempotency_key outside debounce -> rejected with status="coalesced", 0 additional fires.
	outDup, err := d.HandleEvent(ctx, spec, CoalescedEvent{
		Event:          Event{TriggerID: spec.ID, Source: "webhook", ReceivedAt: now},
		Count:          1,
		IdempotencyKey: "ci:soma:abc123",
	})
	if err != nil || outDup.Status != "coalesced" || fires.Load() != 1 {
		t.Fatalf("expected duplicate rejection, got %+v fires=%d err=%v", outDup, fires.Load(), err)
	}

	// Different sha inside warm TTL reuses the exact warmBranchID!
	outWarm, err := d.HandleEvent(ctx, spec, CoalescedEvent{
		Event:          Event{TriggerID: spec.ID, Source: "webhook", ReceivedAt: now},
		Count:          1,
		IdempotencyKey: "ci:soma:def456",
	})
	if err != nil || outWarm.Status != "done" || outWarm.BranchID != out1.BranchID {
		t.Fatalf("expected warm_branch reuse %s, got %+v err=%v", out1.BranchID, outWarm, err)
	}

	// Advance clock past 1h retention and run TTL GC -> purges expired rows.
	now = now.Add(2 * time.Hour)
	purged, err := d.RunGC(ctx)
	if err != nil || purged != 2 {
		t.Fatalf("expected 2 expired trigger_runs purged, got %d err=%v", purged, err)
	}
}

func TestDaemon_InhibitoryBrakesAndUrgentOverride(t *testing.T) {
	ctx := context.Background()
	st := openTestDaemonStore(t)

	cfg := DefaultConfig()
	cfg.QuietHours = "23:00-07:30"
	cfg.MaxPendingInbox = 2
	cfg.DailyMeteredBudgetUSD = 5.0

	quietTime := time.Date(2026, 11, 22, 23, 30, 0, 0, time.UTC)
	dayTime := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

	// 1) Quiet hours inhibit normal urgency, bypassed by urgent.
	dec, err := EvaluateBrakes(ctx, st, cfg, "normal", quietTime)
	if err != nil || !dec.Inhibited {
		t.Fatalf("expected quiet_hours inhibition at 23:30, got %+v err=%v", dec, err)
	}
	decUrg, err := EvaluateBrakes(ctx, st, cfg, "urgent", quietTime)
	if err != nil || decUrg.Inhibited {
		t.Fatalf("expected urgent to bypass quiet_hours, got %+v err=%v", decUrg, err)
	}

	// 2) Pending inbox >= max_pending_inbox inhibits normal urgency during daytime.
	for i := 0; i < 2; i++ {
		if err := st.InsertInboxItem(ctx, store.InboxItem{
			ID: fmt.Sprintf("inb_blk_%d", i), ItemKind: "draft_pr", Title: "Pending", Summary: "s",
		}); err != nil {
			t.Fatalf("InsertInboxItem: %v", err)
		}
	}
	decInbox, err := EvaluateBrakes(ctx, st, cfg, "normal", dayTime)
	if err != nil || !decInbox.Inhibited {
		t.Fatalf("expected pending inbox inhibition, got %+v err=%v", decInbox, err)
	}

	// 3) Daily metered budget breach inhibits even urgent.
	if err := st.UpsertAccount(ctx, store.Account{
		ID: "met_brake", Provider: "openai", ModelID: "m", BillingMode: "metered", DailyBudgetUSD: 10,
	}); err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	if err := st.ReleaseLease(ctx, "met_brake", 5.25); err != nil {
		t.Fatalf("ReleaseLease spend: %v", err)
	}
	decBudget, err := EvaluateBrakes(ctx, st, cfg, "urgent", dayTime)
	if err != nil || !decBudget.Inhibited {
		t.Fatalf("expected metered budget inhibition, got %+v err=%v", decBudget, err)
	}

	// 4) Reset metered spend below budget and test all accounts cooling down brake.
	cfg.DailyMeteredBudgetUSD = 100
	if err := st.SetCooldown(ctx, "met_brake", dayTime.Add(5*time.Minute).UnixMilli(), "429"); err != nil {
		t.Fatalf("SetCooldown: %v", err)
	}
	decCool, err := EvaluateBrakes(ctx, st, cfg, "urgent", dayTime)
	if err != nil || !decCool.Inhibited || decCool.Reason != "all_accounts_cooling_down" {
		t.Fatalf("expected all_accounts_cooling_down, got %+v err=%v", decCool, err)
	}
}

func TestDaemon_WebhookHMACFilterAndRealFSNotify(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st := openTestDaemonStore(t)
	ws := t.TempDir()
	watchFile := filepath.Join(ws, "signal.txt")
	if err := os.WriteFile(watchFile, []byte("init"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg := DefaultConfig()
	cfg.WebhookAddr = "127.0.0.1:0"
	cfg.WebhookSecret = "super-secret-key"
	cfg.Triggers = []TriggerSpec{
		{
			ID:       "ci-fail",
			Type:     "webhook",
			Filter:   "conclusion == 'failure'",
			Debounce: "10ms",
		},
		{
			ID:       "fs-watch",
			Type:     "fsnotify",
			Paths:    []string{watchFile},
			Debounce: "25ms",
		},
	}

	d := New(st, cfg, ws)
	webhookFired := make(chan CoalescedEvent, 4)
	fsFired := make(chan CoalescedEvent, 4)
	d.Handler = func(_ context.Context, spec TriggerSpec, ev CoalescedEvent, _ string) (string, float64, error) {
		if spec.ID == "ci-fail" {
			webhookFired <- ev
		} else if spec.ID == "fs-watch" {
			fsFired <- ev
		}
		return "", 0, nil
	}

	// Direct HTTP handler verification for 401 bad HMAC, 202 filter skip, and 202 match.
	ts := httptest.NewServer(d.WebhookHandler())
	defer ts.Close()

	failPayload := []byte(`{"conclusion":"failure","sha":"111"}`)
	reqBadSig, _ := http.NewRequest(http.MethodPost, ts.URL+"/webhook/ci-fail", bytes.NewReader(failPayload))
	reqBadSig.Header.Set("X-Soma-Signature", "sha256=deadbeef")
	respBad, err := http.DefaultClient.Do(reqBadSig)
	if err != nil {
		t.Fatalf("bad sig POST: %v", err)
	}
	_ = respBad.Body.Close()
	if respBad.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 on invalid HMAC, got %d", respBad.StatusCode)
	}

	sign := func(b []byte) string {
		mac := hmac.New(sha256.New, []byte(cfg.WebhookSecret))
		mac.Write(b)
		return "sha256=" + hex.EncodeToString(mac.Sum(nil))
	}

	passPayload := []byte(`{"conclusion":"success","sha":"222"}`)
	reqFilterMiss, _ := http.NewRequest(http.MethodPost, ts.URL+"/webhook/ci-fail", bytes.NewReader(passPayload))
	reqFilterMiss.Header.Set("X-Soma-Signature", sign(passPayload))
	respMiss, err := http.DefaultClient.Do(reqFilterMiss)
	if err != nil {
		t.Fatalf("filter miss POST: %v", err)
	}
	_ = respMiss.Body.Close()
	if respMiss.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202 skipped, got %d", respMiss.StatusCode)
	}

	reqValid, _ := http.NewRequest(http.MethodPost, ts.URL+"/webhook/ci-fail", bytes.NewReader(failPayload))
	reqValid.Header.Set("X-Soma-Signature", sign(failPayload))
	respOK, err := http.DefaultClient.Do(reqValid)
	if err != nil {
		t.Fatalf("valid POST: %v", err)
	}
	_ = respOK.Body.Close()
	if respOK.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202 accepted, got %d", respOK.StatusCode)
	}

	select {
	case <-webhookFired:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for valid webhook event")
	}

	// Start full Daemon and verify real fsnotify trigger.
	startDone := make(chan error, 1)
	go func() {
		startDone <- d.Start(ctx)
	}()
	time.Sleep(60 * time.Millisecond)

	if err := os.WriteFile(watchFile, []byte("updated payload"), 0o644); err != nil {
		t.Fatalf("WriteFile update: %v", err)
	}
	select {
	case <-fsFired:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for fsnotify trigger")
	}

	cancel()
	if err := <-startDone; err != nil {
		t.Fatalf("Daemon.Start shutdown err: %v", err)
	}
}
