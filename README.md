# soma — A Durable, Replayable, Capability-Scoped Agent Kernel

`soma` is an open-source local agent execution kernel written in pure Go (`CGO_ENABLED=0`), built as a systems-first alternative to linear chat-loop harnesses such as **Hermes** and **OpenClaw**.

Instead of mutating an append-only conversation transcript that directly executes side effects, `soma` treats every agent run as a **durable, content-addressed Merkle execution DAG** paired with a **cache-safe paged Context VM**.

> Architecture specification: [RFC-0001 (`soma` v2.0 Architecture)](docs/rfcs/0001-soma-v2-architecture.md)

---

## Core Thesis (Five Orthogonal Primitives)

| Primitive | What It Replaces | v0.1 Status |
|---|---|---|
| **1. Event-Sourced Execution DAG & Replay** | Mutable linear chat transcripts that lose work on crashes or HTTP 429s | Implemented (`internal/dag`, `internal/replay`) |
| **2. Cache-Safe Hybrid Paged Context VM (`@page`)** | Raw tool dumps & lossy LLM compaction thrashing | Implemented (`internal/vmem`, `internal/compiler`) |
| **3. Typed Capability Brokers & Fail-Closed Sandbox** | Prompt-level willpower & unprotected host environments | Kernel probe in v0.1; brokers/taint in v0.2 |
| **4. Bi-Temporal Memory Graph & Immutable Skills** | Flat `MEMORY.md` files & self-mutating prompt bloat | Schema + active rules/graveyard in v0.1; full graph/linter in v0.2 |
| **5. Verification Contracts & Quota Scheduler** | Self-graded completion reports & blind 429 retries | Quota/cooldown scheduler in v0.1; oracles/bridges in v0.2–v0.3 |

---

## v0.1 Features Included Today

- **Split-Pool Pure-Go SQLite WAL (`modernc.org/sqlite`) + zstd Blob Store (`internal/store`)**:
  - Dedicated single-connection writer (`BEGIN IMMEDIATE`) + concurrent reader pool (`query_only`).
  - Content-addressed zstd-compressed blobs (`.soma/blobs/`) keeping storage linear `O(T)` instead of quadratic `O(T^2)`.
- **Virtual Memory `@page` & Structural Previewers (`internal/vmem`)**:
  - Anti-syscall-tax inline thresholds (`3 KiB` worker / `16 KiB` frontier).
  - Deterministic type-aware structural previews (`log`, `code`, `json`, `tabular`, `diff`, `http`, `text`) + `page_slice` / `page_grep`.
- **Strict Prefix-Stability 4-Layer Context Compiler (`internal/compiler`)**:
  - Layers 0 and 1 are frozen per epoch; dynamic mid-epoch loads append strictly to Layer 2's tail to prevent prefix KV-cache invalidation.
  - Hybrid Epoch Rollover preserves compact reasoning/action skeletons and failed-branch Graveyard entries while masking older raw observations.
- **Merkle DAG, Branch Forking, 429 Suspension & Deterministic Replay (`internal/dag`, `internal/replay`)**:
  - Durable `SUSPENDED_QUOTA` persistence on HTTP 429 rate limits and zero-loss resume.
  - Speculative `soma fork` branching and automatic `negative_path` Graveyard capture on abandoned branches.
  - Side-effect-free `soma replay` for offline prompt/model counterfactual debugging.
- **CLI & 3-Boundary X-Ray (`cmd/soma`, `internal/cli`)**:
  - Built-in `--mock` provider mode for instant zero-key local evaluation and demos.

---

## Quickstart

```bash
# Build single static binary (zero CGO)
make build

# Initialize .soma/ in a workspace (proves bwrap / Linux Landlock ABI availability)
./bin/soma init --dir /tmp/demo

# Run a task with the built-in deterministic mock provider
./bin/soma run --dir /tmp/demo --mock --goal "Diagnose webhook race condition"

# Inspect runs, steps, 3-boundary X-Ray, speculative forks, and offline replay
./bin/soma status --dir /tmp/demo
./bin/soma xray <step_id> --dir /tmp/demo
./bin/soma fork <step_id> --dir /tmp/demo --label "alt-approach"
./bin/soma replay <branch_id> --dir /tmp/demo --mock
```

---

## Reproducing Benchmarks (`benchmarks/`)

`soma` evaluates itself against anonymized failure modes captured from live Hermes & OpenClaw workloads:

```bash
# Run full test suite + B1 & B2 benchmark verification under Go race detector
make test

# Run B1 (Log-Storm & Cache Efficiency) and B2 (429 Chaos & Crash Durability)
make bench
```

### Suites Implemented in v0.1
- **Suite B1 (`benchmarks/b1_log_storm_test.go`)**: 50-turn debugging run injecting five 200 KB logs plus mid-epoch skill/MCP loads, verified against an unpaged append-only baseline (`0` LLM compactions, `0` Layer-0/1 cache busts, `>= 94%` prefix cache hit rate, `< 24k` peak compiled context, `>= 40%` token savings).
- **Suite B2 (`benchmarks/b2_chaos_durability_test.go`)**: 20 concurrent/sequential tasks under injected `HTTP 429` storms and hard simulated process crashes (`100%` completion after cooldown/resume, `0` lost steps, `0` `SQLITE_BUSY` errors).
