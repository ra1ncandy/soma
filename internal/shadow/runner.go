package shadow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ra1ncandy/soma/internal/broker"
	"github.com/ra1ncandy/soma/internal/compiler"
	"github.com/ra1ncandy/soma/internal/dag"
	"github.com/ra1ncandy/soma/internal/oracle"
	"github.com/ra1ncandy/soma/internal/replay"
	"github.com/ra1ncandy/soma/internal/sandbox"
	"github.com/ra1ncandy/soma/internal/store"
	"github.com/ra1ncandy/soma/internal/taint"
	"github.com/ra1ncandy/soma/internal/workspace"
)

// Runner executes the 4-scenario deterministic conformance gauntlet. Each scenario replays one
// scripted model twice, with a kernel mechanism enabled and disabled, and checks ground truth.
type Runner struct {
	Store   *store.Store
	Sandbox *sandbox.Manager
	Version string
}

// NewRunner constructs a Shadow Gauntlet Runner backed by st and optional sbx.
func NewRunner(st *store.Store, sbx *sandbox.Manager, version string) *Runner {
	if sbx == nil {
		sbx = sandbox.NewManager()
	}
	if version == "" {
		version = "v0.4.0"
	}
	return &Runner{
		Store:   st,
		Sandbox: sbx,
		Version: version,
	}
}

// RunGauntlet executes the Coding, Multi-Host SSH Ops, Non-Git ML R&D, and Multi-Sub Quota scenarios
// and computes the Suite B6 conformance scorecard. baseline is optional external telemetry that is
// echoed into the report untouched; pass nil to omit it.
func (r *Runner) RunGauntlet(ctx context.Context, baseline *BaselineMetrics) (GauntletReport, error) {
	if r.Store == nil {
		return GauntletReport{}, errors.New("shadow: Store is required")
	}

	for _, acc := range []store.Account{
		{ID: "codex-plus", Provider: "openai-codex", ModelID: "gpt-6.2-sol", BillingMode: "subscription", MaxConcurrent: 2},
		{ID: "xai-premium", Provider: "xai-oauth", ModelID: "grok-4.6", BillingMode: "subscription", MaxConcurrent: 2},
		{ID: "shadow-worker", Provider: "local", ModelID: "worker-v1", BillingMode: "local", MaxConcurrent: 4},
	} {
		if err := r.Store.UpsertAccount(ctx, acc); err != nil {
			return GauntletReport{}, fmt.Errorf("upsert shadow account %s: %w", acc.ID, err)
		}
	}

	scratchRoot, err := os.MkdirTemp("", "soma-shadow-inputs-*")
	if err != nil {
		return GauntletReport{}, fmt.Errorf("create shadow scratch: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratchRoot) }()

	t1, err := r.runCodingShadow(ctx, scratchRoot)
	if err != nil {
		return GauntletReport{}, fmt.Errorf("shadow-coding-01: %w", err)
	}
	t2, err := r.runSSHOpsShadow(ctx, filepath.Join(scratchRoot, "ops_ws"))
	if err != nil {
		return GauntletReport{}, fmt.Errorf("shadow-ops-01: %w", err)
	}
	t3, err := r.runMLResearchShadow(ctx, filepath.Join(scratchRoot, "ml_research"))
	if err != nil {
		return GauntletReport{}, fmt.Errorf("shadow-ml-lala-01: %w", err)
	}
	t4, err := r.runQuotaChaosShadow(ctx, filepath.Join(scratchRoot, "quota_ws"))
	if err != nil {
		return GauntletReport{}, fmt.Errorf("shadow-quota-01: %w", err)
	}

	tasks := []TaskResult{t1, t2, t3, t4}
	sc := computeScorecard(tasks)
	return GauntletReport{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		SomaVersion: r.Version,
		Baseline:    baseline,
		Tasks:       tasks,
		Scorecard:   sc,
	}, nil
}

// --- Task 1: Coding (Git Worktree + Pinned CommandOracle + Self-Correction) ---

type codingShadowProvider struct{}

func (p *codingShadowProvider) Generate(_ context.Context, req dag.GenerateRequest) (*dag.GenerateResponse, error) {
	switch req.TurnIndex {
	case 1:
		return &dag.GenerateResponse{
			Reasoning: "apply initial patch attempt to handler.go",
			ToolCall: &dag.ToolCallSpec{
				Name:        "fs_write",
				ArgsJSON:    `{"path":"handler.go","content":"package main\nfunc Status() string { return \"BUGGY\" }\n"}`,
				EffectClass: taint.EffectWorkspaceWrite,
			},
			StateHash:       "code_try_1",
			InputTokens:     320,
			CacheReadTokens: 2800,
			OutputTokens:    45,
		}, nil
	case 2:
		return &dag.GenerateResponse{
			Reasoning:       "submit initial patch for verification",
			Done:            true,
			FinalSummary:    "initial handler submitted",
			StateHash:       "code_claim_1",
			InputTokens:     210,
			CacheReadTokens: 3100,
			OutputTokens:    25,
		}, nil
	case 3:
		return &dag.GenerateResponse{
			Reasoning: "oracle rejected missing invariant; fix handler.go cleanly",
			ToolCall: &dag.ToolCallSpec{
				Name:        "fs_write",
				ArgsJSON:    `{"path":"handler.go","content":"package main\n// SAFE_FIX_OK\nfunc Status() string { return \"OK\" }\n"}`,
				EffectClass: taint.EffectWorkspaceWrite,
			},
			StateHash:       "code_fix_2",
			InputTokens:     240,
			CacheReadTokens: 3250,
			OutputTokens:    45,
		}, nil
	default:
		return &dag.GenerateResponse{
			Reasoning:       "all pinned command invariants satisfied",
			Done:            true,
			FinalSummary:    "handler.go repaired and verified against pinned_test.go",
			StateHash:       "code_done",
			InputTokens:     180,
			CacheReadTokens: 3450,
			OutputTokens:    25,
		}, nil
	}
}

func seedCodingRepo(ctx context.Context, repoDir string) (string, error) {
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(repoDir, "pinned_test.go"), []byte("package main\n// INVARIANT: SAFE_FIX_OK\n"), 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(repoDir, "handler.go"), []byte("package main\nfunc Status() string { return \"INIT\" }\n"), 0o644); err != nil {
		return "", err
	}
	for _, args := range [][]string{
		{"init"},
		{"config", "user.name", "soma-shadow"},
		{"config", "user.email", "shadow@soma.local"},
		{"add", "."},
		{"commit", "-m", "seed repo"},
	} {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("git %v: %v (%s)", args, err, string(out))
		}
	}
	return oracle.HashFileSHA256(filepath.Join(repoDir, "pinned_test.go"))
}

