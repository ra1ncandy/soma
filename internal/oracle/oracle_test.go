package oracle

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ra1ncandy/soma/internal/compiler"
	"github.com/ra1ncandy/soma/internal/dag"
	"github.com/ra1ncandy/soma/internal/sandbox"
	"github.com/ra1ncandy/soma/internal/store"
	"github.com/ra1ncandy/soma/internal/taint"
)

type oracleBuilderProvider struct {
	tamperFirst bool
}

func (p *oracleBuilderProvider) Generate(_ context.Context, req dag.GenerateRequest) (*dag.GenerateResponse, error) {
	switch req.TurnIndex {
	case 1:
		if p.tamperFirst {
			// Sneaky builder writes stale hash / bad metrics on first try.
			return &dag.GenerateResponse{
				Reasoning: "write stale metrics",
				ToolCall: &dag.ToolCallSpec{
					Name:        "fs_write",
					ArgsJSON:    `{"path":"artifacts/eval.json","content":"{\"d3_60x_dilated\":0.0021,\"sha\":\"old\"}"}`,
					EffectClass: taint.EffectWorkspaceWrite,
				},
				StateHash: "bad_art",
			}, nil
		}
		return &dag.GenerateResponse{
			Reasoning: "write good metrics",
			ToolCall: &dag.ToolCallSpec{
				Name:        "fs_write",
				ArgsJSON:    `{"path":"artifacts/eval.json","content":"{\"d3_60x_dilated\":0.9610,\"sha\":\"fresh\"}"}`,
				EffectClass: taint.EffectWorkspaceWrite,
			},
			StateHash: "good_art",
		}, nil
	case 2:
		return &dag.GenerateResponse{
			Reasoning:    "claim done prematurely",
			Done:         true,
			FinalSummary: "done 1",
			StateHash:    "done1",
		}, nil
	case 3:
		// After oracle rejected Turn 2 and injected LastOracleError in L3, fix artifact!
		if !strings.Contains(req.Frame.Layer3, "ORACLE") && !strings.Contains(req.Frame.Layer3, "metric") && !strings.Contains(req.Frame.Layer3, "stale") {
			return &dag.GenerateResponse{Done: true, FinalSummary: "no counterexample seen"}, nil
		}
		return &dag.GenerateResponse{
			Reasoning: "fix metrics to meet Track D threshold",
			ToolCall: &dag.ToolCallSpec{
				Name:        "fs_write",
				ArgsJSON:    `{"path":"artifacts/eval.json","content":"{\"d3_60x_dilated\":0.9610,\"sha\":\"fresh\"}"}`,
				EffectClass: taint.EffectWorkspaceWrite,
			},
			StateHash: "fixed_art",
		}, nil
	default:
		return &dag.GenerateResponse{
			Reasoning:    "now verified",
			Done:         true,
			FinalSummary: "verified done",
			StateHash:    "done_ok",
		}, nil
	}
}

