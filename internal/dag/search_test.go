package dag_test

import (
	"context"
	"encoding/json"
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
	"github.com/ra1ncandy/soma/internal/store"
	"github.com/ra1ncandy/soma/internal/taint"
)

type stallingProvider struct {
	turns int
}

func (p *stallingProvider) Generate(_ context.Context, _ dag.GenerateRequest) (*dag.GenerateResponse, error) {
	p.turns++
	return &dag.GenerateResponse{
		Reasoning: "repeating identical zero-progress command",
		ToolCall: &dag.ToolCallSpec{
			Name:        "noop_probe",
			ArgsJSON:    `{}`,
			EffectClass: taint.EffectReadOnly,
		},
		StateHash: "identical_loop_hash",
	}, nil
}

type guidedSearchProvider struct {
	branchSeenGraveyard map[string]int
}

func (p *guidedSearchProvider) Generate(_ context.Context, req dag.GenerateRequest) (*dag.GenerateResponse, error) {
	if p.branchSeenGraveyard == nil {
		p.branchSeenGraveyard = make(map[string]int)
	}
	sawBranch1 := strings.Contains(req.Frame.Layer1, `\"main\"`) || strings.Contains(req.Frame.Layer1, `"main"`)
	sawBranch2 := strings.Contains(req.Frame.Layer1, `branch-2`)
	graveCount := 0
	if sawBranch1 {
		graveCount++
	}
	if sawBranch2 {
		graveCount++
	}
	p.branchSeenGraveyard[req.BranchID] = graveCount

	// Branch 1 ("main"): Turn 1 seeds baseline config, Turns 2-4 stall repeating identical StateHash.
	if !sawBranch1 {
		if req.TurnIndex == 1 {
			return &dag.GenerateResponse{
				Reasoning: "seed baseline workspace config",
				ToolCall: &dag.ToolCallSpec{
					Name:        "fs_write",
					ArgsJSON:    `{"path":"status.txt","content":"setup_ready"}`,
					EffectClass: taint.EffectWorkspaceWrite,
				},
				StateHash: "anchor_setup",
			}, nil
		}
		return &dag.GenerateResponse{
			Reasoning: "stuck hill-climbing on failed hyperparameter",
			ToolCall: &dag.ToolCallSpec{
				Name:        "fs_write",
				ArgsJSON:    `{"path":"result.json","content":"{\"score\":0.10}"}`,
				EffectClass: taint.EffectWorkspaceWrite,
			},
			StateHash: "stalled_same_hash",
		}, nil
	}

	// Branch 2 (sees Branch 1 Graveyard entry): submits score=0.50 -> rejected by ArtifactOracle (>= 0.90).
	if !sawBranch2 {
		if req.TurnIndex <= 2 {
			return &dag.GenerateResponse{
				Reasoning: "try second hypothesis avoiding stalled path",
				ToolCall: &dag.ToolCallSpec{
					Name:        "fs_write",
					ArgsJSON:    `{"path":"result.json","content":"{\"score\":0.50}"}`,
					EffectClass: taint.EffectWorkspaceWrite,
				},
				StateHash: "hyp_2_write",
			}, nil
		}
		return &dag.GenerateResponse{
			Reasoning:    "submit hypothesis 2",
			Done:         true,
			FinalSummary: "hypothesis 2 submitted",
			StateHash:    "hyp_2_done",
		}, nil
	}

	// Branch 3 (sees both Graveyard entries in Layer 1): submits score=0.96 -> PASSES!
	if req.TurnIndex <= 2 {
		return &dag.GenerateResponse{
			Reasoning: "synthesize hypothesis 3 avoiding both graveyard failures",
			ToolCall: &dag.ToolCallSpec{
				Name:        "fs_write",
				ArgsJSON:    `{"path":"result.json","content":"{\"score\":0.96}"}`,
				EffectClass: taint.EffectWorkspaceWrite,
			},
			StateHash: "hyp_3_write",
		}, nil
	}
	return &dag.GenerateResponse{
		Reasoning:    "submit validated hypothesis 3",
		Done:         true,
		FinalSummary: "hypothesis 3 verified",
		StateHash:    "hyp_3_done",
	}, nil
}

