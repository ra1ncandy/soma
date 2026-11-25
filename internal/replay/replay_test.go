package replay

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/ra1ncandy/soma/internal/compiler"
	"github.com/ra1ncandy/soma/internal/dag"
	"github.com/ra1ncandy/soma/internal/store"
	"github.com/ra1ncandy/soma/internal/taint"
)

type replayMockProvider struct {
	mode string // "same" | "diverge_turn2" | "sandbox_alt"
}

func (p *replayMockProvider) Generate(_ context.Context, req dag.GenerateRequest) (*dag.GenerateResponse, error) {
	switch req.TurnIndex {
	case 1:
		return &dag.GenerateResponse{
			Reasoning:   "read README",
			ToolCall:    &dag.ToolCallSpec{Name: "fs_read", ArgsJSON: `{"path":"README.md"}`, EffectClass: taint.EffectReadOnly},
			StateHash:   "t1",
			InputTokens: 100, CacheReadTokens: 800, OutputTokens: 20,
		}, nil
	case 2:
		if p.mode == "diverge_turn2" {
			return &dag.GenerateResponse{
				Reasoning:   "different tool",
				ToolCall:    &dag.ToolCallSpec{Name: "fs_write", ArgsJSON: `{"path":"other.txt","content":"alt"}`, EffectClass: taint.EffectWorkspaceWrite},
				StateHash:   "t2_alt",
				InputTokens: 110, CacheReadTokens: 900, OutputTokens: 25,
			}, nil
		}
		if p.mode == "sandbox_alt" {
			return &dag.GenerateResponse{
				Reasoning:   "write sandbox file",
				ToolCall:    &dag.ToolCallSpec{Name: "fs_write", ArgsJSON: `{"path":"sandbox.txt","content":"ok"}`, EffectClass: taint.EffectWorkspaceWrite},
				StateHash:   "t2_sbx",
				InputTokens: 110, CacheReadTokens: 900, OutputTokens: 25,
			}, nil
		}
		return &dag.GenerateResponse{
			Reasoning:   "write output",
			ToolCall:    &dag.ToolCallSpec{Name: "fs_write", ArgsJSON: `{"path":"out.txt","content":"ok"}`, EffectClass: taint.EffectWorkspaceWrite},
			StateHash:   "t2",
			InputTokens: 120, CacheReadTokens: 950, OutputTokens: 30,
		}, nil
	default:
		return &dag.GenerateResponse{
			Reasoning:    "complete",
			Done:         true,
			FinalSummary: "all done (" + p.mode + ")",
			StateHash:    "done_" + p.mode,
			InputTokens:  50, CacheReadTokens: 1000, OutputTokens: 15,
		}, nil
	}
}

type countingRunner struct {
	inner dag.ToolRunner
	calls atomic.Int64
}

func (c *countingRunner) Execute(ctx context.Context, call dag.ToolCall) (dag.ToolOutput, error) {
	c.calls.Add(1)
	return c.inner.Execute(ctx, call)
}

