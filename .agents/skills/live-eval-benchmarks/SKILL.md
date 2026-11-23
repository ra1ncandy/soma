---
name: live-eval-benchmarks
description: >-
  Run, debug, and extend soma's live LLM A/B evaluations (benchmarks/live_eval_test.go) and
  6-scenario Hard Gauntlet (benchmarks/live_hard_test.go), including offline fixture/baseline pins
  (live_baseline_test.go, live_hard_offline_test.go). Use when working on live benchmarks,
  baseline fairness, or evaluating model runs.
---

# Live LLM Evaluations & Hard Gauntlet

## Key Files

- `benchmarks/live_baseline_test.go` — Offline unit pins ensuring the linear baseline preserves summaries across compactions, shows the summarizer the latest tool result, and matches `soma`'s turn budget
- `benchmarks/live_hard_offline_test.go` — Offline pins for Hard1–Hard6 fixtures, anti-cheese traps, and reference solutions (zero API cost)
- `benchmarks/live_eval_test.go` — Original 3-task live A/B evaluation (`TestLiveEvaluation_HypothesisAB`)
- `benchmarks/live_hard_test.go` — 6-scenario Hard Gauntlet (`TestLiveHard_Gauntlet`)

## Step 1: Verify Offline Pins First (Always)

Before spending any live API tokens, confirm all offline baseline and Hard Gauntlet pins pass:

```bash
go test -count=1 -race -v -run "TestLiveBaseline|TestLiveHardOffline" ./benchmarks/
```

(If test names differ, run the full offline benchmark package without API keys set: `go test -count=1 -race -v ./benchmarks/`.)

## Step 2: Run Live Evaluations

Live tests are skipped unless `SOMA_LIVE_API_KEY` (or fallback `OPENCODE_API_KEY`) is set.

### 3-Task A/B Evaluation

```bash
SOMA_LIVE_API_KEY="$KEY" \
SOMA_LIVE_BASE_URL="https://opencode.ai/zen/go/v1" \
SOMA_LIVE_MODEL="deepseek-v4.1-flash" \
SOMA_LIVE_REPEATS=3 \
SOMA_LIVE_OUT="live_results.jsonl" \
go test -count=1 -v -run TestLiveEvaluation_HypothesisAB ./benchmarks/
```

### 6-Scenario Hard Gauntlet

```bash
SOMA_LIVE_API_KEY="$KEY" \
SOMA_LIVE_MODELS="deepseek-v4.1-flash,glm-5.3-flash" \
SOMA_LIVE_REPEATS=3 \
SOMA_LIVE_OUT="live_hard_results.jsonl" \
go test -count=1 -v -timeout 45m -run TestLiveHard_Gauntlet ./benchmarks/
```

Optional environment variables:
- `SOMA_LIVE_MAX_TOKENS` (default `16384`)
- `SOMA_LIVE_REASONING_EFFORT`

## Reporting Rules

- Never mix offline B1–B6 conformance figures with live LLM evaluation figures.
- Report pass counts per model alongside the raw `SOMA_LIVE_OUT` JSONL log.
- State clearly which scenarios are fair no-oracle A/B comparisons (`Exp1`, `Hard1`), which test oracle feedback (`Exp2`, `Exp3`, `Hard2`, `Hard3`), and which are `soma`-only stress tests (`Hard4`–`Hard6`).
- Do not commit temporary `*.jsonl` output files to git.
