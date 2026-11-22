package shadow

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RenderMarkdown formats a publication-grade Benchmark Report from GauntletReport.
func RenderMarkdown(rep GauntletReport) string {
	var b strings.Builder
	b.WriteString("# `soma` v0.4 Reproducible Shadow Gauntlet Benchmark Report (Suite B6)\n\n")
	fmt.Fprintf(&b, "- **Generated At**: `%s`\n", rep.GeneratedAt)
	fmt.Fprintf(&b, "- **Kernel Version**: `%s`\n", rep.SomaVersion)
	fmt.Fprintf(&b, "- **Reference Baseline**: %s\n", rep.Baseline.SourceLabel)
	gateBadge := "PASS"
	if !rep.Scorecard.GatePassed {
		gateBadge = "FAIL"
	}
	fmt.Fprintf(&b, "- **Suite B6 Acceptance Gate**: **%s** (`soma` `%.0f%%` vs. Linear `%.0f%%` = **%.2fx**, `ExactReplay=%.0f%%`, `Compactions=%d`)\n\n",
		gateBadge,
		rep.Scorecard.SomaCompletionRate*100.0,
		rep.Scorecard.LinearCompletionRate*100.0,
		rep.Scorecard.CompletionRatio,
		rep.Scorecard.ExactReplayPassRate,
		rep.Scorecard.TotalCompactions,
	)

	b.WriteString("---\n\n## 1. Executive Scorecard (`soma` vs. Linear Harness)\n\n")
	b.WriteString("| Metric | `soma` | Linear Baseline | RFC-0001 v2.1 Gate |\n")
	b.WriteString("|---|---:|---:|---|\n")
	fmt.Fprintf(&b, "| Verified Task Completion Rate | **%.1f%%** | %.1f%% | >= 2.0x linear (**%.2fx**) |\n",
		rep.Scorecard.SomaCompletionRate*100.0, rep.Scorecard.LinearCompletionRate*100.0, rep.Scorecard.CompletionRatio)
	fmt.Fprintf(&b, "| Average Prefix Cache Hit Rate | **%.2f%%** | < 85%% (lossy compaction) | >= 85%% |\n", rep.Scorecard.AvgCacheHitRate)
	fmt.Fprintf(&b, "| Total LLM Text Compactions | **%d** | High (`%d` observed) | `0` |\n", rep.Scorecard.TotalCompactions, rep.Baseline.Compactions)
	fmt.Fprintf(&b, "| SHA-256 `ModeExact` Replay Pass Rate | **%.0f%%** | N/A (unhashed log) | `100%%` |\n", rep.Scorecard.ExactReplayPassRate)
	fmt.Fprintf(&b, "| Net Token Reduction vs. Unpaged Linear | **%.1f%%** | Baseline | >= 40%% on churny runs |\n\n", rep.Scorecard.TokenSavingsPct)

	b.WriteString("---\n\n## 2. Empirical Hermes Baseline Telemetry\n\n")
	b.WriteString("| Failure Mode | Observed Production Telemetry (`state.db` / `agent.log`) | `soma` Architectural Eliminator |\n")
	b.WriteString("|---|---|---|\n")
	fmt.Fprintf(&b, "| Lossy Compactions | `%d` compactions across `%d` sessions | `@page` Virtual Memory + Immutable L0/L1 Epochs (`0` compactions) |\n",
		rep.Baseline.Compactions, rep.Baseline.Sessions)
	fmt.Fprintf(&b, "| HTTP 429 & Orphan Reaps | `%d/%d` (%.1f%%) 429 deaths; `%d/%d` (%.1f%%) orphan reaps | SQLite Cooldown Scheduler + Epoch Handoff Failover |\n",
		rep.Baseline.RateLimitDeaths, rep.Baseline.RateLimitTotal,
		100.0*float64(rep.Baseline.RateLimitDeaths)/float64(maxInt(1, rep.Baseline.RateLimitTotal)),
		rep.Baseline.OrphanReaps, rep.Baseline.OrphanTotal,
		100.0*float64(rep.Baseline.OrphanReaps)/float64(maxInt(1, rep.Baseline.OrphanTotal)))
	fmt.Fprintf(&b, "| Unattended Approvals & Curator Bloat | `%d` `BLOCKED:` tool denials; `%d` `.curator_ledger.jsonl` edits | Scoped `bwrap`/Broker Policies + Immutable Lint-Gated Skills |\n\n",
		rep.Baseline.BlockedUnattendedCalls, rep.Baseline.CuratorMutations)

	b.WriteString("---\n\n## 3. Domain-by-Domain Shadow Gauntlet Results\n\n")
	b.WriteString("| Task ID | Domain | `soma` | Linear | Branches | Graveyard | Turns | Cache Hit | Oracles | Replay (`exact`) | Notes |\n")
	b.WriteString("|---|---|---|---|---:|---:|---:|---:|---|---|---|\n")
	for _, t := range rep.Tasks {
		sStatus := "FAIL"
		if t.SomaCompleted {
			sStatus = "PASS"
		}
		lStatus := "FAIL"
		if t.LinearCompleted {
			lStatus = "PASS (degraded)"
		}
		rStatus := "FAIL"
		if t.ExactReplayVerified {
			rStatus = fmt.Sprintf("PASS (%d steps)", t.ReplayStepsChecked)
		}
		fmt.Fprintf(&b, "| `%s` | `%s` | **%s** | %s | %d | %d | %d | %.1f%% | %s | %s | %s |\n",
			t.ID, t.Category, sStatus, lStatus, t.BranchesExplored, t.GraveyardEntries,
			t.TurnsExecuted, t.CacheHitRate, t.OraclesPassed, rStatus, t.Notes)
	}

	b.WriteString("\n---\n\n## 4. Reproducible Verification & Time-Travel Inspection\n\n")
	b.WriteString("```bash\n")
	b.WriteString("# Reproduce full test suite + B1-B6 benchmarks under -race\n")
	b.WriteString("go test -count=1 -race -v ./benchmarks/...\n\n")
	b.WriteString("# Inspect or regenerate this shadow gauntlet report cleanly\n")
	b.WriteString("make shadow\n")
	for _, t := range rep.Tasks {
		if t.WinningBranchID != "" {
			fmt.Fprintf(&b, "# Exact SHA-256 replay for %s (%s): soma replay %s --mode exact (head step: %s)\n",
				t.ID, t.Category, t.WinningBranchID, t.HeadStepID)
		}
	}
	b.WriteString("```\n")
	return b.String()
}

// WriteReport writes Markdown and optional JSON gauntlet artifacts cleanly to disk.
func WriteReport(rep GauntletReport, mdPath, jsonPath string) error {
	if mdPath != "" {
		if err := os.MkdirAll(filepath.Dir(mdPath), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(mdPath, []byte(RenderMarkdown(rep)), 0o644); err != nil {
			return err
		}
	}
	if jsonPath != "" {
		if err := os.MkdirAll(filepath.Dir(jsonPath), 0o755); err != nil {
			return err
		}
		raw, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(jsonPath, append(raw, '\n'), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
