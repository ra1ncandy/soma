package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/ra1ncandy/soma/internal/compiler"
	"github.com/ra1ncandy/soma/internal/dag"
	"github.com/ra1ncandy/soma/internal/replay"
	"github.com/ra1ncandy/soma/internal/store"
)

const Version = "v0.1.0 (RFC-0001 v2.1 Durable Core)"

type projectConfig struct {
	Version       int    `yaml:"version"`
	DefaultModel  string `yaml:"default_model"`
	DefaultTier   string `yaml:"default_tier"`
	WorkspaceKind string `yaml:"workspace_kind"`
	APIBaseURL    string `yaml:"api_base_url,omitempty"`
}

// NewRootCmd constructs the soma CLI command tree.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "soma",
		Short: "soma — A Durable, Replayable, Capability-Scoped Agent Kernel",
	}

	root.AddCommand(
		newVersionCmd(),
		newInitCmd(),
		newRunCmd(),
		newResumeCmd(),
		newForkCmd(),
		newReplayCmd(),
		newReconcileCmd(),
		newXRayCmd(),
		newStatusCmd(),
	)
	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print soma kernel version",
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintln(cmd.OutOrStdout(), "soma", Version)
		},
	}
}

func newInitCmd() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize .soma/ kernel state and probe OS sandbox support",
		RunE: func(cmd *cobra.Command, _ []string) error {
			absDir, err := filepath.Abs(dir)
			if err != nil {
				return err
			}
			st, err := openStore(absDir)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			wsKind := "generic"
			if _, err := os.Stat(filepath.Join(absDir, ".git")); err == nil {
				wsKind = "git"
			}

			cfgPath := filepath.Join(st.SomaDir(), "config.yaml")
			if _, err := os.Stat(cfgPath); errors.Is(err, os.ErrNotExist) {
				cfg := projectConfig{
					Version:       2,
					DefaultModel:  "mock-worker-v1",
					DefaultTier:   "worker",
					WorkspaceKind: wsKind,
					APIBaseURL:    os.Getenv("SOMA_API_BASE"),
				}
				raw, err := yaml.Marshal(cfg)
				if err != nil {
					return err
				}
				if err := os.WriteFile(cfgPath, raw, 0o644); err != nil {
					return err
				}
			}

			if err := ensureDefaultAccount(cmd.Context(), st, "mock-worker-v1"); err != nil {
				return err
			}

			bwrapStatus, llABI, ready := probeSandboxEnvironment()
			badge := "DEGRADED"
			if ready {
				badge = "READY"
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Initialized soma workspace: %s\n", absDir)
			fmt.Fprintf(out, "  SQLite WAL:  %s\n", st.DBPath())
			fmt.Fprintf(out, "  Workspace:   %s\n", wsKind)
			fmt.Fprintf(out, "  Sandbox:     [%s] bwrap=%s, landlock_abi=%d\n", badge, bwrapStatus, llABI)
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", ".", "Target workspace directory")
	return cmd
}

func newRunCmd() *cobra.Command {
	var dir, goal, profileName string
	var useMock bool
	var maxTurns int

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Start a new durable DAG run",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(goal) == "" {
				return errors.New("--goal is required")
			}
			absDir, err := filepath.Abs(dir)
			if err != nil {
				return err
			}
			st, err := openStore(absDir)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			prof := selectProfile(profileName)
			prov, modelID, err := selectProvider(absDir, useMock)
			if err != nil {
				return err
			}
			if err := ensureDefaultAccount(cmd.Context(), st, modelID); err != nil {
				return err
			}

			exec := dag.NewExecutor(st, prov, nil, prof)
			runID, branchID, err := exec.StartRun(cmd.Context(), dag.RunRequest{
				WorkspacePath: absDir,
				GoalSpec:      goal,
				Profile:       prof,
			})
			if err != nil {
				return err
			}

			res, execErr := exec.ExecuteBranch(cmd.Context(), branchID, maxTurns)
			printBranchResult(cmd.OutOrStdout(), runID, branchID, res)
			return execErr
		},
	}
	cmd.Flags().StringVar(&dir, "dir", ".", "Workspace directory")
	cmd.Flags().StringVar(&goal, "goal", "", "Task goal specification")
	cmd.Flags().StringVar(&profileName, "profile", "worker", "Model profile (worker|frontier)")
	cmd.Flags().BoolVar(&useMock, "mock", false, "Use deterministic local simulation provider")
	cmd.Flags().IntVar(&maxTurns, "max-turns", 20, "Maximum turns to execute")
	return cmd
}

