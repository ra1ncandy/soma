package shadow

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func passFail(ok bool) string {
	if ok {
		return "PASS"
	}
	return "FAIL"
}

// RenderMarkdown formats the Suite B6 conformance report from GauntletReport.
func RenderMarkdown(rep GauntletReport) string {
	var b strings.Builder
	sc := rep.Scorecard
	b.WriteString("# `soma` Kernel Conformance & Ablation Report (Suite B6)\n\n")
	fmt.Fprintf(&b, "- **Generated At**: `%s`\n", rep.GeneratedAt)
	fmt.Fprintf(&b, "- **Kernel Version**: `%s`\n", rep.SomaVersion)
	fmt.Fprintf(&b, "- **Suite B6 Gate**: **%s** (kernel verified `%d/%d`, mechanisms load-bearing `%d/%d`, exact replay `%.0f%%`)\n\n",
		passFail(sc.GatePassed), sc.KernelVerified, sc.Tasks, sc.MechanismsLoadBearing, sc.Tasks, sc.ExactReplayPassRate)

	b.WriteString("## What this report is, and is not\n\n")
	b.WriteString("This is a **deterministic conformance test of kernel mechanisms**, not a model-quality or\n")
	b.WriteString("harness-versus-harness benchmark. Every scenario is driven by a **scripted provider** that replays a\n")
	b.WriteString("fixed sequence of tool calls, including a deliberate mistake (a premature `Done`, an injected shell\n")
	b.WriteString("command, a deceptive local optimum, a 429). No LLM is called.\n\n")
	b.WriteString("Each scenario runs that same script twice: once with the kernel mechanism under test **enabled**, once\n")
	b.WriteString("with it **disabled**. Both outcomes are then checked against ground truth read back from the workspace\n")
	b.WriteString("or the fake host, not from the script's own claim of completion. A mechanism is *load-bearing* when the\n")
	b.WriteString("enabled run verifies and the disabled run does not.\n\n")
	b.WriteString("It therefore shows that the mechanisms work as specified and that the scenarios would fail without\n")
	b.WriteString("them. It says nothing about how often a real model makes these mistakes, about token cost or cache\n")
	b.WriteString("behaviour, or about any other agent harness. For live-model results see `benchmarks/live_eval_test.go`.\n\n")

	b.WriteString("---\n\n## 1. Scorecard\n\n")
	b.WriteString("| Measured outcome | Result | Gate |\n|---|---:|---|\n")
	fmt.Fprintf(&b, "| Scenarios verified with mechanism enabled | **%d/%d** | all |\n", sc.KernelVerified, sc.Tasks)
	fmt.Fprintf(&b, "| Scenarios verified with mechanism disabled | %d/%d | reported, not gated |\n", sc.AblationVerified, sc.Tasks)
	fmt.Fprintf(&b, "| Mechanisms shown load-bearing | **%d/%d** | all |\n", sc.MechanismsLoadBearing, sc.Tasks)
	fmt.Fprintf(&b, "| SHA-256 `ModeExact` replay pass rate | **%.0f%%** | `100%%` |\n\n", sc.ExactReplayPassRate)

	b.WriteString("---\n\n## 2. Scenario Results\n\n")
	b.WriteString("| Task ID | Mechanism under test | Enabled | Disabled | Branches | Graveyard | Turns | Replay (`exact`) |\n")
	b.WriteString("|---|---|---|---|---:|---:|---:|---|\n")
	for _, t := range rep.Tasks {
		rStatus := "FAIL"
		if t.ExactReplayVerified {
			rStatus = fmt.Sprintf("PASS (%d steps)", t.ReplayStepsChecked)
		}
		fmt.Fprintf(&b, "| `%s` | %s | **%s** | %s | %d | %d | %d | %s |\n",
			t.ID, t.Mechanism, passFail(t.KernelVerified), passFail(t.AblationVerified),
			t.BranchesExplored, t.GraveyardEntries, t.TurnsExecuted, rStatus)
	}
	b.WriteString("\n### Evidence\n\n")
	for _, t := range rep.Tasks {
		fmt.Fprintf(&b, "- **`%s`** (%s)\n  - Enabled: %s\n  - Disabled: %s\n", t.ID, t.Title, t.KernelEvidence, t.AblationOutcome)
	}

	if bl := rep.Baseline; bl != nil {
		b.WriteString("\n---\n\n## 3. External Reference Telemetry (operator-supplied)\n\n")
		b.WriteString("> Supplied by the operator via `--baseline`. **Not measured, reproduced, or verified by this suite**,\n")
		b.WriteString("> and not used in any score above.\n\n")
		fmt.Fprintf(&b, "- **Source**: %s\n", bl.SourceLabel)
		fmt.Fprintf(&b, "- Compactions: `%d` across `%d` sessions\n", bl.Compactions, bl.Sessions)
		fmt.Fprintf(&b, "- HTTP 429 deaths: `%d/%d`; orphan reaps: `%d/%d`\n", bl.RateLimitDeaths, bl.RateLimitTotal, bl.OrphanReaps, bl.OrphanTotal)
		fmt.Fprintf(&b, "- Blocked unattended tool calls: `%d`; curator ledger edits: `%d`\n", bl.BlockedUnattendedCalls, bl.CuratorMutations)
	}

	b.WriteString("\n---\n\n## Reproduce\n\n")
	b.WriteString("```bash\n")
	b.WriteString("# Full test suite + B1-B6 under -race\n")
	b.WriteString("go test -count=1 -race -v ./benchmarks/...\n\n")
	b.WriteString("# Regenerate this report\n")
	b.WriteString("make shadow\n")
	b.WriteString("```\n\n")
	b.WriteString("Branch and step IDs are generated per run; `soma shadow run --dir <dir>` keeps the store so each\n")
	b.WriteString("winning branch can be re-checked with `soma replay <branch> --mode exact`.\n")
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
