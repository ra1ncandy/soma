package benchmarks

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ra1ncandy/soma/internal/compiler"
	"github.com/ra1ncandy/soma/internal/dag"
	"github.com/ra1ncandy/soma/internal/oracle"
	"github.com/ra1ncandy/soma/internal/replay"
	"github.com/ra1ncandy/soma/internal/sandbox"
	"github.com/ra1ncandy/soma/internal/shadow"
	"github.com/ra1ncandy/soma/internal/store"
)

func TestB6_BranchingSearchAndShadowGauntlet(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	st, err := store.Open(filepath.Join(ws, ".soma"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	if err := st.UpsertAccount(ctx, store.Account{
		ID: "b6_worker", Provider: "local", ModelID: "worker-v1", BillingMode: "local", MaxConcurrent: 4,
	}); err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}

	// Part 1: Non-git ML R&D deceptive local optimum, scripted provider (single branch vs BranchSearcher + Graveyard backtracking).
	stalePath := filepath.Join(t.TempDir(), "stale_lala.json")
	_ = os.WriteFile(stalePath, []byte(`{"model":"stale_prev"}`), 0o644)
	staleSHA, _ := oracle.HashFileSHA256(stalePath)

	// 1a) Linear baseline without branching gets trapped chasing eval_dev and fails frozen Track D/E gates.
	linExec := dag.NewExecutor(st, shadow.NewLalaResearchProvider(), nil, compiler.WorkerProfile())
	linExec.Verifier = oracle.NewVerifier(st, sandbox.NewManager())
	linRunID, linBrID, err := linExec.StartRun(ctx, dag.RunRequest{
		Title:         "b6-linear-trap",
		WorkspacePath: ws,
		WorkspaceKind: "research",
		GoalSpec:      "Beat Lala-l1 open_unseen_rt >= 0.97 + Track D/E",
		AccountIDs:    []string{"b6_worker"},
	})
	if err != nil {
		t.Fatalf("StartRun linear: %v", err)
	}
	if err := shadow.SeedLalaResearchContracts(ctx, st, linRunID, []string{staleSHA}); err != nil {
		t.Fatalf("SeedLalaResearchContracts linear: %v", err)
	}
	linRes, linErr := linExec.ExecuteBranch(ctx, linBrID, 5)
	var vfErr *dag.VerificationFailedError
	if !errors.As(linErr, &vfErr) || linRes.Status != dag.StatusFailed {
		t.Fatalf("expected linear baseline to fail frozen Track D/E gate, got res=%+v err=%v", linRes, linErr)
	}

	// 1b) soma BranchSearcher escapes via Graveyard backtracking across isolated copy_snapshot workspaces.
	prof := compiler.WorkerProfile()
	prof.MaxStalls = 2
	searchExec := dag.NewExecutor(st, shadow.NewLalaResearchProvider(), nil, prof)
	searchExec.Verifier = oracle.NewVerifier(st, sandbox.NewManager())
	sRunID, sRootBrID, err := searchExec.StartRun(ctx, dag.RunRequest{
		Title:         "b6-branching-search",
		WorkspacePath: ws,
		WorkspaceKind: "research",
		GoalSpec:      "Beat Lala-l1 open_unseen_rt >= 0.97 + Track D/E via BranchSearcher",
		AccountIDs:    []string{"b6_worker"},
	})
	if err != nil {
		t.Fatalf("StartRun search: %v", err)
	}
	if err := shadow.SeedLalaResearchContracts(ctx, st, sRunID, []string{staleSHA}); err != nil {
		t.Fatalf("SeedLalaResearchContracts search: %v", err)
	}

	searcher := dag.NewBranchSearcher(searchExec)
	sRes, err := searcher.SearchExistingRun(ctx, sRunID, sRootBrID, dag.SearchConfig{
		MaxBranches:       4,
		MaxTurnsPerBranch: 4,
		AnchorTurn:        1,
	})
	if err != nil || !sRes.Completed || len(sRes.Attempts) != 3 || sRes.GraveyardCount != 2 {
		t.Fatalf("BranchSearcher expected 3 attempts / 2 graveyard / completion, got %+v err=%v", sRes, err)
	}

	// Verify per-branch copy_snapshot workspace isolation without cross-contamination.
	expectedModels := []string{"burst-long-L1024", "ablation-dilated", "lala-l1.1-causal-dilated-v2"}
	wsSeen := make(map[string]bool)
	replayer := replay.NewReplayer(st)
	for i, att := range sRes.Attempts {
		br, bErr := st.GetBranch(ctx, att.BranchID)
		if bErr != nil || br.WorkspaceStrategy != "copy_snapshot" {
			t.Fatalf("attempt %d expected copy_snapshot workspace, got %+v err=%v", i, br, bErr)
		}
		if wsSeen[br.WorkspaceDir] {
			t.Fatalf("workspace collision across branch %d: %s", i, br.WorkspaceDir)
		}
		wsSeen[br.WorkspaceDir] = true

		artRaw, rErr := os.ReadFile(filepath.Join(br.WorkspaceDir, "artifacts", "metrics.json"))
		if rErr != nil || !strings.Contains(string(artRaw), expectedModels[i]) {
			t.Fatalf("branch %d expected model %s in isolated workspace, got %q err=%v", i, expectedModels[i], string(artRaw), rErr)
		}

		rep, repErr := replayer.Replay(ctx, att.BranchID, replay.ReplayOptions{Mode: replay.ModeExact})
		if repErr != nil || rep.Diverged || rep.StepsChecked == 0 || rep.HashVerifiedCount != rep.StepsChecked {
			t.Fatalf("branch %d exact replay failed: %+v err=%v", i, rep, repErr)
		}
	}

	// Part 2: 4-scenario conformance gauntlet. Each scenario replays one script with its kernel
	// mechanism enabled and disabled, and both outcomes are checked against ground truth.
	runner := shadow.NewRunner(st, sandbox.NewManager(), "v0.4.0")
	rep, err := runner.RunGauntlet(ctx, nil)
	if err != nil {
		t.Fatalf("RunGauntlet: %v", err)
	}
	sc := rep.Scorecard
	if !sc.GatePassed {
		t.Fatalf("Suite B6 scorecard gate failed: %+v", sc)
	}
	if sc.KernelVerified != sc.Tasks || sc.MechanismsLoadBearing != sc.Tasks || sc.ExactReplayPassRate != 100.0 {
		t.Fatalf("unexpected conformance metrics: %+v", sc)
	}

	t.Logf("=== SUITE B6: BRANCHING SEARCH & SHADOW GAUNTLET PASSED ===")
	t.Logf("Part 1 Lala-l1.1 Trap: Linear=%s vs BranchSearcher=%v (branches=%d, graveyard=%d)",
		linRes.Status, sRes.Completed, len(sRes.Attempts), sRes.GraveyardCount)
	t.Logf("Part 2 Conformance: kernel_verified=%d/%d ablation_verified=%d/%d load_bearing=%d/%d exact_replay=%.0f%%",
		sc.KernelVerified, sc.Tasks, sc.AblationVerified, sc.Tasks, sc.MechanismsLoadBearing, sc.Tasks, sc.ExactReplayPassRate)
}