func newResumeCmd() *cobra.Command {
	var dir, profileName string
	var useMock bool
	var maxTurns int

	cmd := &cobra.Command{
		Use:   "resume <branch_id>",
		Short: "Resume a suspended or queued DAG branch",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			absDir, err := filepath.Abs(dir)
			if err != nil {
				return err
			}
			st, err := openStore(absDir)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			branch, err := st.GetBranch(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			prof := selectProfile(profileName)
			prov, modelID, err := selectProvider(absDir, useMock)
			if err != nil {
				return err
			}
			if err := ensureDefaultAccount(cmd.Context(), st, modelID); err != nil {
				return err
			}

			exec := dag.NewExecutor(st, prov, nil, prof)
			res, execErr := exec.ExecuteBranch(cmd.Context(), branch.ID, maxTurns)
			printBranchResult(cmd.OutOrStdout(), branch.RunID, branch.ID, res)
			return execErr
		},
	}
	cmd.Flags().StringVar(&dir, "dir", ".", "Workspace directory")
	cmd.Flags().StringVar(&profileName, "profile", "worker", "Model profile (worker|frontier)")
	cmd.Flags().BoolVar(&useMock, "mock", false, "Use deterministic local simulation provider")
	cmd.Flags().IntVar(&maxTurns, "max-turns", 20, "Maximum turns to execute")
	return cmd
}

