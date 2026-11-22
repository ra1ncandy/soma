package dag

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ra1ncandy/soma/internal/compiler"
	"github.com/ra1ncandy/soma/internal/store"
	"github.com/ra1ncandy/soma/internal/taint"
)

type scriptedProvider struct {
	fn func(turn int, req GenerateRequest) (*GenerateResponse, error)
}

func (s *scriptedProvider) Generate(_ context.Context, req GenerateRequest) (*GenerateResponse, error) {
	return s.fn(req.TurnIndex, req)
}

type recordingToolRunner struct {
	calls        []ToolCall
	remoteWrites atomic.Int64
	idemSeen     map[string]ToolOutput
}

func (r *recordingToolRunner) Execute(_ context.Context, call ToolCall) (ToolOutput, error) {
	r.calls = append(r.calls, call)
	if call.EffectClass == "idempotent_write" {
		if r.idemSeen == nil {
			r.idemSeen = make(map[string]ToolOutput)
		}
		if prev, ok := r.idemSeen[call.IdempotencyKey]; ok {
			// Server-side idempotency dedup: returns cached result without executing twice.
			return prev, nil
		}
		r.remoteWrites.Add(1)
		out := ToolOutput{
			NameHint:  call.Name + ".json",
			Content:   []byte(`{"status":"created","id":101}`),
			TrustTier: "trusted_repo",
		}
		r.idemSeen[call.IdempotencyKey] = out
		return out, nil
	}
	if call.EffectClass == taint.EffectIrreversible {
		r.remoteWrites.Add(1)
	}
	return ToolOutput{
		NameHint:  call.Name + ".log",
		Content:   []byte("ok from " + call.Name),
		TrustTier: "trusted_repo",
	}, nil
}

func setupAccount(t *testing.T, st *store.Store, id string) {
	t.Helper()
	if err := st.UpsertAccount(context.Background(), store.Account{
		ID:            id,
		Provider:      "test",
		ModelID:       "model-test",
		BillingMode:   "subscription",
		MaxConcurrent: 2,
	}); err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
}

