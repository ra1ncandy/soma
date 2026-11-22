# soma — A Durable, Replayable, Capability-Scoped Agent Kernel

[![CI](https://github.com/ra1ncandy/soma/actions/workflows/ci.yml/badge.svg)](https://github.com/ra1ncandy/soma/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/ra1ncandy/soma)](https://goreportcard.com/report/github.com/ra1ncandy/soma)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

`soma` is an open-source agent execution kernel written in pure Go (`CGO_ENABLED=0`), engineered from first principles as a systems-grade alternative to linear chat-loop harnesses such as **[Hermes](https://github.com/nousresearch/hermes-agent)** and **OpenClaw**.

Instead of treating an autonomous agent as an append-only chat array that dumps raw tool outputs into context, compacts history via lossy LLM summaries, and self-grades its own work, `soma` executes every run as a **durable, content-addressed Merkle execution DAG** featuring:

- **4-Layer Monotonic Prompt Compiler (`Layer 0`–`Layer 3`)** engineered for **85%–98.6% KV-cache reuse** and zero mid-epoch prefix invalidation.
- **Content-Addressed Virtual Memory (`@page_XXXX`)** with zstd blob spilling, structural previewers, `page_grep`, `page_slice`, and **Matryoshka `int8` + BM25 Hybrid RRF `page_search`**.
- **Bi-Temporal Provenance Memory Graph** with point-in-time `QueryAsOf`, *MINJA* untrusted quarantine, semantic conflict detection, and **Epoch-Pinned Top-$K$ Hybrid Retrieval**.
- **External Verification Oracles (`CommandOracle`, `ArtifactOracle`, Zero-History `AdvisoryReviewer`)** with kernel `EROFS` read-only test pinning that eliminate self-graded completion lies.
- **Speculative Branching (`soma search`) & Concurrent Multi-Agent Swarms (`soma swarm`)** with isolated per-branch Git worktrees, a cache-safe **`Layer 2` Blackboard bus**, **3-way `git merge-file` workspace merging**, and a cross-branch **Graveyard (`negative_path`)**.
- **Fail-Closed OS Sandboxing (`bwrap` / Landlock / Seatbelt / Docker / Remote SSH)** paired with **Secretless Capability Brokers** and **Data/Action-Level Provenance Taint Tracking**.
- **Oracle-Verified SFT & Contrastive DPO/GRPO Trajectory Export (`soma export-trajectories`)** pairing verified winning branches against Graveyard-abandoned sibling branches from their exact shared fork ancestor.

> - Architecture Specification: [RFC-0001 (`soma` v2.1 Architecture)](docs/rfcs/0001-soma-v2-architecture.md)
> - Reproducible Shadow Gauntlet Scorecard: [Benchmark Report (Suite B6)](docs/benchmarks/BENCHMARK_REPORT.md)
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
│  • Linux Landlock v4 / macOS Seatbelt  │  │  • fsnotify + HMAC-SHA256 Webhooks│
│  • Ephemeral Docker & Remote SSH       │  │  • 30s Debounce Coalescer         │
│  • Secretless HTTP/SSH/FS Brokers      │  │  • 4 Hard Circuit Brakes & TTL GC │
│  • Data/Action Provenance Taint        │  │                                   │
└────────────────────────────────────────┘  └───────────────────────────────────┘
```

---

## 2. Empirical Benchmark Results

### A. Live LLM A/B Evaluation (`soma` vs. Linear Chat Baseline)

Executed live via `benchmarks/live_eval_test.go` (`TestLiveEvaluation_HypothesisAB`) against real tool-calling models (`beam-free` and `hy4-flash-free`):
- **Overall Live Pass Rate**: **`soma` passed `5/5` (`100%`)** across both models vs. **`2/5` (`40%`)** for the Hermes-style append-only Linear Chat Baseline.
- **Controlled Warm Turn KV-Cache Probe**: **`98.6%` real server-side KV cache hit (`2,816 / 2,856 cached_tokens`)** in `711 ms`.

| Experiment (`beam-free`) | System | Passed | Turns | Prompt Tokens | Uncached Input Tokens | Cached Tokens (Hit %) | Compactions | Exact SHA-256 Replay | Wall Time | Outcome / Verification Detail |
| :--- | :--- | :---: | :---: | :---: | :---: | :---: | :---: | :---: | :---: | :--- |
| **Exp1: 280 KB Log Storm & Needle** | `Linear` | `false` | 10 | `140,152` | `137,272` | `2,880` (`2.1%`) | 9 | `false` | `17,389 ms` | Compaction thrash wiped needle (`missing diagnosis.json`) |
| **Exp1: 280 KB Log Storm & Needle** | **`soma`** | **`true`** | **6** | **`12,485` (-91.1%)** | **`7,109` (-94.8%)** | **`5,376` (`43.1%`)** | **0** | **`true`** | **`5,144 ms` (3.4x faster)** | `yaml_4250=true error_code="SOMA_CRC_7741" timeout_ms=4250` |
| **Exp2: Held-Out Oracle vs Self-Grade** | `Linear` | `false` | 3 | `1,036` | `140` | `896` (`86.5%`) | 0 | `false` | `11,497 ms` | Self-graded completion lie (`AssertionError: precedence failed`) |
| **Exp2: Held-Out Oracle vs Self-Grade** | **`soma`** | **`true`** | **7** | **`12,502`** | **`6,230`** | **`6,272` (`50.2%`)** | **0** | **`true`** | **`17,358 ms`** | `HELD_OUT_ORACLE_OK (oracle_runs=1 status=completed)` |
| **Exp3: Deceptive ML Local Optimum** | `Linear` | `true` | 9 | `28,792` | `10,872` | `17,920` (`62.2%`) | 0 | `false` | `32,894 ms` | `mode=causal_multiscale dev=0.9812 d3=0.9580 causal=2.1e-07` |
| **Exp3: Deceptive ML Local Optimum** | **`soma`** | **`true`** | **9** | **`18,698` (-35.1%)** | **`10,314`** | **`8,384` (`44.8%`)** | **0** | **`true`** | **`37,159 ms`** | `mode=causal_multiscale dev=0.9812 d3=0.9580 causal=2.1e-07` |

### B. Offline Verification Suites (`benchmarks/` — Suites B1–B6)

| Suite | What It Verifies | Key Acceptance Gate |
|---|---|---|
| **B1** (`b1_log_storm_test.go`) | 50 turns, five 200 KB logs, mid-epoch skill & MCP loads | `0` compactions, **`94.7%` cache hit**, `< 24k` peak tokens, **`88.3%` token savings**, **`13.3%` (`7.5x` smaller) delta storage**, `100%` SHA-256 frame reconstruction |
| **B2** (`b2_chaos_durability_test.go`) | 20 concurrent chaos tasks + 429 storms + crashes between dispatch and commit | `20/20` completion, `0` lost steps, `0` `SQLITE_BUSY`, `0` duplicate `irreversible_write` executions |
| **B2/v0.3** (`b2_v3_surface_test.go`) | Multi-sub 429 failover, 100 unchanged sentinel ticks + 20 burst webhooks, Discord/Telegram Unix IPC bridge & concurrent 4-tab TUI | `0` tokens on unchanged sentinels, `20 -> 1` burst coalescing, clean Epoch 2 Handoff Packet + primary recovery, concurrent TUI/IPC zero `SQLITE_BUSY` |
| **B3** (`b3_memory_antipoison_test.go`) | 30 sequential rule updates (`SUPERSEDES`/`MERGES`) + *MINJA* injection + skill immutability | `100%` current & point-in-time recall, `0%` untrusted rule activation, `0` silent `SKILL.md` mutations |
| **B4** (`b4_sandbox_confused_deputy_test.go`) | Real `bwrap` isolation + poisoned `SKILL.md` + `MCP 2026-07-28` Confused Deputy + SSH injection/leases | `0` leaked bytes/network, `100%` Confused Deputy `POST` blocked until deterministic `DeclassifyStructured`, SSH injection rejected |
| **B5** (`b5_oracles_reward_hacking_test.go`) | *ImpossibleBench* pinned test tampering + stale checksum (`p181`/`p183`) & Track D collapse | `100%` premature `Done` rejected until fresh verified artifact passes |
| **B6** (`b6_branching_shadow_gauntlet_test.go`) | Non-git ML deceptive local optimum (`Lala-l1.1` Track D/E trap) + 4-domain Shadow Gauntlet vs. Hermes/linear | `BranchSearcher` escapes via Graveyard backtracking; **`100%` completion (`4.0x` linear)**, `0` compactions, `100%` `ModeExact` SHA-256 replay |

---

## 3. Key Subsystems

- **16 Canonical Workspace, Virtual Memory, Blackboard & Multimodal Tools (`internal/dag`)**:
  - **Filesystem & Forgiving Patching**: `fs_list`, `fs_read`, `fs_write`, and multi-hunk `fs_patch` (supporting `<<<<<<< SEARCH ... ======= ... >>>>>>> REPLACE`, JSON `search`/`replace`, and unified diffs with 3-tier exact/trimmed/indentation-fuzzy matching and atomic rollback).
  - **Sandboxed Execution & `@page` Background Jobs**: Synchronous `exec` (`bwrap --unshare-net --unshare-pid`) plus asynchronous `bg_exec`, `bg_poll`, and `bg_kill` whose live output streams automatically spill to `@page_XXXX` handles.
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

# 7. Launch the 4-tab Bubble Tea v2 interactive TUI or run the Shadow Gauntlet
./bin/soma ui --dir /tmp/demo
make shadow
```

---

## 5. License

Released under the [MIT License](LICENSE).