func TestCommandArtifactAdvisoryOraclesAndGate(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	st, err := store.Open(filepath.Join(ws, ".soma"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	_ = st.UpsertAccount(ctx, store.Account{
		ID: "acc", Provider: "local", ModelID: "m1", BillingMode: "local", MaxConcurrent: 2,
	})

	// 1. ArtifactOracle + Executor Gate blocks premature Done until threshold passes.
	exec := dag.NewExecutor(st, &oracleBuilderProvider{tamperFirst: true}, nil, compiler.WorkerProfile())
	exec.Verifier = NewVerifier(st, sandbox.NewManager())
	runID, branchID, err := exec.StartRun(ctx, dag.RunRequest{
		WorkspacePath: ws,
		GoalSpec:      "beat Track D 0.90",
	})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	artSpec, _ := json.Marshal(ArtifactOracleSpec{
		RequiredPaths:    []string{"artifacts/eval.json"},
		DisallowedHashes: map[string][]string{"artifacts/eval.json": {"deadbeef"}},
		RequiredJSONKeys: map[string][]string{"artifacts/eval.json": {"d3_60x_dilated", "sha"}},
		MetricThresholds: []MetricThreshold{{Key: "d3_60x_dilated", Op: ">=", Value: 0.90}},
	})
	if err := st.CreateContract(ctx, store.Contract{
		ID: "ctr_art", RunID: runID, Kind: "artifact_oracle", SpecJSON: string(artSpec), PinnedHash: "h1", Required: true,
	}); err != nil {
		t.Fatalf("CreateContract: %v", err)
	}

	res, err := exec.ExecuteBranch(ctx, branchID, 6)
	if err != nil || res.Status != dag.StatusCompleted {
		t.Fatalf("expected recovery & completion on turn 4 after oracle rejection on turn 2, got res=%+v err=%v", res, err)
	}
	cruns, err := st.ListBranchContractRuns(ctx, branchID)
	if err != nil || len(cruns) != 2 || cruns[0].Passed || !cruns[1].Passed {
		t.Fatalf("expected 1 failed then 1 passed ContractRun, got %+v err=%v", cruns, err)
	}

	// 2. Stale hash rejection check.
	br, _ := st.GetBranch(ctx, branchID)
	evalSHA, err := HashFileSHA256(filepath.Join(br.WorkspaceDir, "artifacts", "eval.json"))
	if err != nil {
		t.Fatalf("HashFileSHA256: %v", err)
	}
	passed, _, ce, err := evalArtifactOracle(br.WorkspaceDir, ArtifactOracleSpec{
		RequiredPaths:    []string{"artifacts/eval.json"},
		DisallowedHashes: map[string][]string{"artifacts/eval.json": {evalSHA}},
	})
	if err != nil || passed || !strings.Contains(ce, "stale artifact rejected") {
		t.Fatalf("expected stale artifact rejection, got passed=%v ce=%q err=%v", passed, ce, err)
	}

	// 3. CommandOracle pinned-file tamper detection + real bwrap execution.
	if _, err := exec.Tools.Execute(ctx, dag.ToolCall{
		Name: "fs_write", ArgsJSON: `{"path":"pinned.txt","content":"assert x == 1\n"}`, WorkspaceDir: br.WorkspaceDir,
	}); err != nil {
		t.Fatalf("seed pinned.txt: %v", err)
	}
	pinSHA, _ := HashFileSHA256(filepath.Join(br.WorkspaceDir, "pinned.txt"))
	if _, err := exec.Tools.Execute(ctx, dag.ToolCall{
		Name: "fs_write", ArgsJSON: `{"path":"pinned.txt","content":"assert true\n"}`, WorkspaceDir: br.WorkspaceDir,
	}); err != nil {
		t.Fatalf("tamper pinned.txt: %v", err)
	}
	if _, err := exec.Tools.Execute(ctx, dag.ToolCall{
		Name: "fs_write", ArgsJSON: `{"path":"pinned.txt","content":"assert true\n"}`, WorkspaceDir: br.WorkspaceDir,
	}); err != nil {
		t.Fatalf("tamper pinned.txt: %v", err)
	}

	ver := NewVerifier(st, sandbox.NewManager())
	okCmd, _, ceCmd, err := ver.evalCommandOracle(ctx, runID, br.WorkspaceDir, CommandOracleSpec{
		Command:     "/bin/sh",
		Args:        []string{"-c", "echo ok"},
		PinnedFiles: map[string]string{"pinned.txt": pinSHA},
	})
	if err != nil || okCmd || !strings.Contains(ceCmd, ErrPinnedInvariantTampered.Error()) {
		t.Fatalf("expected ErrPinnedInvariantTampered, got ok=%v ce=%q err=%v", okCmd, ceCmd, err)
	}

	// Clean CommandOracle inside bwrap (if bwrap installed).
	if _, lpErr := execLookPath("bwrap"); lpErr == nil {
		_ = os.WriteFile(filepath.Join(br.WorkspaceDir, "pinned.txt"), []byte("assert x == 1\n"), 0o644)
		okClean, _, ceClean, err := ver.evalCommandOracle(ctx, runID, br.WorkspaceDir, CommandOracleSpec{
			Command:          "/bin/sh",
			Args:             []string{"-c", "cat /workspace/pinned.txt"},
			PinnedFiles:      map[string]string{"pinned.txt": pinSHA},
			JSONMetricsFile:  "artifacts/eval.json",
			MetricThresholds: []MetricThreshold{{Key: "d3_60x_dilated", Op: ">=", Value: 0.90}},
		})
		if err != nil || !okClean {
			t.Fatalf("expected clean CommandOracle pass, got ok=%v ce=%q err=%v", okClean, ceClean, err)
		}
	}

	// 4. AdvisoryAudit test.
	advProv := &mockAdvProvider{}
	rep, err := RunAdvisoryAudit(ctx, advProv, "adv", "fable-5.2", "fix race", "none", "+ mutex.Lock()")
	if err != nil || rep.Passed || !strings.Contains(rep.Counterexample, "missing Unlock") {
		t.Fatalf("unexpected advisory report: %+v err=%v", rep, err)
	}
}

type mockAdvProvider struct{}

func (m *mockAdvProvider) Generate(_ context.Context, _ dag.GenerateRequest) (*dag.GenerateResponse, error) {
	return &dag.GenerateResponse{Done: true, FinalSummary: "COUNTEREXAMPLE: missing Unlock() on error return path"}, nil
}

func execLookPath(bin string) (string, error) {
	return exec.LookPath(bin)
}

var _ = errors.Is
