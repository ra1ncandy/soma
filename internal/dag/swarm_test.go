package dag_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ra1ncandy/soma/internal/compiler"
	"github.com/ra1ncandy/soma/internal/dag"
	"github.com/ra1ncandy/soma/internal/oracle"
	"github.com/ra1ncandy/soma/internal/replay"
	"github.com/ra1ncandy/soma/internal/sandbox"
	"github.com/ra1ncandy/soma/internal/store"
	"github.com/ra1ncandy/soma/internal/taint"
)

// cooperativeSwarmProvider simulates 2 concurrent cooperative agents:
// - "worker-backend" writes backend.py, publishes an API contract finding to the Blackboard, and edits top of shared.txt.
// - "worker-tests" waits for the Blackboard finding, writes test_app.py consuming backend.py's function name, and edits bottom of shared.txt.
type cooperativeSwarmProvider struct {
	role            string
	readyBarrier    *sync.WaitGroup
	findingReleased chan struct{}
}

func (p *cooperativeSwarmProvider) Generate(_ context.Context, req dag.GenerateRequest) (*dag.GenerateResponse, error) {
	switch p.role {
	case "backend":
		switch req.TurnIndex {
		case 1:
			return &dag.GenerateResponse{
				Reasoning: "Implement compute_timeout() in backend.py, update top of shared.txt, and publish API finding to Blackboard.",
				ToolCalls: []dag.ToolCallSpec{
					{
						Name:        "fs_write",
						ArgsJSON:    `{"path":"backend.py","content":"def compute_timeout() -> int:\n    return 4250\n"}`,
						EffectClass: taint.EffectWorkspaceWrite,
					},
					{
						Name:        "fs_write",
						ArgsJSON:    `{"path":"shared.txt","content":"HEADER_BACKEND\nline2\nline3\nline4\nline5\nline6\nline7\nline8\n"}`,
						EffectClass: taint.EffectWorkspaceWrite,
					},
					{
						Name:        "publish_finding",
						ArgsJSON:    `{"summary":"Exported compute_timeout() -> 4250 in backend.py"}`,
						EffectClass: taint.EffectReadOnly,
					},
				},
				StateHash:       "backend_t1",
				InputTokens:     120,
				CacheReadTokens: 800,
				OutputTokens:    45,
			}, nil
		default:
			if p.findingReleased != nil {
				select {
				case <-p.findingReleased:
				default:
					close(p.findingReleased)
				}
			}
			return &dag.GenerateResponse{
				Reasoning:    "Backend implementation and blackboard broadcast complete.",
				Done:         true,
				FinalSummary: "backend ready",
				StateHash:    "backend_done",
				InputTokens:  100,
			}, nil
		}

	case "tests":
		switch req.TurnIndex {
		case 1:
			// Wait until backend agent has published its finding on Turn 1 so we deterministically test Blackboard delivery on Turn 2.
			if p.findingReleased != nil {
				<-p.findingReleased
			}
			return &dag.GenerateResponse{
				Reasoning: "Update bottom of shared.txt while waiting for backend Blackboard contract.",
				ToolCalls: []dag.ToolCallSpec{
					{
						Name:        "fs_write",
						ArgsJSON:    `{"path":"shared.txt","content":"line1\nline2\nline3\nline4\nline5\nline6\nline7\nFOOTER_TESTS\n"}`,
						EffectClass: taint.EffectWorkspaceWrite,
					},
				},
				StateHash:       "tests_t1",
				InputTokens:     120,
				CacheReadTokens: 800,
				OutputTokens:    35,
			}, nil
		case 2:
			// Verify the Blackboard finding from worker-backend arrived in Layer 2 without mutating Layer 0/1!
			if !strings.Contains(req.Frame.Layer2, "compute_timeout()") {
				return nil, fmt.Errorf("expected peer finding compute_timeout() in Layer 2, got:\n%s", req.Frame.Layer2)
			}
			return &dag.GenerateResponse{
				Reasoning: "Received compute_timeout() contract from Blackboard; write test_app.py.",
				ToolCalls: []dag.ToolCallSpec{
					{
						Name:        "fs_write",
						ArgsJSON:    `{"path":"test_app.py","content":"import backend\nassert backend.compute_timeout() == 4250\nprint('COOP_SWARM_OK')\n"}`,
						EffectClass: taint.EffectWorkspaceWrite,
					},
				},
				StateHash:       "tests_t2",
				InputTokens:     130,
				CacheReadTokens: 850,
				OutputTokens:    40,
			}, nil
		default:
			return &dag.GenerateResponse{
				Reasoning:    "Integration test written.",
				Done:         true,
				FinalSummary: "tests ready",
				StateHash:    "tests_done",
				InputTokens:  100,
			}, nil
		}
	}
	return &dag.GenerateResponse{Done: true, StateHash: "done"}, nil
}

