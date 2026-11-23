# Benchmarks & Evidence Honesty Rules

Follow these rules whenever touching `benchmarks/`, `internal/shadow/`, `README.md`, or benchmark reports:

1. **Separate Offline Conformance from Live LLM Evaluations**
   - **Offline Conformance Suites (`benchmarks/b1_*`–`b6_*`, `internal/shadow`)** use scripted providers and no LLM. They prove kernel mechanism conformance (prefix immutability, durability, taint blocking, oracle gating, replay determinism). Never present them as model-quality or harness-vs-harness benchmarks.
   - **Live LLM Evaluations (`benchmarks/live_eval_test.go`, `benchmarks/live_hard_test.go`)** run against real tool-calling models. Always label which evidence class a number comes from.

2. **Reproducible, Measured Numbers Only**
   - Never hardcode a baseline's or competitor's outcome in tests, reports, or docs. Every published number must be produced by code in this repository and reproducible from it.
   - Do not claim the repository's minimal linear baseline (`live_eval_test.go`) represents Hermes, OpenClaw, or any external harness.

3. **Fair Baseline Parity**
   - Any A/B baseline must receive the same turn budget and the same task information as `soma` (or any intentional difference — such as oracle feedback in `Exp2`/`Exp3`/`Hard2`/`Hard3` — must be explicitly stated next to the result).
   - Keep `benchmarks/live_baseline_test.go` and `benchmarks/live_hard_offline_test.go` green so baseline compaction/turn-budget invariants and Hard Gauntlet fixtures/anti-cheese traps stay pinned offline.