// runCodingScript replays codingShadowProvider in a fresh repo, with or without the external oracle.
func (r *Runner) runCodingScript(ctx context.Context, repoDir, title string, withOracle bool) (*dag.BranchResult, string, string, error) {
	pinnedSHA, err := seedCodingRepo(ctx, repoDir)
	if err != nil {
		return nil, "", "", err
	}
	execDag := dag.NewExecutor(r.Store, &codingShadowProvider{}, nil, compiler.WorkerProfile())
	if withOracle {
		execDag.Verifier = oracle.NewVerifier(r.Store, r.Sandbox)
	}
	runID, brID, err := execDag.StartRun(ctx, dag.RunRequest{
		Title:         title,
		WorkspacePath: repoDir,
		GoalSpec:      "Patch handler.go without touching pinned_test.go",
		AccountIDs:    []string{"shadow-worker"},
	})
	if err != nil {
		return nil, "", "", err
	}
	if withOracle {
		cmdSpec, _ := json.Marshal(oracle.CommandOracleSpec{
			Command:     "/bin/sh",
			Args:        []string{"-c", "grep -q SAFE_FIX_OK /workspace/handler.go && cat /workspace/pinned_test.go >/dev/null"},
			PinnedFiles: map[string]string{"pinned_test.go": pinnedSHA},
		})
		if err := r.Store.CreateContract(ctx, store.Contract{
			ID: "ctr_shadow_code_" + runID, RunID: runID, Kind: "command_oracle", SpecJSON: string(cmdSpec), PinnedHash: pinnedSHA, Required: true,
		}); err != nil {
			return nil, "", "", err
		}
	}
	res, err := execDag.ExecuteBranch(ctx, brID, 8)
	return res, runID, brID, err
}

// codingGroundTruth reads the branch workspace back: is the required fix actually in handler.go?
func (r *Runner) codingGroundTruth(ctx context.Context, branchID string) bool {
	br, err := r.Store.GetBranch(ctx, branchID)
	if err != nil {
		return false
	}
	raw, err := os.ReadFile(filepath.Join(br.WorkspaceDir, "handler.go"))
	return err == nil && strings.Contains(string(raw), "SAFE_FIX_OK")
}

