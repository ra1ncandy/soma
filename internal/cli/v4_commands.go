package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ra1ncandy/soma/internal/dag"
	"github.com/ra1ncandy/soma/internal/oracle"
	"github.com/ra1ncandy/soma/internal/sandbox"
	"github.com/ra1ncandy/soma/internal/shadow"
)

func newShadowCmd() *cobra.Command {
	shCmd := &cobra.Command{
		Use:   "shadow",
		Short: "Run and format the reproducible Suite B6 Shadow Gauntlet (soma vs. Hermes/linear)",
	}

	var dir, baselinePath, outMD, outJSON string
	runCmd := &cobra.Command{
		Use:   "run",
		Short: "Execute all 4 reproducible shadow gauntlet scenarios and print/write the scorecard",
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

			base := shadow.DefaultHermesBaseline()
			if strings.TrimSpace(baselinePath) != "" {
				base, err = shadow.LoadBaselineJSON(baselinePath)
				if err != nil {
					return err
				}
			}

			rep, err := shadow.NewRunner(st, sandbox.NewManager(), Version).RunGauntlet(cmd.Context(), base)
			if err != nil {
				return err
			}

			if outMD != "" || outJSON != "" {
				if err := shadow.WriteReport(rep, outMD, outJSON); err != nil {
					return err
				}
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Shadow Gauntlet Complete (gate_passed=%v)\n", rep.Scorecard.GatePassed)
			fmt.Fprintf(out, "  Completion:   soma=%.1f%% vs linear=%.1f%% (ratio=%.2fx)\n",
				rep.Scorecard.SomaCompletionRate*100.0, rep.Scorecard.LinearCompletionRate*100.0, rep.Scorecard.CompletionRatio)
			fmt.Fprintf(out, "  Cache Hit:    %.2f%% | Compactions: %d | Exact Replay: %.0f%%\n",
				rep.Scorecard.AvgCacheHitRate, rep.Scorecard.TotalCompactions, rep.Scorecard.ExactReplayPassRate)
			for _, t := range rep.Tasks {
				fmt.Fprintf(out, "  - %-16s [soma=%-4v linear=%-5v] branches=%d graveyard=%d replay=%v (%d steps)\n",
					t.ID, t.SomaCompleted, t.LinearCompleted, t.BranchesExplored, t.GraveyardEntries, t.ExactReplayVerified, t.ReplayStepsChecked)
			}
			if outMD != "" {
				fmt.Fprintf(out, "Wrote Markdown report: %s\n", outMD)
			}
			if outJSON != "" {
				fmt.Fprintf(out, "Wrote JSON report:     %s\n", outJSON)
			}
			if !rep.Scorecard.GatePassed {
				return errors.New("shadow gauntlet failed Suite B6 acceptance gate")
			}
			return nil
		},
	}
	runCmd.Flags().StringVar(&dir, "dir", ".", "Workspace directory for shadow .soma store")
	runCmd.Flags().StringVar(&baselinePath, "baseline", "", "Optional JSON file overriding Hermes baseline metrics")
	runCmd.Flags().StringVar(&outMD, "out-md", "", "Optional output path for Markdown benchmark report")
	runCmd.Flags().StringVar(&outJSON, "out-json", "", "Optional output path for JSON benchmark report")

	var inJSON, repOutMD string
	reportCmd := &cobra.Command{
		Use:   "report",
		Short: "Render a Markdown Benchmark Report from a saved gauntlet JSON file",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(inJSON) == "" {
				return errors.New("--json is required")
			}
			raw, err := os.ReadFile(inJSON)
			if err != nil {
				return err
			}
			var rep shadow.GauntletReport
			if err := json.Unmarshal(raw, &rep); err != nil {
				return err
			}
			if repOutMD != "" {
				if err := shadow.WriteReport(rep, repOutMD, ""); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Rendered Markdown report -> %s\n", repOutMD)
				return nil
			}
			fmt.Fprint(cmd.OutOrStdout(), shadow.RenderMarkdown(rep))
			return nil
		},
	}
	reportCmd.Flags().StringVar(&inJSON, "json", "", "Saved GauntletReport JSON path")
	reportCmd.Flags().StringVar(&repOutMD, "out-md", "", "Optional output Markdown path (prints to stdout when omitted)")

	shCmd.AddCommand(runCmd, reportCmd)
	return shCmd
}

func newSearchCmd() *cobra.Command {
	var dir, goal, profileName string
	var useMock bool
	var maxBranches, maxTurns int

	cmd := &cobra.Command{
		Use:   "search",
		Short: "Execute bounded speculative branching search with Stall Breaker & Graveyard backtracking",
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
			exec.Verifier = oracle.NewVerifier(st, sandbox.NewManager())
			searcher := dag.NewBranchSearcher(exec)
			res, err := searcher.Search(cmd.Context(), dag.RunRequest{
				WorkspacePath: absDir,
				GoalSpec:      goal,
				Profile:       prof,
			}, dag.SearchConfig{
				MaxBranches:       maxBranches,
				MaxTurnsPerBranch: maxTurns,
				AnchorTurn:        1,
			})
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Branch Search Run=%s Completed=%v Winner=%s Graveyard=%d Turns=%d\n",
				res.RunID, res.Completed, res.WinningBranchID, res.GraveyardCount, res.TotalTurns)
			for _, att := range res.Attempts {
				fmt.Fprintf(out, "  - %-10s branch=%s status=%s turns=%d rollovers=%d passed=%v\n",
					att.Label, att.BranchID, att.Status, att.Turns, att.Rollovers, att.Passed)
			}
			if !res.Completed {
				return errors.New("branch search exhausted max-branches without passing verification")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", ".", "Workspace directory")
	cmd.Flags().StringVar(&goal, "goal", "", "Task goal specification")
	cmd.Flags().StringVar(&profileName, "profile", "worker", "Model profile (worker|frontier)")
	cmd.Flags().BoolVar(&useMock, "mock", false, "Use deterministic local simulation provider")
	cmd.Flags().IntVar(&maxBranches, "max-branches", 4, "Maximum branches to explore")
	cmd.Flags().IntVar(&maxTurns, "max-turns", 8, "Maximum turns per branch")
	return cmd
}