func TestThreeReplayModes(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	st, err := store.Open(filepath.Join(ws, ".soma"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	if err := st.UpsertAccount(ctx, store.Account{
		ID: "acc_main", Provider: "local", ModelID: "orig-model", BillingMode: "local", MaxConcurrent: 2,
	}); err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}

	exec := dag.NewExecutor(st, &replayMockProvider{mode: "same"}, nil, compiler.WorkerProfile())
	_, branchID, err := exec.StartRun(ctx, dag.RunRequest{
		WorkspacePath: ws,
		GoalSpec:      "test 3 replay modes",
		AccountIDs:    []string{"acc_main"},
	})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	// Seed README.md inside isolated workspace for fs_read.
	br, _ := st.GetBranch(ctx, branchID)
	if err := os.WriteFile(filepath.Join(br.WorkspaceDir, "README.md"), []byte("hello soma\n"), 0o644); err != nil {
		t.Fatalf("seed README.md: %v", err)
	}

	res, err := exec.ExecuteBranch(ctx, branchID, 10)
	if err != nil || res.Status != dag.StatusCompleted {
		t.Fatalf("ExecuteBranch: res=%+v err=%v", res, err)
	}

	replayer := NewReplayer(st)

	// 1. ModeExact: 0 provider calls, 0 tool calls, 100% SHA-256 verification.
	exactRep, err := replayer.Replay(ctx, branchID, ReplayOptions{Mode: ModeExact})
	if err != nil {
		t.Fatalf("ModeExact failed: %v", err)
	}
	if exactRep.StepsChecked != 3 || exactRep.HashVerifiedCount != 3 || exactRep.Diverged {
		t.Fatalf("unexpected exact report: %+v", exactRep)
	}

	// 2. ModeCounterfactual: matches turn 1, diverges at turn 2, halts cleanly without running tools.
	cfRunner := &countingRunner{inner: exec.Tools}
	cfRep, err := replayer.Replay(ctx, branchID, ReplayOptions{
		Mode:          ModeCounterfactual,
		OverrideModel: "alt-model",
		Provider:      &replayMockProvider{mode: "diverge_turn2"},
		Tools:         cfRunner,
	})
	if err != nil {
		t.Fatalf("ModeCounterfactual failed: %v", err)
	}
	if cfRunner.calls.Load() != 0 {
		t.Fatalf("counterfactual mode MUST execute 0 live tools, got %d", cfRunner.calls.Load())
	}
	if !cfRep.Diverged || cfRep.StepsChecked != 2 || cfRep.Comparisons[0].Diverged || !cfRep.Comparisons[1].Diverged {
		t.Fatalf("expected halt at turn 2 divergence, got %+v", cfRep)
	}

	// 3. ModeSandboxBranch: forks from step 1 into a new isolated workspace and live-executes.
	steps, err := st.ListBranchSteps(ctx, branchID)
	if err != nil {
		t.Fatalf("ListBranchSteps: %v", err)
	}
	cr := &countingRunner{inner: exec.Tools}
	sbRep, err := replayer.Replay(ctx, branchID, ReplayOptions{
		Mode:          ModeSandboxBranch,
		FromStepID:    steps[0].ID,
		OverrideModel: "sandbox-model",
		Provider:      &replayMockProvider{mode: "sandbox_alt"},
		Tools:         cr,
	})
	if err != nil {
		t.Fatalf("ModeSandboxBranch failed: %v", err)
	}
	if sbRep.SandboxBranchID == "" || sbRep.SandboxBranchID == branchID {
		t.Fatalf("expected distinct SandboxBranchID, got %q", sbRep.SandboxBranchID)
	}
	if cr.calls.Load() == 0 {
		t.Fatal("expected sandbox_branch mode to execute tools live inside forked workspace")
	}
	sbBranch, err := st.GetBranch(ctx, sbRep.SandboxBranchID)
	if err != nil || sbBranch.WorkspaceDir == br.WorkspaceDir {
		t.Fatalf("expected isolated workspace dir for sandbox branch, orig=%s new=%s err=%v", br.WorkspaceDir, sbBranch.WorkspaceDir, err)
	}

	// 4. Abandon the diverged sandbox branch to the Graveyard and verify ExportSFT + Contrastive ExportDPO!
	if err := exec.AbandonBranch(ctx, sbRep.SandboxBranchID, "failed held-out dilation check", ""); err != nil {
		t.Fatalf("AbandonBranch: %v", err)
	}

	sftList, err := replayer.ExportSFT(ctx, ExportOptions{VerifiedOnly: true})
	if err != nil {
		t.Fatalf("ExportSFT: %v", err)
	}
	if len(sftList) != 1 || sftList[0].BranchID != branchID || len(sftList[0].Messages) < 4 {
		t.Fatalf("expected exactly 1 completed SFT trajectory (excluding abandoned branch), got %+v", sftList)
	}

	dpoPairs, err := replayer.ExportDPO(ctx, ExportOptions{VerifiedOnly: true})
	if err != nil {
		t.Fatalf("ExportDPO: %v", err)
	}
	if len(dpoPairs) != 1 {
		t.Fatalf("expected 1 contrastive DPO preference pair (chosen=%s vs rejected=%s), got %+v", branchID, sbRep.SandboxBranchID, dpoPairs)
	}
	pair := dpoPairs[0]
	if pair.ChosenBranchID != branchID || pair.RejectedBranchID != sbRep.SandboxBranchID || pair.GraveyardReason == "" {
		t.Fatalf("unexpected DPO pair metadata: %+v", pair)
	}
	jsonlBytes, err := EncodeJSONL(dpoPairs)
	if err != nil || len(jsonlBytes) == 0 {
		t.Fatalf("EncodeJSONL failed: len=%d err=%v", len(jsonlBytes), err)
	}
}