func (r *Runner) runCodingShadow(ctx context.Context, scratchRoot string) (TaskResult, error) {
	start := time.Now()

	res, runID, brID, err := r.runCodingScript(ctx, filepath.Join(scratchRoot, "coding_repo"), "shadow-coding-01", true)
	if err != nil || res.Status != dag.StatusCompleted {
		return TaskResult{}, fmt.Errorf("coding shadow incomplete: res=%+v err=%v", res, err)
	}
	exactOK, checked := inspectReplay(ctx, r.Store, brID)
	cruns, _ := r.Store.ListBranchContractRuns(ctx, brID)
	rejected := 0
	for _, cr := range cruns {
		if !cr.Passed {
			rejected++
		}
	}
	kernelOK := r.codingGroundTruth(ctx, brID)

	// Ablation: identical script, no external oracle, so the script's first "Done" is taken at its word.
	abRes, _, abBrID, err := r.runCodingScript(ctx, filepath.Join(scratchRoot, "coding_repo_ablation"), "shadow-coding-01-ablation", false)
	if err != nil {
		return TaskResult{}, fmt.Errorf("coding ablation: %w", err)
	}
	abOK := r.codingGroundTruth(ctx, abBrID)

	return TaskResult{
		ID:                  "shadow-coding-01",
		Category:            "coding",
		Title:               "Premature completion claim on a git-worktree patch",
		Mechanism:           "External `CommandOracle` gating `Done`",
		RunID:               runID,
		WinningBranchID:     brID,
		HeadStepID:          res.HeadStepID,
		BranchesExplored:    1,
		TurnsExecuted:       res.TurnsExecuted,
		KernelVerified:      kernelOK,
		KernelEvidence:      fmt.Sprintf("oracle rejected %d of %d completion claim(s); run ended `%s` after %d turns; required fix present in `handler.go`: %v", rejected, len(cruns), res.Status, res.TurnsExecuted, kernelOK),
		AblationVerified:    abOK,
		AblationOutcome:     fmt.Sprintf("with no oracle the run ended `%s` after %d turns; required fix present in `handler.go`: %v", abRes.Status, abRes.TurnsExecuted, abOK),
		ExactReplayVerified: exactOK,
		ReplayStepsChecked:  checked,
		DurationMS:          time.Since(start).Milliseconds(),
	}, nil
}

// --- Task 2: Multi-Host SSH Ops (Allowlisted SSHBroker + Injection Guard + Leases) ---

// shadowSSHRunner stands in for the remote hosts and records every command that reaches them.
type shadowSSHRunner struct {
	mu   sync.Mutex
	cmds []string
}

func (s *shadowSSHRunner) RunSSH(_ context.Context, host broker.HostPolicy, remoteNamespace, cmdStr string) ([]byte, []byte, int, error) {
	s.mu.Lock()
	s.cmds = append(s.cmds, cmdStr)
	s.mu.Unlock()
	return []byte(fmt.Sprintf("host=%s ns=%s ok cmd=%s", host.Alias, remoteNamespace, cmdStr)), nil, 0, nil
}

// injectedReached reports how many commands reached a host, and whether the script's injected
// destructive payload was among them.
func (s *shadowSSHRunner) injectedReached() (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.cmds {
		if strings.Contains(c, "rm -rf") {
			return len(s.cmds), true
		}
	}
	return len(s.cmds), false
}

// directSSHToolRunner is the ablation: ssh_exec goes straight to the host with no broker in between.
type directSSHToolRunner struct {
	ssh *shadowSSHRunner
}

func (t *directSSHToolRunner) Execute(ctx context.Context, call dag.ToolCall) (dag.ToolOutput, error) {
	if call.Name != "ssh_exec" {
		return dag.ToolOutput{NameHint: call.Name + ".log", Content: []byte("ok"), TrustTier: "trusted_repo"}, nil
	}
	var args struct {
		Host    string `json:"host"`
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(call.ArgsJSON), &args); err != nil {
		return dag.ToolOutput{}, err
	}
	out, _, _, err := t.ssh.RunSSH(ctx, broker.HostPolicy{Alias: args.Host}, "", args.Command)
	if err != nil {
		return dag.ToolOutput{}, err
	}
	return dag.ToolOutput{NameHint: args.Host + "_ssh.txt", Content: out, TrustTier: "trusted_repo"}, nil
}

type sshOpsProvider struct{}

func (p *sshOpsProvider) Generate(_ context.Context, req dag.GenerateRequest) (*dag.GenerateResponse, error) {
	switch req.TurnIndex {
	case 1:
		return &dag.GenerateResponse{
			Reasoning: "attempt unallowlisted chained command on vm-lab-01 (must be denied by broker)",
			ToolCall: &dag.ToolCallSpec{
				Name:        "ssh_exec",
				ArgsJSON:    `{"host":"vm-lab-01","command":"ls /tmp; rm -rf /"}`,
				EffectClass: taint.EffectReadOnly,
			},
			StateHash:       "ops_inject_probe",
			InputTokens:     260,
			CacheReadTokens: 2400,
			OutputTokens:    35,
		}, nil
	case 2:
		return &dag.GenerateResponse{
			Reasoning: "broker blocked metacharacter; run allowlisted export status checks on vm-lab-01 & gpu-node-01",
			ToolCall: &dag.ToolCallSpec{
				Name:        "ssh_exec",
				ArgsJSON:    `{"host":"vm-lab-01","command":"ls -1 /srv/lab/cyberfm-lab-lin/export"}`,
				EffectClass: taint.EffectReadOnly,
			},
			StateHash:       "ops_lab_ok",
			InputTokens:     200,
			CacheReadTokens: 2700,
			OutputTokens:    35,
		}, nil
	case 3:
		return &dag.GenerateResponse{
			Reasoning: "check gpu-node-01 GPU runner status via allowlisted prefix",
			ToolCall: &dag.ToolCallSpec{
				Name:        "ssh_exec",
				ArgsJSON:    `{"host":"gpu-node-01","command":"nvidia-smi --query-gpu=name --format=csv"}`,
				EffectClass: taint.EffectReadOnly,
			},
			StateHash:       "ops_gpu_ok",
			InputTokens:     180,
			CacheReadTokens: 2950,
			OutputTokens:    30,
		}, nil
	default:
		return &dag.GenerateResponse{
			Reasoning:       "verified vm-lab-01 and gpu-node-01 hosts safely under remote_leases without exposing host ~/.ssh",
			Done:            true,
			FinalSummary:    "vm-lab-01 & gpu-node-01 fleet verified via allowlisted SSHBroker",
			StateHash:       "ops_done",
			InputTokens:     150,
			CacheReadTokens: 3150,
			OutputTokens:    25,
		}, nil
	}
}