func TestDurable429SuspendResumeAndGraveyardOnFork(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	st, err := store.Open(filepath.Join(ws, ".soma"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	setupAccount(t, st, "acc_sub")

	var attempts int
	prov := &scriptedProvider{
		fn: func(turn int, req GenerateRequest) (*GenerateResponse, error) {
			attempts++
			if attempts == 2 {
				return nil, &RateLimitError{AccountID: req.AccountID, RetryAfter: 30 * time.Millisecond, Err: errors.New("429")}
			}
			if turn == 1 {
				return &GenerateResponse{
					Reasoning: "read repo",
					ToolCall:  &ToolCallSpec{Name: "noop", ArgsJSON: `{"a":1}`, EffectClass: taint.EffectReadOnly},
					StateHash: "s1",
				}, nil
			}
			return &GenerateResponse{
				Reasoning:    "done",
				Done:         true,
				FinalSummary: "finished cleanly",
				StateHash:    "done",
			}, nil
		},
	}

	exec := NewExecutor(st, prov, &recordingToolRunner{}, compiler.WorkerProfile())
	runID, branchID, err := exec.StartRun(ctx, RunRequest{
		WorkspacePath: ws,
		GoalSpec:      "survive 429",
		AccountIDs:    []string{"acc_sub"},
	})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	res1, err := exec.ExecuteBranch(ctx, branchID, 10)
	var sq *SuspendedQuotaError
	if !errors.As(err, &sq) {
		t.Fatalf("expected SuspendedQuotaError, got (%+v, %v)", res1, err)
	}
	if res1.TurnsExecuted != 1 || res1.Status != StatusSuspendedQuota {
		t.Fatalf("expected 1 turn committed before 429 suspend, got %+v", res1)
	}

	time.Sleep(50 * time.Millisecond)
	res2, err := exec.ExecuteBranch(ctx, branchID, 10)
	if err != nil || res2.Status != StatusCompleted || res2.TurnsExecuted != 1 {
		t.Fatalf("resume after cooldown failed: res=%+v err=%v", res2, err)
	}

	// Fork from step 1 and abandon branch -> verify Graveyard entry enters next epoch L1.
	steps, err := st.ListBranchSteps(ctx, branchID)
	if err != nil || len(steps) != 2 {
		t.Fatalf("expected 2 steps, got %d err=%v", len(steps), err)
	}
	forkID, err := exec.ForkBranch(ctx, runID, steps[0].ID, "bad-hypothesis")
	if err != nil {
		t.Fatalf("ForkBranch: %v", err)
	}
	if err := exec.AbandonBranch(ctx, forkID, "caused deadlock in callback", ""); err != nil {
		t.Fatalf("AbandonBranch: %v", err)
	}

	// New branch on same run sees Graveyard entry automatically in Layer 1.
	b3 := store.Branch{ID: "b_sibling", RunID: runID, Label: "sibling", Epoch: 1, Status: string(StatusQueued)}
	if err := st.CreateBranch(ctx, b3); err != nil {
		t.Fatalf("CreateBranch sibling: %v", err)
	}
	var sawGraveyard bool
	exec.Provider = &scriptedProvider{
		fn: func(turn int, req GenerateRequest) (*GenerateResponse, error) {
			if strings.Contains(req.Frame.Layer1, "caused deadlock in callback") {
				sawGraveyard = true
			}
			return &GenerateResponse{Done: true, FinalSummary: "ok", StateHash: "sib_done"}, nil
		},
	}
	if _, err := exec.ExecuteBranch(ctx, b3.ID, 5); err != nil {
		t.Fatalf("ExecuteBranch sibling: %v", err)
	}
	if !sawGraveyard {
		t.Fatal("expected sibling branch Layer 1 to automatically include abandoned branch Graveyard entry")
	}
}

func TestCrashRecoveryIdempotentAndIrreversibleNeverAutoRetries(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	somaDir := filepath.Join(ws, ".soma")
	st, err := store.Open(somaDir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	setupAccount(t, st, "acc1")

	tools := &recordingToolRunner{}

	// Case A: idempotent_write crashes after dispatch -> re-dispatches with identical idempotency_key (1 logical write).
	provIdem := &scriptedProvider{
		fn: func(turn int, _ GenerateRequest) (*GenerateResponse, error) {
			if turn == 1 {
				return &GenerateResponse{
					Reasoning: "upsert remote ticket",
					ToolCall:  &ToolCallSpec{Name: "upsert_ticket", ArgsJSON: `{"id":1}`, EffectClass: "idempotent_write"},
					StateHash: "t1",
				}, nil
			}
			return &GenerateResponse{Done: true, FinalSummary: "done idem", StateHash: "done"}, nil
		},
	}
	exec := NewExecutor(st, provIdem, tools, compiler.WorkerProfile())
	exec.AfterDispatchHook = func(_ context.Context, _ store.ActionJournalEntry) error {
		return ErrSimulatedCrash
	}
	_, bIdem, err := exec.StartRun(ctx, RunRequest{WorkspacePath: ws, GoalSpec: "idem test", AccountIDs: []string{"acc1"}})
	if err != nil {
		t.Fatalf("StartRun idem: %v", err)
	}
	if _, err := exec.ExecuteBranch(ctx, bIdem, 5); !errors.Is(err, ErrSimulatedCrash) {
		t.Fatalf("expected ErrSimulatedCrash, got %v", err)
	}
	_ = st.Close()

	// Reopen DB from disk (simulating process restart) without crash hook.
	st2, err := store.Open(somaDir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer func() { _ = st2.Close() }()
	exec2 := NewExecutor(st2, provIdem, tools, compiler.WorkerProfile())
	exec2.AccountIDs = []string{"acc1"}

	resIdem, err := exec2.ExecuteBranch(ctx, bIdem, 5)
	if err != nil || resIdem.Status != StatusCompleted {
		t.Fatalf("expected clean completion on resume, got res=%+v err=%v", resIdem, err)
	}
	if tools.remoteWrites.Load() != 1 {
		t.Fatalf("expected 1 logical idempotent write across crash+resume, got %d", tools.remoteWrites.Load())
	}

	// Case B: irreversible_write crashes after dispatch -> NEVER auto-retries, suspends to SUSPENDED_RECONCILE.
	tools.remoteWrites.Store(0)
	provIrrev := &scriptedProvider{
		fn: func(turn int, _ GenerateRequest) (*GenerateResponse, error) {
			if turn == 1 {
				return &GenerateResponse{
					Reasoning: "deploy prod",
					ToolCall:  &ToolCallSpec{Name: "deploy_prod", ArgsJSON: `{"tag":"v1"}`, EffectClass: taint.EffectIrreversible},
					StateHash: "dep1",
				}, nil
			}
			return &GenerateResponse{Done: true, FinalSummary: "after reconcile", StateHash: "done_rec"}, nil
		},
	}
	// Pre-seed an already-dispatched irreversible_write in action_journal to simulate a direct crash after approval+dispatch.
	exec2.Provider = provIrrev
	runIrrev, bIrrev, err := exec2.StartRun(ctx, RunRequest{WorkspacePath: ws, GoalSpec: "irrev test", AccountIDs: []string{"acc1"}})
	if err != nil {
		t.Fatalf("StartRun irrev: %v", err)
	}
	if err := st2.PrepareAction(ctx, store.ActionJournalEntry{
		ActionID:       "act_irrev_crash",
		RunID:          runIrrev,
		BranchID:       bIrrev,
		Epoch:          1,
		TurnIndex:      1,
		ToolName:       "deploy_prod",
		ArgsJSON:       `{"tag":"v1"}`,
		ArgsHash:       "h1",
		EffectClass:    string(taint.EffectIrreversible),
		IdempotencyKey: "idem_irrev",
		Status:         "dispatched",
	}); err != nil {
		t.Fatalf("PrepareAction irrev: %v", err)
	}
	// Pretend dispatch happened once before crash.
	tools.remoteWrites.Add(1)

	_, err = exec2.ExecuteBranch(ctx, bIrrev, 5)
	var recErr *SuspendedReconcileError
	if !errors.As(err, &recErr) || recErr.ActionID != "act_irrev_crash" {
		t.Fatalf("expected SuspendedReconcileError for act_irrev_crash, got %v", err)
	}
	if tools.remoteWrites.Load() != 1 {
		t.Fatalf("irreversible action MUST NOT auto-retry! remoteWrites=%d", tools.remoteWrites.Load())
	}

	// Repeated resume before reconciliation stays suspended with 0 retries.
	if _, err := exec2.ExecuteBranch(ctx, bIrrev, 5); !errors.As(err, &recErr) {
		t.Fatalf("expected continued SuspendedReconcileError before reconcile, got %v", err)
	}

	// Operator resolves via ReconcileAction, then resume finishes cleanly.
	if err := exec2.ReconcileAction(ctx, "act_irrev_crash", OutcomeSucceeded, "verified deploy v1 live"); err != nil {
		t.Fatalf("ReconcileAction: %v", err)
	}
	resFin, err := exec2.ExecuteBranch(ctx, bIrrev, 5)
	if err != nil || resFin.Status != StatusCompleted {
		t.Fatalf("expected completion after reconciliation, got res=%+v err=%v", resFin, err)
	}
	if tools.remoteWrites.Load() != 1 {
		t.Fatalf("expected total remote writes == 1, got %d", tools.remoteWrites.Load())
	}
}

type failoverTestProvider struct {
	primaryLimitTurn int
	usedAccounts     []string
}

func (p *failoverTestProvider) Generate(_ context.Context, req GenerateRequest) (*GenerateResponse, error) {
	p.usedAccounts = append(p.usedAccounts, req.AccountID)
	if req.AccountID == "primary" && req.TurnIndex == p.primaryLimitTurn {
		return nil, &RateLimitError{
			AccountID:  "primary",
			RetryAfter: 50 * time.Millisecond,
			Err:        errors.New("429 primary window exhausted"),
		}
	}
	if req.TurnIndex == 1 {
		return &GenerateResponse{
			Reasoning: "read repo context",
			ToolCall: &ToolCallSpec{
				Name:        "inspect",
				ArgsJSON:    `{}`,
				EffectClass: taint.EffectReadOnly,
			},
			StateHash: "s1",
		}, nil
	}
	if req.TurnIndex == 2 {
		if req.AccountID == "fallback" && !strings.Contains(req.Frame.Layer2, "handoff") {
			return nil, errors.New("expected Epoch 2 handoff packet in Layer2 after failover")
		}
		return &GenerateResponse{
			Reasoning: "continue on epoch 2",
			ToolCall: &ToolCallSpec{
				Name:        "patch",
				ArgsJSON:    `{}`,
				EffectClass: taint.EffectReadOnly,
			},
			StateHash: "s2",
		}, nil
	}
	return &GenerateResponse{
		Reasoning:    "finished cleanly",
		Done:         true,
		FinalSummary: "ok",
		StateHash:    "done",
	}, nil
}

func TestExecutor_MultiSubscriptionFailoverCleanEpoch(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), ".soma"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	for _, acc := range []store.Account{
		{ID: "primary", Provider: "openai-codex", ModelID: "gpt-6.2-sol", BillingMode: "subscription", MaxConcurrent: 2},
		{ID: "fallback", Provider: "xai-oauth", ModelID: "grok-4.6", BillingMode: "subscription", MaxConcurrent: 2},
	} {
		if err := st.UpsertAccount(ctx, acc); err != nil {
			t.Fatalf("UpsertAccount %s: %v", acc.ID, err)
		}
	}

	ws := t.TempDir()
	prov := &failoverTestProvider{primaryLimitTurn: 2}
	exec := NewExecutor(st, prov, &recordingToolRunner{}, compiler.WorkerProfile())
	exec.AllowFailover = true
	exec.AccountIDs = []string{"primary", "fallback"}

	_, b1, err := exec.StartRun(ctx, RunRequest{
		WorkspacePath: ws,
		GoalSpec:      "survive primary 429 cleanly",
		AccountIDs:    []string{"primary", "fallback"},
	})
	if err != nil {
		t.Fatalf("StartRun b1: %v", err)
	}

	res1, err := exec.ExecuteBranch(ctx, b1, 10)
	if err != nil || res1.Status != StatusCompleted {
		t.Fatalf("ExecuteBranch b1 failed: res=%+v err=%v", res1, err)
	}
	if res1.Rollovers < 1 {
		t.Fatalf("expected clean epoch rollover on failover, got Rollovers=%d", res1.Rollovers)
	}

	steps1, err := st.ListBranchSteps(ctx, b1)
	if err != nil || len(steps1) != 3 {
		t.Fatalf("expected 3 steps on b1, got %d err=%v", len(steps1), err)
	}
	if steps1[0].Epoch != 1 || steps1[0].AccountID != "primary" {
		t.Fatalf("expected step 1 on epoch 1 primary, got epoch=%d acc=%s", steps1[0].Epoch, steps1[0].AccountID)
	}
	if steps1[1].Epoch != 2 || steps1[1].AccountID != "fallback" {
		t.Fatalf("expected step 2 on epoch 2 fallback, got epoch=%d acc=%s", steps1[1].Epoch, steps1[1].AccountID)
	}
	// Verify SHA-256 bit-for-bit frame reconstruction on the Epoch 2 failover step.
	frame2, err := compiler.ReconstructFrame(ctx, st, steps1[1].ID, compiler.WorkerProfile())
	if err != nil {
		t.Fatalf("ReconstructFrame epoch 2 failover step: %v", err)
	}
	if !strings.Contains(frame2.Layer2, "handoff account=\"fallback\"") {
		t.Fatalf("expected handoff packet in reconstructed Epoch 2 frame, got:\n%s", frame2.Layer2)
	}

	// Ensure AccountIDs ordering was NOT mutated and primary preference returns once cooldown expires.
	if exec.AccountIDs[0] != "primary" {
		t.Fatalf("AccountIDs order mutated: %v", exec.AccountIDs)
	}
	time.Sleep(65 * time.Millisecond)

	prov2 := &failoverTestProvider{primaryLimitTurn: 99}
	exec2 := NewExecutor(st, prov2, &recordingToolRunner{}, compiler.WorkerProfile())
	exec2.AllowFailover = true
	exec2.AccountIDs = []string{"primary", "fallback"}
	_, b2, err := exec2.StartRun(ctx, RunRequest{
		WorkspacePath: ws,
		GoalSpec:      "primary recovers after cooldown",
		AccountIDs:    []string{"primary", "fallback"},
	})
	if err != nil {
		t.Fatalf("StartRun b2: %v", err)
	}
	res2, err := exec2.ExecuteBranch(ctx, b2, 10)
	if err != nil || res2.Status != StatusCompleted {
		t.Fatalf("ExecuteBranch b2 failed: res=%+v err=%v", res2, err)
	}
	steps2, err := st.ListBranchSteps(ctx, b2)
	if err != nil || len(steps2) != 3 {
		t.Fatalf("expected 3 steps on b2, got %d err=%v", len(steps2), err)
	}
	for i, s := range steps2 {
		if s.AccountID != "primary" {
			t.Fatalf("expected primary preference restored on step %d, got %s", i, s.AccountID)
		}
	}
}