func TestSwarmCoordinator_CooperativeBlackboardThreeWayMergeAndOracle(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	sharedInitial := "line1\nline2\nline3\nline4\nline5\nline6\nline7\nline8\n"
	if err := os.WriteFile(filepath.Join(ws, "shared.txt"), []byte(sharedInitial), 0o644); err != nil {
		t.Fatalf("WriteFile shared.txt: %v", err)
	}

	st, err := store.Open(filepath.Join(ws, ".soma"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	// Seed 2 concurrent accounts in the QuotaScheduler pool so both agents execute in parallel.
	for _, accID := range []string{"acc_a", "acc_b"} {
		_ = st.UpsertAccount(ctx, store.Account{
			ID: accID, Provider: "local", ModelID: "swarm-mock", BillingMode: "subscription", MaxConcurrent: 2,
		})
	}

	sbx := sandbox.NewManager()
	execDag := dag.NewExecutor(st, &dag.MockProvider{ModelName: "root"}, nil, compiler.WorkerProfile())
	execDag.AccountIDs = []string{"acc_a", "acc_b"}
	execDag.Verifier = oracle.NewVerifier(st, sbx)

	runID, rootBrID, err := execDag.StartRun(ctx, dag.RunRequest{
		Title:         "coop-swarm-test",
		WorkspacePath: ws,
		GoalSpec:      "Build backend.py and test_app.py cooperatively and update shared.txt",
		AccountIDs:    []string{"acc_a", "acc_b"},
	})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	// Attach a root CommandOracle that requires BOTH backend.py (from Agent 1) and test_app.py (from Agent 2)
	// to exist in the merged root workspace and pass `python3 test_app.py`!
	cmdSpec, _ := json.Marshal(oracle.CommandOracleSpec{
		Command: "/usr/bin/python3",
		Args:    []string{"test_app.py"},
	})
	if err := st.CreateContract(ctx, store.Contract{
		ID: "ctr_swarm_" + runID, RunID: runID, Kind: "command_oracle", SpecJSON: string(cmdSpec), PinnedHash: "swarm_v1", Required: true,
	}); err != nil {
		t.Fatalf("CreateContract: %v", err)
	}

	findingReleased := make(chan struct{})
	coord := dag.NewSwarmCoordinator(execDag)
	sRes, err := coord.RunExisting(ctx, runID, rootBrID, dag.SwarmRequest{
		Mode: dag.SwarmModeCooperative,
		Tasks: []dag.SwarmTask{
			{
				Label:     "worker-backend",
				SubGoal:   "Implement compute_timeout() in backend.py and publish finding",
				AccountID: "acc_a",
				Provider:  &cooperativeSwarmProvider{role: "backend", findingReleased: findingReleased},
				MaxTurns:  5,
			},
			{
				Label:     "worker-tests",
				SubGoal:   "Write test_app.py verifying backend.py contract from Blackboard",
				AccountID: "acc_b",
				Provider:  &cooperativeSwarmProvider{role: "tests", findingReleased: findingReleased},
				MaxTurns:  5,
			},
		},
	})
	if err != nil {
		t.Fatalf("SwarmCoordinator.RunExisting: %v", err)
	}
	if !sRes.Completed || !sRes.OracleSummary.AllPassed {
		t.Fatalf("expected cooperative swarm + root oracle to pass, got %+v", sRes)
	}
	if len(sRes.MergeReport.AddedFiles) != 2 || len(sRes.MergeReport.ThreeWayMerged) != 1 {
		t.Fatalf("expected 2 added files (backend.py, test_app.py) and 1 3-way merged file (shared.txt), got %+v", sRes.MergeReport)
	}

	// Verify both worker branches share the exact same Layer 0 + Layer 1 PrefixHashL0L1
	// and pass 100% bit-for-bit SHA-256 deterministic replay (including Blackboard notices!).
	replayer := replay.NewReplayer(st)
	var sharedPrefixHash string
	for _, w := range sRes.Workers {
		rep, rErr := replayer.Replay(ctx, w.BranchID, replay.ReplayOptions{Mode: replay.ModeExact})
		if rErr != nil || rep.Diverged || rep.HashVerifiedCount != rep.StepsChecked {
			t.Fatalf("worker %s failed exact SHA-256 replay: rep=%+v err=%v", w.Label, rep, rErr)
		}
		steps, _ := st.ListBranchSteps(ctx, w.BranchID)
		for _, s := range steps {
			f, fErr := compiler.ReconstructFrame(ctx, st, s.ID, compiler.WorkerProfile())
			if fErr != nil {
				t.Fatalf("ReconstructFrame: %v", fErr)
			}
			if sharedPrefixHash == "" {
				sharedPrefixHash = f.PrefixHashL0L1
			} else if f.PrefixHashL0L1 != sharedPrefixHash {
				t.Fatalf("expected all concurrent workers to share identical Layer 0+1 KV-cache prefix hash %s, got %s on %s",
					sharedPrefixHash, f.PrefixHashL0L1, w.Label)
			}
		}
	}
}

type competitiveSwarmProvider struct {
	mode      string
	trapFailed chan struct{}
}

func (p *competitiveSwarmProvider) Generate(_ context.Context, req dag.GenerateRequest) (*dag.GenerateResponse, error) {
	if p.mode == "trap" {
		// Stalls immediately on turn 1 & 2 to trigger StallBreaker -> AbandonBranch -> Blackboard graveyard_notice.
		return &dag.GenerateResponse{
			Reasoning: "Repeating trapped burst config.",
			ToolCall: &dag.ToolCallSpec{
				Name:        "fs_read",
				ArgsJSON:    `{"path":"README.md"}`,
				EffectClass: taint.EffectReadOnly,
			},
			StateHash:   "stalled_trap_hash",
			InputTokens: 100,
		}, nil
	}

	// Winning worker: on turn 1 reads README, then waits for trap worker to abandon and verifies graveyard_notice in Layer 2!
	switch req.TurnIndex {
	case 1:
		return &dag.GenerateResponse{
			Reasoning: "Inspect workspace while sibling explores trap.",
			ToolCall: &dag.ToolCallSpec{
				Name:        "fs_read",
				ArgsJSON:    `{"path":"README.md"}`,
				EffectClass: taint.EffectReadOnly,
			},
			StateHash:   "win_t1",
			InputTokens: 100,
		}, nil
	case 2:
		if p.trapFailed != nil {
			<-p.trapFailed
		}
		return &dag.GenerateResponse{
			Reasoning: "Write winning solution after sibling stall.",
			ToolCall: &dag.ToolCallSpec{
				Name:        "fs_write",
				ArgsJSON:    `{"path":"winner.txt","content":"causal_multiscale_ok\n"}`,
				EffectClass: taint.EffectWorkspaceWrite,
			},
			StateHash:   "win_t2",
			InputTokens: 110,
		}, nil
	default:
		if !strings.Contains(req.Frame.Layer2, "graveyard_notice") || !strings.Contains(req.Frame.Layer2, "hyp-trap") {
			return nil, fmt.Errorf("expected live graveyard_notice from hyp-trap in Layer 2, got:\n%s", req.Frame.Layer2)
		}
		return &dag.GenerateResponse{
			Reasoning:    "Verified sibling graveyard notice in Layer 2; completing winning branch.",
			Done:         true,
			FinalSummary: "competitive winner done",
			StateHash:    "win_done",
			InputTokens:  115,
		}, nil
	}
}

func TestSwarmCoordinator_CompetitiveLiveGraveyardBroadcast(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	_ = os.WriteFile(filepath.Join(ws, "README.md"), []byte("# competitive test\n"), 0o644)

	st, err := store.Open(filepath.Join(ws, ".soma"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	_ = st.UpsertAccount(ctx, store.Account{
		ID: "comp_pool", Provider: "local", ModelID: "swarm-mock", BillingMode: "subscription", MaxConcurrent: 4,
	})

	prof := compiler.WorkerProfile()
	prof.MaxStalls = 1
	execDag := dag.NewExecutor(st, &dag.MockProvider{ModelName: "root"}, nil, prof)
	execDag.AccountIDs = []string{"comp_pool"}

	trapFailed := make(chan struct{})
	// Wrap trap provider so we signal trapFailed as soon as turn 2 triggers StallBreaker.
	trapProv := &competitiveSwarmProvider{mode: "trap"}
	winProv := &competitiveSwarmProvider{mode: "winner", trapFailed: trapFailed}

	// Hook AfterDispatch on trap via a custom wrapper provider that closes trapFailed after 2nd Generate.
	var trapCalls int
	var mu sync.Mutex
	wrappedTrap := providerFunc(func(ctx context.Context, req dag.GenerateRequest) (*dag.GenerateResponse, error) {
		resp, err := trapProv.Generate(ctx, req)
		mu.Lock()
		trapCalls++
		c := trapCalls
		mu.Unlock()
		if c >= 2 {
			go func() {
				// Give AbandonBranch a brief moment to publish graveyard_notice before winner enters turn 3.
				for {
					if len(execDag.Blackboard.Snapshot(req.RunID)) >= 3 {
						select {
						case <-trapFailed:
						default:
							close(trapFailed)
						}
						return
					}
				}
			}()
		}
		return resp, err
	})

	coord := dag.NewSwarmCoordinator(execDag)
	sRes, err := coord.Run(ctx, dag.SwarmRequest{
		RunRequest: dag.RunRequest{
			Title:         "comp-swarm-test",
			WorkspacePath: ws,
			GoalSpec:      "Explore competing hypotheses in parallel",
			Profile:       prof,
			AccountIDs:    []string{"comp_pool"},
		},
		Mode: dag.SwarmModeCompetitive,
		Tasks: []dag.SwarmTask{
			{Label: "hyp-trap", SubGoal: "Try burst trap hypothesis", Provider: wrappedTrap, MaxTurns: 5},
			{Label: "hyp-winner", SubGoal: "Try causal multiscale hypothesis", Provider: winProv, MaxTurns: 5},
		},
	})
	if err != nil {
		t.Fatalf("SwarmCoordinator.Run competitive: %v", err)
	}
	if !sRes.Completed || sRes.GraveyardCount != 1 {
		t.Fatalf("expected competitive swarm to complete with 1 Graveyard entry, got %+v", sRes)
	}
}

type providerFunc func(ctx context.Context, req dag.GenerateRequest) (*dag.GenerateResponse, error)

func (f providerFunc) Generate(ctx context.Context, req dag.GenerateRequest) (*dag.GenerateResponse, error) {
	return f(ctx, req)
}