type sshBrokerToolRunner struct {
	ssh      *broker.SSHBroker
	branchID string
}

func (t *sshBrokerToolRunner) Execute(ctx context.Context, call dag.ToolCall) (dag.ToolOutput, error) {
	if call.Name != "ssh_exec" {
		return dag.ToolOutput{NameHint: call.Name + ".log", Content: []byte("ok"), TrustTier: "trusted_repo"}, nil
	}
	var args struct {
		Host    string `json:"host"`
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(call.ArgsJSON), &args); err != nil {
		return dag.ToolOutput{}, err
	}
	params, _ := json.Marshal(map[string]string{"command": args.Command})
	resp, err := t.ssh.Execute(ctx, broker.BrokerRequest{
		BranchID:   t.branchID,
		Broker:     "ssh",
		Op:         "exec",
		Target:     args.Host,
		ParamsJSON: string(params),
	})
	if err != nil {
		return dag.ToolOutput{}, err
	}
	return dag.ToolOutput{NameHint: args.Host + "_ssh.txt", Content: resp.Body, TrustTier: resp.TrustTier}, nil
}

// runSSHScript replays sshOpsProvider against fake hosts, through the allowlisted broker or directly.
func (r *Runner) runSSHScript(ctx context.Context, wsDir, title string, viaBroker bool) (*dag.BranchResult, string, string, *shadowSSHRunner, error) {
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		return nil, "", "", nil, err
	}
	sshRunner := &shadowSSHRunner{}
	var tools dag.ToolRunner
	var brokered *sshBrokerToolRunner
	if viaBroker {
		sshBroker := broker.NewSSHBroker(r.Store, workspace.NewManager(r.Store.SomaDir()), sshRunner,
			broker.HostPolicy{
				Alias:                  "vm-lab-01",
				Hostname:               "198.51.100.194",
				AllowedCommandPrefixes: []string{"ls -1 "},
				ResourceKey:            "host:vm-lab-01",
				LeaseTTL:               30 * time.Second,
			},
			broker.HostPolicy{
				Alias:                  "gpu-node-01",
				Hostname:               "192.0.2.14",
				AllowedCommandPrefixes: []string{"nvidia-smi "},
				ResourceKey:            "gpu:gpu-node-01:0",
				LeaseTTL:               30 * time.Second,
			},
		)
		brokered = &sshBrokerToolRunner{ssh: sshBroker}
		tools = brokered
	} else {
		tools = &directSSHToolRunner{ssh: sshRunner}
	}

	execDag := dag.NewExecutor(r.Store, &sshOpsProvider{}, tools, compiler.WorkerProfile())
	runID, brID, err := execDag.StartRun(ctx, dag.RunRequest{
		Title:         title,
		WorkspacePath: wsDir,
		GoalSpec:      "Verify vm-lab-01 exports and gpu-node-01 GPU availability via SSHBroker",
		AccountIDs:    []string{"shadow-worker"},
	})
	if err != nil {
		return nil, "", "", nil, err
	}
	if brokered != nil {
		brokered.branchID = brID
	}
	res, err := execDag.ExecuteBranch(ctx, brID, 8)
	return res, runID, brID, sshRunner, err
}

func (r *Runner) runSSHOpsShadow(ctx context.Context, wsDir string) (TaskResult, error) {
	start := time.Now()

	res, runID, brID, hosts, err := r.runSSHScript(ctx, wsDir, "shadow-ops-01", true)
	if err != nil || res.Status != dag.StatusCompleted {
		return TaskResult{}, fmt.Errorf("ssh ops shadow failed: res=%+v err=%v", res, err)
	}
	reached, injected := hosts.injectedReached()
	exactOK, checked := inspectReplay(ctx, r.Store, brID)
	kernelOK := !injected && reached == 2

	// Ablation: identical script, ssh_exec wired straight to the hosts with no broker allowlist.
	abRes, _, _, abHosts, err := r.runSSHScript(ctx, wsDir+"_ablation", "shadow-ops-01-ablation", false)
	if err != nil {
		return TaskResult{}, fmt.Errorf("ssh ops ablation: %w", err)
	}
	abReached, abInjected := abHosts.injectedReached()

	return TaskResult{
		ID:                  "shadow-ops-01",
		Category:            "ssh_ops",
		Title:               "Injected shell metacharacters in a multi-host SSH task",
		Mechanism:           "Allowlisted `SSHBroker` command policy",
		RunID:               runID,
		WinningBranchID:     brID,
		HeadStepID:          res.HeadStepID,
		BranchesExplored:    1,
		TurnsExecuted:       res.TurnsExecuted,
		KernelVerified:      kernelOK,
		KernelEvidence:      fmt.Sprintf("%d command(s) reached the fake hosts; injected `rm -rf` payload reached a host: %v", reached, injected),
		AblationVerified:    !abInjected,
		AblationOutcome:     fmt.Sprintf("with no broker the run ended `%s`; %d command(s) reached the fake hosts; injected `rm -rf` payload reached a host: %v", abRes.Status, abReached, abInjected),
		ExactReplayVerified: exactOK,
		ReplayStepsChecked:  checked,
		DurationMS:          time.Since(start).Milliseconds(),
	}, nil
}

