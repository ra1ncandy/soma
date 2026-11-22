# soma — A Durable, Replayable, Capability-Scoped Agent Kernel

[![CI](https://github.com/ra1ncandy/soma/actions/workflows/ci.yml/badge.svg)](https://github.com/ra1ncandy/soma/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/ra1ncandy/soma)](https://goreportcard.com/report/github.com/ra1ncandy/soma)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

`soma` is an open-source agent execution kernel written in pure Go (`CGO_ENABLED=0`), engineered from first principles as a systems-grade alternative to linear chat-loop harnesses such as **[Hermes](https://github.com/nousresearch/hermes-agent)** and **OpenClaw**.

Instead of treating an autonomous agent as an append-only chat array that dumps raw tool outputs into context, compacts history via lossy LLM summaries, and self-grades its own work, `soma` executes every run as a **durable, content-addressed Merkle execution DAG** featuring:

- **4-Layer Monotonic Prompt Compiler (`Layer 0`–`Layer 3`)** engineered to keep the prompt prefix byte-stable within an epoch (zero mid-epoch prefix invalidation), so provider-side KV caches can be reused.
- **Content-Addressed Virtual Memory (`@page_XXXX`)** with zstd blob spilling, structural previewers, `page_grep`, `page_slice`, and **Matryoshka `int8` + BM25 Hybrid RRF `page_search`**.
- **Bi-Temporal Provenance Memory Graph** with point-in-time `QueryAsOf`, *MINJA* untrusted quarantine, semantic conflict detection, and **Epoch-Pinned Top-$K$ Hybrid Retrieval**.
- **External Verification Oracles (`CommandOracle`, `ArtifactOracle`, Zero-History `AdvisoryReviewer`)** with kernel `EROFS` read-only test pinning that eliminate self-graded completion lies.
- **Speculative Branching (`soma search`) & Concurrent Multi-Agent Swarms (`soma swarm`)** with isolated per-branch Git worktrees, a cache-safe **`Layer 2` Blackboard bus**, **3-way `git merge-file` workspace merging**, and a cross-branch **Graveyard (`negative_path`)**.
- **Fail-Closed OS Sandboxing (`bwrap` / Seatbelt / Docker / Remote SSH; Landlock is probed but not yet an enforcement backend)** paired with **Secretless Capability Brokers** and **Data/Action-Level Provenance Taint Tracking**.
- **Oracle-Verified SFT & Contrastive DPO/GRPO Trajectory Export (`soma export-trajectories`)** pairing verified winning branches against Graveyard-abandoned sibling branches from their exact shared fork ancestor.

> - Architecture Specification: [RFC-0001 (`soma` v2.1 Architecture)](docs/rfcs/0001-soma-v2-architecture.md)
> - Deterministic Kernel Conformance & Ablation Report: [Suite B6](docs/benchmarks/BENCHMARK_REPORT.md)
> - Security & Threat Model: [SECURITY.md](SECURITY.md)

---

## 1. Architecture Overview

```text
┌───────────────────────────────────────────────────────────────────────────────┐
│                            OPERATOR & BRIDGE SURFACE                          │
│  CLI (`soma run/search/swarm/export-trajectories`)  │  Bubble Tea v2 4-Tab TUI│
│  Unix-Socket JSON-RPC Bridge (`soma-bridge-discord`, TelegramAdapter)         │
└───────────────────────────────────────┬───────────────────────────────────────┘
                                        │
┌───────────────────────────────────────▼───────────────────────────────────────┐
│                               SOMA KERNEL CORE                                │
│                                                                               │
│  ┌─────────────────────────────────┐  ┌────────────────────────────────────┐  │
│  │  4-LAYER MONOTONIC COMPILER     │  │  MERKLE DAG & SWARM COORDINATOR    │  │
│  │  L0: Frozen Kernel & Syscalls   │  │  • 2-Phase Durable Action Journal  │  │
│  │  L1: Frozen Epoch Registers     │◄─┤  • BranchSearcher & Stall Breaker  │  │
│  │      (Top-K RRF Memory + Grave) │  │  • Concurrent Swarm (Coop & Comp)  │  │
│  │  L2: Append-Only Working Log    │  │  • Layer-2 Blackboard Peer Bus     │  │
│  │      (Turns + Blackboard + MCP) │  │  • 3-Way Worktree Merge Engine     │  │
│  │  L3: Ephemeral Live Tail        │  │  • Multi-Account QuotaScheduler    │  │
│  └────────────────┬────────────────┘  └─────────────────┬──────────────────┘  │
│                   │                                     │                     │
│  ┌────────────────▼────────────────┐  ┌─────────────────▼──────────────────┐  │
│  │  VIRTUAL MEMORY (@page) & RRF   │  │  VERIFICATION & REPLAY ENGINES     │  │
│  │  • zstd Content-Addressed Blobs │  │  • CommandOracle (EROFS pinned)    │  │
│  │  • Structural Previewers        │  │  • ArtifactOracle (JSON/Metrics)   │  │
│  │  • Code-Aware BM25 + 256d int8  │  │  • Zero-History AdvisoryReviewer   │  │
│  │    Matryoshka Vector RRF        │  │  • 3-Mode Replay & SFT/DPO Export  │  │
│  └────────────────┬────────────────┘  └─────────────────┬──────────────────┘  │
│                   └──────────────────┬──────────────────┘                     │
│  ┌───────────────────────────────────▼─────────────────────────────────────┐  │
│  │        Pure-Go Split-Pool SQLite WAL (`.soma/state.db` + `blobs/`)      │  │
│  │  runs | branches | epoch_snapshots | steps | pages | action_journal     │  │
│  │  memory_nodes | embeddings | contracts | accounts | trigger_runs | inbox│  │
│  └─────────────────────────────────────────────────────────────────────────┘  │
└───────────────────┬─────────────────────────────────────────┬─────────────────┘
                    │                                         │
┌───────────────────▼────────────────────┐  ┌─────────────────▼─────────────────┐
│   FAIL-CLOSED SANDBOX & TAINT ENGINE   │  │    TIER-0 PROACTIVITY DAEMON      │
│  • Linux bwrap (--unshare-net/pid)     │  │  • 0-Token Shell/SQL Sentinels    │
│  • macOS Seatbelt (Landlock probe only) │  │  • fsnotify + HMAC-SHA256 Webhooks│
│  • Ephemeral Docker & Remote SSH       │  │  • 30s Debounce Coalescer         │
│  • Secretless HTTP/SSH/FS Brokers      │  │  • 4 Hard Circuit Brakes & TTL GC │
│  • Data/Action Provenance Taint        │  │                                   │
└────────────────────────────────────────┘  └───────────────────────────────────┘
```

---

## 2. Verification & Evaluation

There are two different kinds of evidence here. Please do not read one as the other.

### A. Offline Conformance Suites (`benchmarks/` — Suites B1–B6)

These run in CI with **scripted providers and no LLM**. They show that each kernel mechanism does what the RFC specifies. They are not model-quality or harness-versus-harness benchmarks, and token or cache figures in them come from a simulated provider that assumes any byte-stable prefix is cached.

| Suite | What It Verifies | Key Acceptance Gate |
|---|---|---|
| **B1** (`b1_log_storm_test.go`) | 50 turns, five 200 KB logs, mid-epoch skill & MCP loads | `0` compactions, `100%` SHA-256 frame reconstruction, prefix stays byte-stable (simulated cache hit `>= 94%`), `>= 40%` fewer prompt tokens than the same run with outputs inlined, delta storage under `15%` of full-frame storage |
| **B2** (`b2_chaos_durability_test.go`) | 20 concurrent chaos tasks + 429 storms + crashes between dispatch and commit | `20/20` completion, `0` lost steps, `0` `SQLITE_BUSY`, `0` duplicate `irreversible_write` executions |
| **B2/v0.3** (`b2_v3_surface_test.go`) | Multi-sub 429 failover, 100 unchanged sentinel ticks + 20 burst webhooks, Discord/Telegram Unix IPC bridge & concurrent 4-tab TUI | `0` tokens on unchanged sentinels, `20 -> 1` burst coalescing, clean Epoch 2 Handoff Packet + primary recovery, concurrent TUI/IPC zero `SQLITE_BUSY` |
| **B3** (`b3_memory_antipoison_test.go`) | 30 sequential rule updates (`SUPERSEDES`/`MERGES`) + *MINJA* injection + skill immutability | `100%` current & point-in-time recall, `0%` untrusted rule activation, `0` silent `SKILL.md` mutations |
| **B4** (`b4_sandbox_confused_deputy_test.go`) | Real `bwrap` isolation + poisoned `SKILL.md` + `MCP 2026-07-28` Confused Deputy + SSH injection/leases | `0` leaked bytes/network, `100%` Confused Deputy `POST` blocked until deterministic `DeclassifyStructured`, SSH injection rejected |
| **B5** (`b5_oracles_reward_hacking_test.go`) | *ImpossibleBench* pinned test tampering + stale checksum (`p181`/`p183`) & Track D collapse | `100%` premature `Done` rejected until fresh verified artifact passes |
| **B6** (`b6_branching_shadow_gauntlet_test.go`) | Scripted deceptive-local-optimum search + the 4-scenario [conformance gauntlet](docs/benchmarks/BENCHMARK_REPORT.md): each scenario replays one script with its kernel mechanism enabled and disabled, and checks ground truth both times | `BranchSearcher` escapes via Graveyard backtracking; `4/4` scenarios verified with the mechanism on and `0/4` with it off; `100%` `ModeExact` SHA-256 replay |

### B. Live LLM A/B Evaluation (`benchmarks/live_eval_test.go`)

`TestLiveEvaluation_HypothesisAB` runs three tasks against a real tool-calling model, once through `soma` and once through a minimal append-only chat loop with LLM summarization that is written for this test. It is skipped unless `SOMA_LIVE_API_KEY` is set.

```bash
SOMA_LIVE_API_KEY=... SOMA_LIVE_BASE_URL=... SOMA_LIVE_MODEL=... \
SOMA_LIVE_REPEATS=10 SOMA_LIVE_OUT=live_results.jsonl \
go test -count=1 -v -run TestLiveEvaluation_HypothesisAB ./benchmarks/
```

**No headline numbers are published here yet.** Earlier versions of this README reported `5/5` vs. `2/5` and a `98.6%` cache hit. Those figures were withdrawn because the baseline was not a fair one: it discarded its own summary at every compaction, never showed the summarizer the latest tool result, and was given fewer turns than `soma`. All three are fixed (and pinned by `live_baseline_test.go`), but the evaluation has not been re-run since.

When reading results from this test, keep in mind:

- The baseline is a simple loop written for this repository. It is **not** Hermes, OpenClaw, or any other real harness.
- Exp2 and Exp3 give `soma` an external oracle that feeds failing assertions back to the model, and give the baseline no verifier. They measure the value of verifier feedback, not of the rest of the kernel.
- Three tasks is a small sample. Use `SOMA_LIVE_REPEATS` and report pass counts per model, with the raw `SOMA_LIVE_OUT` log.

---

## 3. Key Subsystems

- **16 Canonical Workspace, Virtual Memory, Blackboard & Multimodal Tools (`internal/dag`)**:
  - **Filesystem & Forgiving Patching**: `fs_list`, `fs_read`, `fs_write`, and multi-hunk `fs_patch` (supporting `<<<<<<< SEARCH ... ======= ... >>>>>>> REPLACE`, JSON `search`/`replace`, and unified diffs with 3-tier exact/trimmed/indentation-fuzzy matching and atomic rollback).
  - **Sandboxed Execution & `@page` Background Jobs**: Synchronous `exec` and asynchronous `bg_exec` both run inside the same fail-closed sandbox (`bwrap --unshare-net --unshare-pid`), with `bg_poll` and `bg_kill`, whose live output streams automatically spill to `@page_XXXX` handles.
  - **Virtual Memory & Hybrid RRF Retrieval**: `page_grep`, `page_slice`, `page_search` (40-line chunked Hybrid BM25 + 256-dim Matryoshka `int8` Vector RRF), and `memory_search`.
  - **Multi-Agent Blackboard**: `publish_finding(summary, page_id?)` broadcasts discoveries and `@page` handles to sibling agents into `Layer 2` without invalidating `Layer 0 + Layer 1` KV cache.
  - **Provenance-Tainted Web & Vision**: `web_search`, `web_extract` (both tagged `TrustTier: "untrusted_external"`), and `vision_analyze`.
- **Matryoshka `int8` + Code-Aware BM25 Hybrid RRF Engine (`internal/embed`, `internal/memory`, `internal/vmem`)**:
  - Works offline out-of-the-box with zero network calls via deterministic `LocalSimHashEmbedder + BM25` RRF.
  - Plugs into any `/v1/embeddings` endpoint (`qwen3-embedding:0.6b`, `jina-code-embeddings`, `embeddinggemma`, `codestral-embed`, `voyage-code-3`, `gemini-embedding-001`) via `SOMA_EMBED_URL` and `SOMA_EMBED_MODEL`, automatically applying asymmetric `Instruct: ...\nQuery: ` prefixes and 256-dim `int8` Matryoshka quantization (`256 bytes/vector` in SQLite).
- **Concurrent Multi-Agent Swarm Coordinator (`internal/dag/swarm.go`, `internal/workspace/merge.go`)**:
  - **`--mode cooperative`**: Runs $K$ parallel workers in isolated Git worktrees/snapshots sharing `PrefixHashL0L1`, merges their results via 3-way `git merge-file` (`MergeBranchWorkspaces`), and gates completion on the root `ContractVerifier` oracle.
  - **`--mode competitive`**: Runs $K$ competing hypotheses in parallel; failed or stalled branches immediately broadcast live `graveyard_notice` warnings to still-running sibling workers.
- **Oracle-Verified SFT & Contrastive DPO/GRPO Trajectory Exporter (`internal/replay/exporter.go`)**:
  - `soma export-trajectories --format sft --verified-only`: Exports ShareGPT / OpenAI tool-calling trajectories strictly from completed branches passing external verification contracts.
  - `soma export-trajectories --format dpo --verified-only`: Pairs winning branches (`ChosenTurns`) against Graveyard-abandoned sibling branches (`RejectedTurns` + `GraveyardReason`) from their shared `PromptAtFork` ancestor for DPO/GRPO reinforcement learning.

---

## 4. Quickstart

```bash
# 1. Build static pure-Go binaries (bin/soma and bin/soma-bridge-discord, CGO_ENABLED=0)
make build

# 2. Initialize .soma/ in a workspace (probes bwrap / Linux Landlock ABI / macOS Seatbelt)
./bin/soma init --dir /tmp/demo

# 3. Run a durable DAG task with a live OpenAI-compatible model (or --mock offline)
SOMA_API_BASE="https://api.openai.com/v1" \
SOMA_API_KEY="$OPENAI_API_KEY" \
SOMA_MODEL="gpt-4.1" \
./bin/soma run --dir /tmp/demo --goal "Inspect logs and fix timeout configuration"

# 4. Run Graveyard-guided speculative branch search
./bin/soma search --dir /tmp/demo --mock --goal "Explore competing hypotheses safely"

# 5. Run a concurrent multi-agent cooperative swarm (Blackboard + 3-way worktree merge + oracle)
./bin/soma swarm \
  --dir /tmp/demo \
  --mock \
  --mode cooperative \
  --goal "Implement RPC timeout handler and integration tests" \
  --subgoal "backend:Implement compute_timeout() in backend.py and publish finding" \
  --subgoal "tests:Write test_app.py verifying backend.py contract from Blackboard"

# 6. Export oracle-verified SFT trajectories or contrastive DPO/GRPO preference pairs
./bin/soma export-trajectories --dir /tmp/demo --format sft --verified-only --out sft.jsonl
./bin/soma export-trajectories --dir /tmp/demo --format dpo --verified-only --out dpo.jsonl

# 7. Launch the 4-tab Bubble Tea v2 interactive TUI or regenerate the conformance report
./bin/soma ui --dir /tmp/demo
make shadow
```

---

## 5. License

Released under the [MIT License](LICENSE).
