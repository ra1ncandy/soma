package benchmarks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ra1ncandy/soma/internal/compiler"
	"github.com/ra1ncandy/soma/internal/dag"
	"github.com/ra1ncandy/soma/internal/store"
	"github.com/ra1ncandy/soma/internal/taint"
)

func storeReadSize(p string) (int64, error) {
	info, err := os.Stat(p)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

type chaosProvider struct {
	calls atomic.Int64
}

func (p *chaosProvider) Generate(_ context.Context, req dag.GenerateRequest) (*dag.GenerateResponse, error) {
	n := p.calls.Add(1)
	// Inject HTTP 429 every 4th API call.
	if n%4 == 0 {
		return nil, &dag.RateLimitError{
			AccountID:  req.AccountID,
			RetryAfter: 25 * time.Millisecond,
			Err:        errors.New("HTTP 429 synthetic quota storm"),
		}
	}
	if req.TurnIndex < 4 {
		return &dag.GenerateResponse{
			Reasoning: fmt.Sprintf("Chaos step %d", req.TurnIndex),
			ToolCall: &dag.ToolCallSpec{
				Name:        "work_step",
				ArgsJSON:    fmt.Sprintf(`{"turn":%d}`, req.TurnIndex),
				EffectClass: taint.EffectReadOnly,
			},
			StateHash:       fmt.Sprintf("ch_%s_%d", req.BranchID, req.TurnIndex),
			InputTokens:     200,
			CacheReadTokens: 1800,
			OutputTokens:    40,
			LatencyMS:       5,
		}, nil
	}
	return &dag.GenerateResponse{
		Reasoning:       "Completed chaos run cleanly.",
		Done:            true,
		FinalSummary:    "chaos run passed",
		StateHash:       "ch_done_" + req.BranchID,
		InputTokens:     120,
		CacheReadTokens: 1900,
		OutputTokens:    30,
		LatencyMS:       5,
	}, nil
}

type chaosToolRunner struct {
	remoteLedgerMu sync.Mutex
	remoteOps      map[string]int
	irrevCount     atomic.Int64
}

func (r *chaosToolRunner) Execute(_ context.Context, call dag.ToolCall) (dag.ToolOutput, error) {
	switch call.EffectClass {
	case "idempotent_write":
		r.remoteLedgerMu.Lock()
		if r.remoteOps == nil {
			r.remoteOps = make(map[string]int)
		}
		r.remoteOps[call.IdempotencyKey]++
		r.remoteLedgerMu.Unlock()
		return dag.ToolOutput{
			NameHint:  "idem.json",
			Content:   []byte(`{"ok":true,"deduped":true}`),
			TrustTier: "trusted_repo",
		}, nil
	case taint.EffectIrreversible:
		r.irrevCount.Add(1)
		return dag.ToolOutput{
			NameHint:  "irrev.json",
			Content:   []byte(`{"deployed":true}`),
			TrustTier: "trusted_repo",
		}, nil
	default:
		return dag.ToolOutput{
			NameHint:  "step.log",
			Content:   []byte("ok"),
			TrustTier: "trusted_repo",
		}, nil
	}
}

func TestB2_RateLimitChaosAndSafeCrashRecovery(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	somaDir := filepath.Join(ws, ".soma")
	st, err := store.Open(somaDir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}

	// Register 4 subscription accounts (max_concurrent=2 each).
	for i := 1; i <= 4; i++ {
		if err := st.UpsertAccount(ctx, store.Account{
			ID:            fmt.Sprintf("sub_%d", i),
			Provider:      "subscription",
			ModelID:       "gpt-6.2-sol",
			BillingMode:   "subscription",
			MaxConcurrent: 2,
		}); err != nil {
			t.Fatalf("UpsertAccount: %v", err)
		}
	}

	// Part 1: 20 concurrent tasks surviving 429 storms and forced mid-run restarts.
	const numTasks = 20
	prov := &chaosProvider{}
	tools := &chaosToolRunner{}

	type taskRef struct {
		runID    string
		branchID string
	}
	refs := make([]taskRef, numTasks)
	initExec := dag.NewExecutor(st, prov, tools, compiler.WorkerProfile())
	for i := 0; i < numTasks; i++ {
		rID, bID, err := initExec.StartRun(ctx, dag.RunRequest{
			WorkspacePath: ws,
			GoalSpec:      fmt.Sprintf("Chaos task %d", i),
		})
		if err != nil {
			t.Fatalf("StartRun %d: %v", i, err)
		}
		refs[i] = taskRef{runID: rID, branchID: bID}
	}

	var wg sync.WaitGroup
	errCh := make(chan error, numTasks)
	for i := 0; i < numTasks; i++ {
		wg.Add(1)
		go func(idx int, ref taskRef) {
			defer wg.Done()
			exec := dag.NewExecutor(st, prov, tools, compiler.WorkerProfile())
			// Force odd tasks to simulate mid-run process interruption after 2 turns, then resume.
			if idx%2 == 1 {
				_, _ = exec.ExecuteBranch(ctx, ref.branchID, 2)
			}
			for attempt := 0; attempt < 20; attempt++ {
				res, err := exec.ExecuteBranch(ctx, ref.branchID, 10)
				if err == nil && res.Status == dag.StatusCompleted {
					return
				}
				var sq *dag.SuspendedQuotaError
				if errors.As(err, &sq) || errors.Is(err, store.ErrNoCapacity) {
					time.Sleep(30 * time.Millisecond)
					continue
				}
				if err != nil {
					errCh <- fmt.Errorf("task %d unexpected error: %w", idx, err)
					return
				}
			}
			errCh <- fmt.Errorf("task %d did not complete within retry window", idx)
		}(i, refs[i])
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}

	for i, ref := range refs {
		br, err := st.GetBranch(ctx, ref.branchID)
		if err != nil {
			t.Fatalf("GetBranch %d: %v", i, err)
		}
		if br.Status != string(dag.StatusCompleted) {
			t.Fatalf("task %d not completed: %s", i, br.Status)
		}
		anc, err := st.GetAncestry(ctx, br.HeadStepID)
		if err != nil || len(anc) != 4 {
			t.Fatalf("task %d expected 4 intact Merkle steps, got %d err=%v", i, len(anc), err)
		}
	}

	// Part 2: Crash-Between-Dispatch-And-Record Matrix across read_only, idempotent_write, irreversible_write.
	// A) read_only crash
	roTools := &chaosToolRunner{}
	roProv := &scriptedB2Provider{effect: taint.EffectReadOnly}
	roExec := dag.NewExecutor(st, roProv, roTools, compiler.WorkerProfile())
	roExec.AfterDispatchHook = func(_ context.Context, _ store.ActionJournalEntry) error { return dag.ErrSimulatedCrash }
	_, bRO, err := roExec.StartRun(ctx, dag.RunRequest{WorkspacePath: ws, GoalSpec: "crash read_only"})
	if err != nil {
		t.Fatalf("StartRun RO: %v", err)
	}
	if _, err := roExec.ExecuteBranch(ctx, bRO, 5); !errors.Is(err, dag.ErrSimulatedCrash) {
		t.Fatalf("expected crash on RO, got %v", err)
	}
	roExec.AfterDispatchHook = nil
	resRO, err := roExec.ExecuteBranch(ctx, bRO, 5)
	if err != nil || resRO.Status != dag.StatusCompleted {
		t.Fatalf("RO crash recovery failed: res=%+v err=%v", resRO, err)
	}

	// B) idempotent_write crash -> same idempotency_key re-dispatched, store closed and reopened cleanly!
	idemTools := &chaosToolRunner{}
	idemProv := &scriptedB2Provider{effect: "idempotent_write"}
	idemExec := dag.NewExecutor(st, idemProv, idemTools, compiler.WorkerProfile())
	idemExec.AfterDispatchHook = func(_ context.Context, _ store.ActionJournalEntry) error { return dag.ErrSimulatedCrash }
	_, bIdem, err := idemExec.StartRun(ctx, dag.RunRequest{WorkspacePath: ws, GoalSpec: "crash idempotent_write"})
	if err != nil {
		t.Fatalf("StartRun Idem: %v", err)
	}
	if _, err := idemExec.ExecuteBranch(ctx, bIdem, 5); !errors.Is(err, dag.ErrSimulatedCrash) {
		t.Fatalf("expected crash on Idem, got %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close DB for restart simulation: %v", err)
	}

	stReopen, err := store.Open(somaDir)
	if err != nil {
		t.Fatalf("reopen DB: %v", err)
	}
	defer func() { _ = stReopen.Close() }()

	idemExec2 := dag.NewExecutor(stReopen, idemProv, idemTools, compiler.WorkerProfile())
	resIdem, err := idemExec2.ExecuteBranch(ctx, bIdem, 5)
	if err != nil || resIdem.Status != dag.StatusCompleted {
		t.Fatalf("Idempotent crash recovery failed: res=%+v err=%v", resIdem, err)
	}
	if len(idemTools.remoteOps) != 1 {
		t.Fatalf("expected exactly 1 unique idempotency_key on remote, got %d (%+v)", len(idemTools.remoteOps), idemTools.remoteOps)
	}

	// C) irreversible_write crash -> NEVER auto-retries! Suspends to SUSPENDED_RECONCILE.
	irrevTools := &chaosToolRunner{}
	irrevProv := &scriptedB2Provider{effect: taint.EffectIrreversible}
	irrevExec := dag.NewExecutor(stReopen, irrevProv, irrevTools, compiler.WorkerProfile())
	rIrrev, bIrrev, err := irrevExec.StartRun(ctx, dag.RunRequest{WorkspacePath: ws, GoalSpec: "crash irreversible_write"})
	if err != nil {
		t.Fatalf("StartRun Irrev: %v", err)
	}
	if err := stReopen.PrepareAction(ctx, store.ActionJournalEntry{
		ActionID:       "b2_irrev_act",
		RunID:          rIrrev,
		BranchID:       bIrrev,
		Epoch:          1,
		TurnIndex:      1,
		ToolName:       "prod_release",
		ArgsJSON:       `{"tag":"v2.1"}`,
		ArgsHash:       "h_irrev",
		EffectClass:    string(taint.EffectIrreversible),
		IdempotencyKey: "b2_idem_irrev",
		Status:         "dispatched",
	}); err != nil {
		t.Fatalf("PrepareAction irrev: %v", err)
	}
	irrevTools.irrevCount.Store(1) // Already executed once externally prior to crash.

	var recErr *dag.SuspendedReconcileError
	if _, err := irrevExec.ExecuteBranch(ctx, bIrrev, 5); !errors.As(err, &recErr) {
		t.Fatalf("expected SuspendedReconcileError, got %v", err)
	}
	if irrevTools.irrevCount.Load() != 1 {
		t.Fatalf("irreversible action auto-retried! count=%d", irrevTools.irrevCount.Load())
	}
	if err := irrevExec.ReconcileAction(ctx, "b2_irrev_act", dag.OutcomeSucceeded, "confirmed externally"); err != nil {
		t.Fatalf("ReconcileAction: %v", err)
	}
	resIrrev, err := irrevExec.ExecuteBranch(ctx, bIrrev, 5)
	if err != nil || resIrrev.Status != dag.StatusCompleted {
		t.Fatalf("expected completion after reconcile, got res=%+v err=%v", resIrrev, err)
	}
	if irrevTools.irrevCount.Load() != 1 {
		t.Fatalf("expected irreversible write count == 1 after full recovery, got %d", irrevTools.irrevCount.Load())
	}

	t.Logf("=== SUITE B2: 429 CHAOS & CRASH RECOVERY PASSED ===")
	t.Logf("Completed 20/20 chaos tasks (0 lost steps, 0 SQLITE_BUSY) + verified read_only / idempotent_write / irreversible_write safety.")
}

type scriptedB2Provider struct {
	effect taint.EffectClass
}

func (p *scriptedB2Provider) Generate(_ context.Context, req dag.GenerateRequest) (*dag.GenerateResponse, error) {
	if req.TurnIndex == 1 {
		return &dag.GenerateResponse{
			Reasoning: "run op",
			ToolCall: &dag.ToolCallSpec{
				Name:        "op_" + string(p.effect),
				ArgsJSON:    `{"n":1}`,
				EffectClass: p.effect,
			},
			StateHash: "op1",
		}, nil
	}
	return &dag.GenerateResponse{
		Reasoning:    "done",
		Done:         true,
		FinalSummary: "ok",
		StateHash:    "done",
	}, nil
}