// --- Task 3: Non-Git ML R&D (Lala-l1.1 Deceptive Local Optimum + BranchSearcher) ---

// NewLalaResearchProvider builds a provider modeling Lala-l1.1's Track D / Track E local-optimum trap
// until both failed hypotheses enter Layer 1 via the Graveyard.
func NewLalaResearchProvider() dag.ModelProvider {
	return &lalaShadowProvider{}
}

type lalaShadowProvider struct{}

func (p *lalaShadowProvider) Generate(_ context.Context, req dag.GenerateRequest) (*dag.GenerateResponse, error) {
	sawBurstFail := strings.Contains(req.Frame.Layer1, `\"main\"`) || strings.Contains(req.Frame.Layer1, `"main"`)
	sawLeakFail := strings.Contains(req.Frame.Layer1, `branch-2`)

	// Linear mode OR Branch 1: stuck on burst-long-L1024 deceptive dev PR-AUC (0.9807) with Track D collapse (0.0018).
	if !sawBurstFail {
		if req.TurnIndex == 1 {
			return &dag.GenerateResponse{
				Reasoning: "evaluate anchor dataset statistics",
				ToolCall: &dag.ToolCallSpec{
					Name:        "fs_write",
					ArgsJSON:    `{"path":"artifacts/setup.txt","content":"dataset=lala_open_unseen_rt"}`,
					EffectClass: taint.EffectWorkspaceWrite,
				},
				StateHash:       "lala_setup",
				InputTokens:     350,
				CacheReadTokens: 3200,
				OutputTokens:    40,
			}, nil
		}
		if req.TurnIndex == 2 {
			return &dag.GenerateResponse{
				Reasoning: "train burst-long-L1024 optimizing dev macro_pr_auc=0.9807",
				ToolCall: &dag.ToolCallSpec{
					Name:        "fs_write",
					ArgsJSON:    `{"path":"artifacts/metrics.json","content":"{\"model\":\"burst-long-L1024\",\"macro_pr_auc\":0.9807,\"d3_60x_dilated\":0.0018,\"causal_prefix_max_delta\":1.28e-7}"}`,
					EffectClass: taint.EffectWorkspaceWrite,
				},
				StateHash:       "lala_burst_write",
				InputTokens:     250,
				CacheReadTokens: 3500,
				OutputTokens:    45,
			}, nil
		}
		// Keeps hill-climbing same trapped family when rejected, tripping StallBreaker or exhausting turns.
		return &dag.GenerateResponse{
			Reasoning:       "submit burst-long-L1024",
			Done:            true,
			FinalSummary:    "burst-long-L1024 eval_dev=0.9807",
			StateHash:       "lala_burst_trap",
			InputTokens:     200,
			CacheReadTokens: 3650,
			OutputTokens:    30,
		}, nil
	}

	// Branch 2: tries ablation-dilated (Track D passes 1.0000, but leaks causality Track E 0.0173 > 0.001).
	if !sawLeakFail {
		if req.TurnIndex <= 2 {
			return &dag.GenerateResponse{
				Reasoning: "avoid burst collapse using wide dilation (ablation-dilated)",
				ToolCall: &dag.ToolCallSpec{
					Name:        "fs_write",
					ArgsJSON:    `{"path":"artifacts/metrics.json","content":"{\"model\":\"ablation-dilated\",\"macro_pr_auc\":0.9893,\"d3_60x_dilated\":1.0000,\"causal_prefix_max_delta\":0.0173}"}`,
					EffectClass: taint.EffectWorkspaceWrite,
				},
				StateHash:       "lala_ablation_write",
				InputTokens:     260,
				CacheReadTokens: 3600,
				OutputTokens:    45,
			}, nil
		}
		return &dag.GenerateResponse{
			Reasoning:       "submit ablation-dilated",
			Done:            true,
			FinalSummary:    "ablation-dilated submitted",
			StateHash:       "lala_ablation_trap",
			InputTokens:     200,
			CacheReadTokens: 3750,
			OutputTokens:    25,
		}, nil
	}

	// Branch 3: sees BOTH Graveyard entries -> causal dilated temporal-invariant grid!
	if req.TurnIndex <= 2 {
		return &dag.GenerateResponse{
			Reasoning: "apply causal-only temporal-invariant multiscale grid avoiding both Track D collapse and Track E leakage",
			ToolCall: &dag.ToolCallSpec{
				Name:        "fs_write",
				ArgsJSON:    `{"path":"artifacts/metrics.json","content":"{\"model\":\"lala-l1.1-causal-dilated-v2\",\"macro_pr_auc\":0.9812,\"d3_60x_dilated\":0.9580,\"causal_prefix_max_delta\":2.1e-7}"}`,
				EffectClass: taint.EffectWorkspaceWrite,
			},
			StateHash:       "lala_v2_write",
			InputTokens:     270,
			CacheReadTokens: 3800,
			OutputTokens:    50,
		}, nil
	}
	return &dag.GenerateResponse{
		Reasoning:       "submit lala-l1.1-causal-dilated-v2 verified across Track D & Track E",
		Done:            true,
		FinalSummary:    "lala-l1.1-causal-dilated-v2 passes frozen Track D & Track E gates",
		StateHash:       "lala_v2_done",
		InputTokens:     190,
		CacheReadTokens: 4000,
		OutputTokens:    30,
	}, nil
}