func TestStallBreakerTripsAtMaxStalls(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	st, err := store.Open(filepath.Join(ws, ".soma"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	if err := st.UpsertAccount(ctx, store.Account{
		ID: "acc_stall", Provider: "local", ModelID: "worker", BillingMode: "local", MaxConcurrent: 2,
	}); err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}

	prof := compiler.WorkerProfile()
	prof.MaxStalls = 2
	exec := dag.NewExecutor(st, &stallingProvider{}, nil, prof)
	exec.EnforceStallBreaker = true

	_, brID, err := exec.StartRun(ctx, dag.RunRequest{
		WorkspacePath: ws,
		GoalSpec:      "trigger stall breaker",
		AccountIDs:    []string{"acc_stall"},
	})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	res, err := exec.ExecuteBranch(ctx, brID, 10)
	var sbErr *dag.StallBreakerError
	if !errors.As(err, &sbErr) {
		t.Fatalf("expected StallBreakerError, got res=%+v err=%v", res, err)
	}
	if res.Status != dag.StatusStalled || sbErr.StallCount != 2 || res.TurnsExecuted != 3 {
		t.Fatalf("unexpected stall outcome: res=%+v err=%+v", res, sbErr)
	}
}

func TestBranchSearcher_GraveyardBacktrackingIsolationAndExactReplay(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	st, err := store.Open(filepath.Join(ws, ".soma"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	if err := st.UpsertAccount(ctx, store.Account{
		ID: "acc_search", Provider: "local", ModelID: "worker", BillingMode: "local", MaxConcurrent: 2,
	}); err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}

	prof := compiler.WorkerProfile()
	prof.MaxStalls = 2

	prov := &guidedSearchProvider{}
	exec := dag.NewExecutor(st, prov, nil, prof)
	exec.Verifier = oracle.NewVerifier(st, sandbox.NewManager())

	runID, rootBranchID, err := exec.StartRun(ctx, dag.RunRequest{
		WorkspacePath: ws,
		WorkspaceKind: "research",
		GoalSpec:      "achieve score >= 0.90 across isolated hypotheses",
		AccountIDs:    []string{"acc_search"},
	})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	artSpec, _ := json.Marshal(oracle.ArtifactOracleSpec{
		RequiredPaths:    []string{"result.json"},
		MetricThresholds: []oracle.MetricThreshold{{Key: "score", Op: ">=", Value: 0.90}},
	})
	if err := st.CreateContract(ctx, store.Contract{
		ID: "ctr_score_90", RunID: runID, Kind: "artifact_oracle", SpecJSON: string(artSpec), PinnedHash: "v1", Required: true,
	}); err != nil {
		t.Fatalf("CreateContract: %v", err)
	}

	searcher := dag.NewBranchSearcher(exec)
	res, err := searcher.SearchExistingRun(ctx, runID, rootBranchID, dag.SearchConfig{
		MaxBranches:       4,
		MaxTurnsPerBranch: 5,
		AnchorTurn:        1,
	})
	if err != nil {
		t.Fatalf("SearchExistingRun failed: %v", err)
	}
	if !res.Completed || res.WinningBranchID == "" || len(res.Attempts) != 3 {
		t.Fatalf("expected 3 explored attempts culminating in completion, got %+v", res)
	}
	if res.GraveyardCount != 2 {
		t.Fatalf("expected 2 Graveyard negative_path entries, got %d", res.GraveyardCount)
	}
	if res.Attempts[0].Passed || res.Attempts[1].Passed || !res.Attempts[2].Passed {
		t.Fatalf("unexpected attempt pass sequence: %+v", res.Attempts)
	}

	// Verify isolated branch workspaces retain distinct independent outputs (no cross-contamination).
	br1, _ := st.GetBranch(ctx, res.Attempts[0].BranchID)
	br2, _ := st.GetBranch(ctx, res.Attempts[1].BranchID)
	br3, _ := st.GetBranch(ctx, res.Attempts[2].BranchID)
	if br1.WorkspaceDir == br2.WorkspaceDir || br2.WorkspaceDir == br3.WorkspaceDir || br1.WorkspaceDir == br3.WorkspaceDir {
		t.Fatalf("branches shared workspace directories: %s | %s | %s", br1.WorkspaceDir, br2.WorkspaceDir, br3.WorkspaceDir)
	}
	b2Bytes, err := os.ReadFile(filepath.Join(br2.WorkspaceDir, "result.json"))
	if err != nil || !strings.Contains(string(b2Bytes), "0.50") {
		t.Fatalf("branch 2 isolated artifact overwritten or missing: %q err=%v", string(b2Bytes), err)
	}
	b3Bytes, err := os.ReadFile(filepath.Join(br3.WorkspaceDir, "result.json"))
	if err != nil || !strings.Contains(string(b3Bytes), "0.96") {
		t.Fatalf("branch 3 winning artifact missing: %q err=%v", string(b3Bytes), err)
	}

	// Verify bit-for-bit ModeExact SHA-256 reconstruction on ALL explored branches (including rolled-over forks).
	replayer := replay.NewReplayer(st)
	for i, att := range res.Attempts {
		rep, err := replayer.Replay(ctx, att.BranchID, replay.ReplayOptions{Mode: replay.ModeExact})
		if err != nil {
			t.Fatalf("ModeExact replay failed on attempt %d (%s): %v", i, att.BranchID, err)
		}
		if rep.Diverged || rep.StepsChecked == 0 || rep.HashVerifiedCount != rep.StepsChecked {
			t.Fatalf("attempt %d exact replay mismatch: %+v", i, rep)
		}
	}
}