func newForkCmd() *cobra.Command {
	var dir, label string
	cmd := &cobra.Command{
		Use:   "fork <step_id>",
		Short: "Fork a speculative branch with an isolated workspace from step_id",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			absDir, err := filepath.Abs(dir)
			if err != nil {
				return err
			}
			st, err := openStore(absDir)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			step, err := st.GetStep(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			exec := dag.NewExecutor(st, nil, nil, compiler.WorkerProfile())
			newBranchID, err := exec.ForkBranch(cmd.Context(), step.RunID, step.ID, label)
			if err != nil {
				return err
			}
			br, err := st.GetBranch(cmd.Context(), newBranchID)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Forked branch %s from %s (run=%s)\n  workspace=%s (%s)\n",
				br.ID, step.ID, br.RunID, br.WorkspaceDir, br.WorkspaceStrategy)
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", ".", "Workspace directory")
	cmd.Flags().StringVar(&label, "label", "fork", "Label for the new branch")
	return cmd
}

func newReplayCmd() *cobra.Command {
	var dir, modeStr, fromStep, model string
	var asJSON, useMock bool

	cmd := &cobra.Command{
		Use:   "replay <branch_id>",
		Short: "Replay a branch in exact, counterfactual, or sandbox_branch mode",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			absDir, err := filepath.Abs(dir)
			if err != nil {
				return err
			}
			st, err := openStore(absDir)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			mode := replay.Mode(modeStr)
			var prov dag.ModelProvider
			if mode != replay.ModeExact {
				p, _, err := selectProvider(absDir, useMock)
				if err != nil {
					return err
				}
				prov = p
			}

			rep, err := replay.NewReplayer(st).Replay(cmd.Context(), args[0], replay.ReplayOptions{
				Mode:          mode,
				FromStepID:    fromStep,
				OverrideModel: model,
				Provider:      prov,
			})
			if err != nil {
				return err
			}

			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(rep)
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Replay mode=%s source=%s steps_checked=%d hash_verified=%d diverged=%v\n",
				rep.Mode, rep.SourceBranchID, rep.StepsChecked, rep.HashVerifiedCount, rep.Diverged)
			if rep.SandboxBranchID != "" {
				fmt.Fprintf(out, "  sandbox_branch=%s\n", rep.SandboxBranchID)
			}
			if rep.Diverged {
				fmt.Fprintf(out, "  diverged_at=%s reason=%s\n", rep.DivergedAtStep, rep.DivergenceReason)
			}
			fmt.Fprintf(out, "  tokens: orig=%d replay=%d\n", rep.OriginalTotalTokens, rep.ReplayTotalTokens)
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", ".", "Workspace directory")
	cmd.Flags().StringVar(&modeStr, "mode", string(replay.ModeExact), "Replay mode (exact|counterfactual|sandbox_branch)")
	cmd.Flags().StringVar(&fromStep, "from-step", "", "Step ID to fork from in sandbox_branch mode")
	cmd.Flags().StringVar(&model, "model", "", "Override model ID for counterfactual/sandbox replay")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit JSON report")
	cmd.Flags().BoolVar(&useMock, "mock", false, "Use mock provider for counterfactual/sandbox_branch")
	return cmd
}

func newReconcileCmd() *cobra.Command {
	var dir, outcome, summary string
	cmd := &cobra.Command{
		Use:   "reconcile <action_id>",
		Short: "Resolve an ambiguous action_journal entry and unblock its branch",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if outcome != "succeeded" && outcome != "failed" {
				return errors.New("--outcome must be succeeded or failed")
			}
			absDir, err := filepath.Abs(dir)
			if err != nil {
				return err
			}
			st, err := openStore(absDir)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			exec := dag.NewExecutor(st, nil, nil, compiler.WorkerProfile())
			if err := exec.ReconcileAction(cmd.Context(), args[0], dag.ReconcileOutcome(outcome), summary); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Reconciled action %s as %s\n", args[0], outcome)
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", ".", "Workspace directory")
	cmd.Flags().StringVar(&outcome, "outcome", "", "Observed external outcome (succeeded|failed)")
	cmd.Flags().StringVar(&summary, "summary", "", "Optional operator note")
	return cmd
}

func newXRayCmd() *cobra.Command {
	var dir string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "xray <step_id>",
		Short: "Inspect 3 boundaries (Raw @page | Verified Reconstructed Frame | Emitted Output)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			absDir, err := filepath.Abs(dir)
			if err != nil {
				return err
			}
			st, err := openStore(absDir)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			step, err := st.GetStep(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			frame, err := compiler.ReconstructFrame(cmd.Context(), st, step.ID, compiler.WorkerProfile())
			if err != nil {
				return err
			}
			emBytes, err := st.Blobs().Get(step.EmittedBlobHash)
			if err != nil {
				return err
			}
			var em dag.EmittedPayload
			_ = json.Unmarshal(emBytes, &em)

			var pagePreview string
			if step.ObservationPageID != "" {
				if p, pErr := st.GetPage(cmd.Context(), step.ObservationPageID); pErr == nil {
					pagePreview = fmt.Sprintf("@%s (%s, %d bytes, %s)\n%s",
						p.ID, p.ContentType, p.ByteSize, p.TrustTier, p.PreviewText)
				}
			}

			if asJSON {
				outObj := map[string]any{
					"step":               step,
					"reconstructed_hash": frame.CompiledFrameHash,
					"hash_verified":      frame.CompiledFrameHash == step.CompiledFrameHash,
					"frame_tokens": map[string]int{
						"L0": frame.L0Tokens, "L1": frame.L1Tokens, "L2": frame.L2Tokens, "L3": frame.L3Tokens, "total": frame.TotalTokens,
					},
					"page_preview": pagePreview,
					"emitted":      em,
				}
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(outObj)
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "=== SOMA 3-BOUNDARY X-RAY: %s (epoch=%d turn=%d) ===\n", step.ID, step.Epoch, step.TurnIndex)
			fmt.Fprintf(out, "[1] RAW OBSERVATION PAGE\n")
			if pagePreview == "" {
				fmt.Fprintln(out, "  (inline / no @page spilled)")
			} else {
				fmt.Fprintln(out, pagePreview)
			}
			fmt.Fprintf(out, "\n[2] RECONSTRUCTED WIRE FRAME (SHA-256 VERIFIED: %s)\n", frame.CompiledFrameHash[:16])
			fmt.Fprintf(out, "  PrefixHashL0L1: %s\n", frame.PrefixHashL0L1[:16])
			fmt.Fprintf(out, "  Tokens: L0=%d | L1=%d | L2=%d | L3=%d | Total=%d\n",
				frame.L0Tokens, frame.L1Tokens, frame.L2Tokens, frame.L3Tokens, frame.TotalTokens)
			fmt.Fprintf(out, "\n[3] EMITTED MODEL RESPONSE & PROVENANCE\n")
			fmt.Fprintf(out, "  Model: %s (account=%s) | Latency: %dms\n", step.ModelID, step.AccountID, step.LatencyMS)
			fmt.Fprintf(out, "  Tokens: in=%d cache_read=%d out=%d | InputPages=%s\n",
				step.InputTokens, step.CacheReadTokens, step.OutputTokens, step.InputProvenanceJSON)
			fmt.Fprintf(out, "  Reasoning: %s\n", em.Response.Reasoning)
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", ".", "Workspace directory")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit JSON report")
	return cmd
}

func newStatusCmd() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Display runs, branches, pending inbox/actions, accounts, and disk usage",
		RunE: func(cmd *cobra.Command, _ []string) error {
			absDir, err := filepath.Abs(dir)
			if err != nil {
				return err
			}
			st, err := openStore(absDir)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			ctx := cmd.Context()
			runs, err := st.ListRuns(ctx)
			if err != nil {
				return err
			}
			branches, err := st.ListBranches(ctx, "")
			if err != nil {
				return err
			}
			pendingActions, err := st.ListPendingActions(ctx, "")
			if err != nil {
				return err
			}
			accounts, err := st.ListAccounts(ctx)
			if err != nil {
				return err
			}
			dbBytes, _ := st.DBSizeBytes()
			blobBytes, _ := st.Blobs().TotalBytes()

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "soma workspace: %s\n", absDir)
			fmt.Fprintf(out, "Storage: sqlite=%d bytes | zstd_blobs=%d bytes\n\n", dbBytes, blobBytes)

			fmt.Fprintf(out, "Runs (%d):\n", len(runs))
			for _, r := range runs {
				fmt.Fprintf(out, "  - %s [%s] kind=%s goal=%q\n", r.ID, r.Status, r.WorkspaceKind, r.GoalSpec)
			}

			fmt.Fprintf(out, "\nBranches (%d):\n", len(branches))
			for _, b := range branches {
				steps, _ := st.ListBranchSteps(ctx, b.ID)
				fmt.Fprintf(out, "  - %s (run=%s) status=%s epoch=%d steps=%d strategy=%s head=%s\n",
					b.ID, b.RunID, b.Status, b.Epoch, len(steps), b.WorkspaceStrategy, b.HeadStepID)
			}

			fmt.Fprintf(out, "\nPending/Ambiguous Actions (%d):\n", len(pendingActions))
			for _, a := range pendingActions {
				fmt.Fprintf(out, "  - %s branch=%s tool=%s class=%s status=%s\n",
					a.ActionID, a.BranchID, a.ToolName, a.EffectClass, a.Status)
			}

			fmt.Fprintf(out, "\nAccounts (%d):\n", len(accounts))
			nowMs := time.Now().UnixMilli()
			for _, a := range accounts {
				cool := "ready"
				if a.CooldownUntil > nowMs {
					cool = fmt.Sprintf("cooldown(%ds)", (a.CooldownUntil-nowMs)/1000)
				}
				fmt.Fprintf(out, "  - %s model=%s mode=%s leases=%d/%d state=%s\n",
					a.ID, a.ModelID, a.BillingMode, a.ActiveLeases, a.MaxConcurrent, cool)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", ".", "Workspace directory")
	return cmd
}

func openStore(workspaceDir string) (*store.Store, error) {
	return store.Open(filepath.Join(workspaceDir, ".soma"))
}

func selectProfile(name string) compiler.ModelProfile {
	if strings.EqualFold(name, "frontier") {
		return compiler.FrontierProfile()
	}
	return compiler.WorkerProfile()
}

func selectProvider(workspaceDir string, useMock bool) (dag.ModelProvider, string, error) {
	if useMock {
		return &dag.MockProvider{ModelName: "mock-worker-v1"}, "mock-worker-v1", nil
	}
	baseURL := os.Getenv("SOMA_API_BASE")
	model := os.Getenv("SOMA_MODEL")
	cfgPath := filepath.Join(workspaceDir, ".soma", "config.yaml")
	if raw, err := os.ReadFile(cfgPath); err == nil {
		var cfg projectConfig
		if yaml.Unmarshal(raw, &cfg) == nil {
			if baseURL == "" {
				baseURL = cfg.APIBaseURL
			}
			if model == "" {
				model = cfg.DefaultModel
			}
		}
	}
	if baseURL == "" {
		return nil, "", errors.New("no provider configured (pass --mock or set SOMA_API_BASE)")
	}
	if model == "" {
		model = "gpt-6.2-sol"
	}
	return dag.NewOpenAICompatProvider(baseURL, os.Getenv("SOMA_API_KEY")), model, nil
}

func ensureDefaultAccount(ctx context.Context, st *store.Store, modelID string) error {
	accs, err := st.ListAccounts(ctx)
	if err != nil {
		return err
	}
	if len(accs) > 0 {
		return nil
	}
	return st.UpsertAccount(ctx, store.Account{
		ID:            "default",
		Provider:      "local",
		ModelID:       modelID,
		BillingMode:   "local",
		MaxConcurrent: 2,
	})
}

func printBranchResult(w interface{ Write([]byte) (int, error) }, runID, branchID string, res *dag.BranchResult) {
	if res == nil {
		return
	}
	_, _ = fmt.Fprintf(w, "Run: %s | Branch: %s | Status: %s | Turns: %d | Head: %s\n",
		runID, branchID, res.Status, res.TurnsExecuted, res.HeadStepID)
	if res.FinalSummary != "" {
		_, _ = fmt.Fprintf(w, "Summary: %s\n", res.FinalSummary)
	}
}