// SeedLalaResearchContracts attaches the frozen Track D and Track E ArtifactOracles to runID.
func SeedLalaResearchContracts(ctx context.Context, st *store.Store, runID string, disallowedSHAs []string) error {
	trackDSpec, _ := json.Marshal(oracle.ArtifactOracleSpec{
		RequiredPaths:    []string{"artifacts/metrics.json"},
		DisallowedHashes: map[string][]string{"artifacts/metrics.json": disallowedSHAs},
		RequiredJSONKeys: map[string][]string{"artifacts/metrics.json": {"model", "macro_pr_auc", "d3_60x_dilated"}},
		MetricThresholds: []oracle.MetricThreshold{
			{Key: "macro_pr_auc", Op: ">=", Value: 0.97},
			{Key: "d3_60x_dilated", Op: ">=", Value: 0.90},
		},
	})
	if err := st.CreateContract(ctx, store.Contract{
		ID: "ctr_lala_track_d_" + runID, RunID: runID, Kind: "artifact_oracle", SpecJSON: string(trackDSpec), PinnedHash: "track_d_v1", Required: true,
	}); err != nil {
		return err
	}

	trackESpec, _ := json.Marshal(oracle.ArtifactOracleSpec{
		RequiredPaths:    []string{"artifacts/metrics.json"},
		RequiredJSONKeys: map[string][]string{"artifacts/metrics.json": {"causal_prefix_max_delta"}},
		MetricThresholds: []oracle.MetricThreshold{
			{Key: "causal_prefix_max_delta", Op: "<=", Value: 0.001},
		},
	})
	return st.CreateContract(ctx, store.Contract{
		ID: "ctr_lala_track_e_" + runID, RunID: runID, Kind: "artifact_oracle", SpecJSON: string(trackESpec), PinnedHash: "track_e_v1", Required: true,
	})
}

// lalaGroundTruth re-reads artifacts/metrics.json from a branch workspace and applies the frozen
// Track D / Track E thresholds directly, independent of any oracle verdict stored by the kernel.
func (r *Runner) lalaGroundTruth(ctx context.Context, branchID string) (bool, string) {
	br, err := r.Store.GetBranch(ctx, branchID)
	if err != nil {
		return false, "branch not found"
	}
	raw, err := os.ReadFile(filepath.Join(br.WorkspaceDir, "artifacts", "metrics.json"))
	if err != nil {
		return false, "no artifacts/metrics.json"
	}
	var m struct {
		Model  string  `json:"model"`
		PRAUC  float64 `json:"macro_pr_auc"`
		D3     float64 `json:"d3_60x_dilated"`
		Causal float64 `json:"causal_prefix_max_delta"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return false, "invalid artifacts/metrics.json"
	}
	ok := m.PRAUC >= 0.97 && m.D3 >= 0.90 && m.Causal <= 0.001
	return ok, fmt.Sprintf("final artifact `%s` (pr_auc=%.4f, track_d=%.4f, track_e=%.1e)", m.Model, m.PRAUC, m.D3, m.Causal)
}

func (r *Runner) runMLResearchShadow(ctx context.Context, wsDir string) (TaskResult, error) {
	start := time.Now()
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		return TaskResult{}, err
	}
	goal := "Beat Lala-l1 open_unseen_rt >= 0.97 while passing Track D (d3_60x_dilated >= 0.90) & Track E (causal <= 0.001)"

	prof := compiler.WorkerProfile()
	prof.MaxStalls = 2
	execDag := dag.NewExecutor(r.Store, NewLalaResearchProvider(), nil, prof)
	execDag.Verifier = oracle.NewVerifier(r.Store, r.Sandbox)

	runID, rootBranchID, err := execDag.StartRun(ctx, dag.RunRequest{
		Title:         "shadow-ml-lala-01",
		WorkspacePath: wsDir,
		WorkspaceKind: "research",
		GoalSpec:      goal,
		AccountIDs:    []string{"shadow-worker"},
	})
	if err != nil {
		return TaskResult{}, err
	}
	if err := SeedLalaResearchContracts(ctx, r.Store, runID, nil); err != nil {
		return TaskResult{}, err
	}

	searcher := dag.NewBranchSearcher(execDag)
	sRes, err := searcher.SearchExistingRun(ctx, runID, rootBranchID, dag.SearchConfig{
		MaxBranches:       4,
		MaxTurnsPerBranch: 4,
		AnchorTurn:        1,
	})
	if err != nil || !sRes.Completed {
		return TaskResult{}, fmt.Errorf("ml shadow search incomplete: %+v err=%v", sRes, err)
	}

	allExact := true
	totalChecked := 0
	for _, att := range sRes.Attempts {
		ok, n := inspectReplay(ctx, r.Store, att.BranchID)
		allExact = allExact && ok
		totalChecked += n
	}
	br, _ := r.Store.GetBranch(ctx, sRes.WinningBranchID)
	kernelOK, kernelDetail := r.lalaGroundTruth(ctx, sRes.WinningBranchID)

	// Ablation: identical script and identical oracles, but a single branch with no search and no
	// Graveyard, so the failed hypotheses never reach Layer 1.
	abWS := wsDir + "_ablation"
	if err := os.MkdirAll(abWS, 0o755); err != nil {
		return TaskResult{}, err
	}
	abExec := dag.NewExecutor(r.Store, NewLalaResearchProvider(), nil, compiler.WorkerProfile())
	abExec.Verifier = oracle.NewVerifier(r.Store, r.Sandbox)
	abRunID, abBrID, err := abExec.StartRun(ctx, dag.RunRequest{
		Title:         "shadow-ml-lala-01-ablation",
		WorkspacePath: abWS,
		WorkspaceKind: "research",
		GoalSpec:      goal,
		AccountIDs:    []string{"shadow-worker"},
	})
	if err != nil {
		return TaskResult{}, err
	}
	if err := SeedLalaResearchContracts(ctx, r.Store, abRunID, nil); err != nil {
		return TaskResult{}, err
	}
	abRes, abErr := abExec.ExecuteBranch(ctx, abBrID, 5)
	var vfErr *dag.VerificationFailedError
	if abErr != nil && !errors.As(abErr, &vfErr) {
		return TaskResult{}, fmt.Errorf("ml ablation: %w", abErr)
	}
	abOK, abDetail := r.lalaGroundTruth(ctx, abBrID)
	abOK = abOK && abRes.Status == dag.StatusCompleted

	return TaskResult{
		ID:                  "shadow-ml-lala-01",
		Category:            "ml_rd",
		Title:               "Deceptive local optimum in a non-git ML research task",
		Mechanism:           "`BranchSearcher` with cross-branch Graveyard",
		RunID:               runID,
		WinningBranchID:     sRes.WinningBranchID,
		HeadStepID:          br.HeadStepID,
		BranchesExplored:    len(sRes.Attempts),
		GraveyardEntries:    sRes.GraveyardCount,
		TurnsExecuted:       sRes.TotalTurns,
		KernelVerified:      kernelOK,
		KernelEvidence:      fmt.Sprintf("%d branch(es) explored, %d abandoned to the Graveyard; winning branch %s", len(sRes.Attempts), sRes.GraveyardCount, kernelDetail),
		AblationVerified:    abOK,
		AblationOutcome:     fmt.Sprintf("with a single branch the run ended `%s` after %d turns; %s", abRes.Status, abRes.TurnsExecuted, abDetail),
		ExactReplayVerified: allExact,
		ReplayStepsChecked:  totalChecked,
		DurationMS:          time.Since(start).Milliseconds(),
	}, nil
}

// --- Task 4: Multi-Subscription Quota Storm (429 Clean Epoch Failover + Recovery) ---

type quotaShadowProvider struct {
	tripped atomic.Bool
}

func (p *quotaShadowProvider) Generate(_ context.Context, req dag.GenerateRequest) (*dag.GenerateResponse, error) {
	if req.AccountID == "codex-plus" && req.TurnIndex == 2 && !p.tripped.Swap(true) {
		return nil, &dag.RateLimitError{
			AccountID:  req.AccountID,
			RetryAfter: 25 * time.Millisecond,
			Err:        errors.New("429 subscription window exhausted"),
		}
	}
	if req.TurnIndex == 1 {
		return &dag.GenerateResponse{
			Reasoning: "capture initial workspace telemetry",
			ToolCall: &dag.ToolCallSpec{
				Name:        "noop_probe",
				ArgsJSON:    `{}`,
				EffectClass: taint.EffectReadOnly,
			},
			StateHash:       "quota_t1",
			InputTokens:     220,
			CacheReadTokens: 2100,
			OutputTokens:    30,
		}, nil
	}
	return &dag.GenerateResponse{
		Reasoning:       "completed cleanly across clean Epoch 2 Handoff Packet",
		Done:            true,
		FinalSummary:    "finished on " + req.AccountID,
		StateHash:       "quota_done",
		InputTokens:     180,
		CacheReadTokens: 2300,
		OutputTokens:    25,
	}, nil
}

// runQuotaScript replays quotaShadowProvider (one 429 on the primary account) with or without failover.
func (r *Runner) runQuotaScript(ctx context.Context, wsDir, title string, failover bool) (*dag.BranchResult, string, string, error) {
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		return nil, "", "", err
	}
	execDag := dag.NewExecutor(r.Store, &quotaShadowProvider{}, nil, compiler.WorkerProfile())
	execDag.AllowFailover = failover
	execDag.AccountIDs = []string{"codex-plus", "xai-premium"}

	runID, brID, err := execDag.StartRun(ctx, dag.RunRequest{
		Title:         title,
		WorkspacePath: wsDir,
		GoalSpec:      "Complete task cleanly despite codex-plus 429 using Epoch 2 Handoff Packet failover",
		AccountIDs:    []string{"codex-plus", "xai-premium"},
	})
	if err != nil {
		return nil, "", "", err
	}
	res, err := execDag.ExecuteBranch(ctx, brID, 8)
	// Let the primary account's short cooldown lapse before anything else uses it.
	time.Sleep(35 * time.Millisecond)
	return res, runID, brID, err
}

func (r *Runner) runQuotaChaosShadow(ctx context.Context, wsDir string) (TaskResult, error) {
	start := time.Now()

	res, runID, brID, err := r.runQuotaScript(ctx, wsDir, "shadow-quota-01", true)
	if err != nil || res.Status != dag.StatusCompleted || res.Rollovers < 1 {
		return TaskResult{}, fmt.Errorf("quota chaos shadow failed: res=%+v err=%v", res, err)
	}
	exactOK, checked := inspectReplay(ctx, r.Store, brID)

	// Ablation: identical script and accounts, failover disabled. A suspension is an expected
	// outcome here, so the error is reported as evidence rather than treated as a harness failure.
	abRes, _, _, abErr := r.runQuotaScript(ctx, wsDir+"_ablation", "shadow-quota-01-ablation", false)
	abStatus, abTurns := dag.BranchStatus("unknown"), 0
	if abRes != nil {
		abStatus, abTurns = abRes.Status, abRes.TurnsExecuted
	}
	abOutcome := fmt.Sprintf("with failover disabled a single pass ended `%s` after %d turn(s)", abStatus, abTurns)
	if abErr != nil {
		abOutcome += " (suspended on the 429 instead of handing off)"
	}

	return TaskResult{
		ID:                  "shadow-quota-01",
		Category:            "quota_chaos",
		Title:               "HTTP 429 on the primary subscription mid-run",
		Mechanism:           "Epoch handoff failover across accounts",
		RunID:               runID,
		WinningBranchID:     brID,
		HeadStepID:          res.HeadStepID,
		BranchesExplored:    1,
		TurnsExecuted:       res.TurnsExecuted,
		KernelVerified:      true,
		KernelEvidence:      fmt.Sprintf("run ended `%s` in one pass after %d account rollover(s)", res.Status, res.Rollovers),
		AblationVerified:    abRes != nil && abRes.Status == dag.StatusCompleted,
		AblationOutcome:     abOutcome,
		ExactReplayVerified: exactOK,
		ReplayStepsChecked:  checked,
		DurationMS:          time.Since(start).Milliseconds(),
	}, nil
}

// inspectReplay re-derives every step hash of branchID in ModeExact.
func inspectReplay(ctx context.Context, st *store.Store, branchID string) (bool, int) {
	rep, err := replay.NewReplayer(st).Replay(ctx, branchID, replay.ReplayOptions{Mode: replay.ModeExact})
	if err != nil || rep == nil {
		return false, 0
	}
	return !rep.Diverged && rep.StepsChecked > 0 && rep.HashVerifiedCount == rep.StepsChecked, rep.StepsChecked
}

func computeScorecard(tasks []TaskResult) Scorecard {
	sc := Scorecard{Tasks: len(tasks)}
	if len(tasks) == 0 {
		return sc
	}
	exactPassed := 0
	for _, t := range tasks {
		if t.KernelVerified {
			sc.KernelVerified++
		}
		if t.AblationVerified {
			sc.AblationVerified++
		}
		if t.LoadBearing() {
			sc.MechanismsLoadBearing++
		}
		if t.ExactReplayVerified {
			exactPassed++
		}
	}
	sc.ExactReplayPassRate = 100.0 * float64(exactPassed) / float64(len(tasks))
	// The gate fails if any scenario stops verifying, any replay diverges, or any scenario would
	// pass just as well with its mechanism switched off (which would make it prove nothing).
	sc.GatePassed = sc.KernelVerified == sc.Tasks && sc.MechanismsLoadBearing == sc.Tasks && exactPassed == len(tasks)
	return sc
}
