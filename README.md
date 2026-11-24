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
- **Fail-Closed OS Sandboxing (`bwrap` / Seatbelt / Docker / Remote SSH; Landlock is probed but not yet an enforcement backend)** paired with **Secretless Capability Brokers**, **Data/Action-Level Provenance Taint Tracking**, opt-in **Sandbox Network Access**, and **Standard / High-Autonomy** execution modes.
- **Web Dashboard (`soma web`) & Isolated Virtual Desktop (`soma-desktop`)** featuring live SSE step streaming, compact collapsible thinking/tool traces, isolated workspace **Review Diff & Apply to Project** (or `Direct` mode), embedded **Cron/Webhook Automations & 11 Channel Bridges** (Discord, Telegram, Slack, Matrix, Email, etc. with DM pairing), **49+ one-click MCP Connectors**, **Cloudflare Tunnel** remote access, and a **noVNC Virtual Desktop** with **Take Over / View-Only** protection, idle auto-stop, and container-scoped package managers (`sudo apt-get`, `pip`, `npm`).
- **Oracle-Verified SFT & Contrastive DPO/GRPO Trajectory Export (`soma export-trajectories`)** pairing verified winning branches against Graveyard-abandoned sibling branches from their exact shared fork ancestor.

> - Architecture Specification: [RFC-0001 (`soma` v2.1 Architecture)](docs/rfcs/0001-soma-v2-architecture.md)
> - Deterministic Kernel Conformance & Ablation Report: [Suite B6](docs/benchmarks/BENCHMARK_REPORT.md)
> - Security & Threat Model: [SECURITY.md](SECURITY.md)

---

## 1. Architecture Overview

```text
┌───────────────────────────────────────────────────────────────────────────────┐
│                            OPERATOR & BRIDGE SURFACE                          │
│  Web Dashboard (`soma web` · SSE, Diff/Apply, Virtual Desktop noVNC, Tunnel)  │
│  CLI (`soma run/search/swarm/mcp/bridge/export-trajectories`) │ 4-Tab TUI     │
│  11 Channel Bridges (Discord, Telegram, Slack, Matrix, Email, Webhooks, IPC)  │
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
│  │        Pure-Go Split-Pool SQLite WAL (`.soma/project.db` + `blobs/`)    │  │
│  │  runs | branches | epoch_snapshots | steps | pages | action_journal     │  │
│  │  memory_nodes | embeddings | contracts | accounts | trigger_runs | inbox│  │
│  └─────────────────────────────────────────────────────────────────────────┘  │
└───────────────────┬─────────────────────────────────────────┬─────────────────┘
                    │                                         │
┌───────────────────▼────────────────────┐  ┌─────────────────▼─────────────────┐
│   FAIL-CLOSED SANDBOX & TAINT ENGINE   │  │    TIER-0 PROACTIVITY DAEMON      │
│  • Linux bwrap (--unshare-net/pid)     │  │  • 0-Token Shell/SQL Sentinels    │
│  • macOS Seatbelt (Landlock probe only) │  │  • 5-Field Cron + Webhooks (HMAC) │
│  • Ephemeral Docker Desktop (noVNC)    │  │  • fsnotify + 30s Coalescer       │
│  • Secretless HTTP/SSH/FS Brokers      │  │  • Embedded in `soma web` & CLI   │
│  • Data/Action Provenance Taint        │  │  • 4 Hard Circuit Brakes & TTL GC │
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

### B. Live LLM A/B Evaluation & Hard Gauntlet (`benchmarks/live_eval_test.go`, `benchmarks/live_hard_test.go`)

`TestLiveEvaluation_HypothesisAB` runs three tasks against a real tool-calling model, once through `soma` and once through a minimal append-only chat loop with LLM summarization that is written for this test. It is skipped unless `SOMA_LIVE_API_KEY` (or fallback `OPENCODE_API_KEY`) is set. It targets the OpenCode Go API (`https://opencode.ai/zen/go/v1`) by default (`deepseek-v4.1-flash`), and supports `SOMA_LIVE_MODELS` (comma-separated, e.g. `deepseek-v4.1-flash,glm-5.3-flash`), `SOMA_LIVE_MAX_TOKENS` (default `16384`), and `SOMA_LIVE_REASONING_EFFORT`.

`TestLiveHard_Gauntlet` (`benchmarks/live_hard_test.go`, with fixtures, anti-cheese traps, and reference solutions pinned offline in `benchmarks/live_hard_offline_test.go`) adds six harder live scenarios designed for `deepseek-v4.1-flash` and `glm-5.3-flash`:

1. **`Hard1_MultiServiceLogCorrelation`** *(fair no-oracle A/B)*: 6-log (~1.9 MB) cross-service 503 incident tracing with ~80 decoy `ERROR`/`WARN` lines, `@page` spilling, and partial YAML/JSON config edits preserving sibling pools/shards.
2. **`Hard2_MultiFileLedgerAndPinnedOracle`** *(oracle-gated `soma` vs. self-graded linear)*: 4-module ledger bug fix (`currency`, transitive `LockGraph` deadlock/rollback, atomic idempotent `LedgerEngine`, out-of-order `EventLog`) passing shallow `tests/test_public.py` (`EROFS`-pinned) yet gated by 14 held-out invariants.
3. **`Hard3_CausalDriftBranchSearchAndDPO`** *(oracle-gated `BranchSearcher` + `ExportSFT`/`ExportDPO` vs. self-graded linear)*: Online causal anomaly detection defeating global z-score and naive fast EWMA traps under slow drift, 5x noise-scale shifts, sustained 60-sample plateaus, and O(N) runtime bounds.
4. **`Hard4_CooperativeSwarmAndThreeWayMerge`** *(`soma` swarm stress test)*: 3 concurrent workers (`parser`, `validator`, `pipeline`) in isolated `git worktree`s communicating over the Layer-2 Blackboard, 3-way merging `app/registry.py`, sharing identical `PrefixHashL0L1`, and passing a root integration `CommandOracle`.
5. **`Hard5_PromptInjectionTaintAndBiTemporalMem`** *(`soma` security/memory stress test)*: Indirect prompt injection + SSRF IMDS lure inside `web_extract`, *MINJA* untrusted memory quarantine, `SUPERSEDES` chain + `MaxLayer1Memories` RRF selection, and run-wide broker `ErrApprovalRequired` taint enforcement.
6. **`Hard6_BgProcessAndEpochRollover`** *(`soma` runtime stress test)*: Sandboxed `bg_exec` -> `bg_poll` (`@page` spill) -> `bg_kill` lifecycle paired with mid-run `Epoch 1 -> Epoch 2` rollover and `ModeExact` replay.

```bash
# Run original 3-task A/B evaluation
SOMA_LIVE_API_KEY=... SOMA_LIVE_BASE_URL=https://opencode.ai/zen/go/v1 \
SOMA_LIVE_MODEL=deepseek-v4.1-flash SOMA_LIVE_REPEATS=10 SOMA_LIVE_OUT=live_results.jsonl \
go test -count=1 -v -run TestLiveEvaluation_HypothesisAB ./benchmarks/

# Run 6-scenario Hard Gauntlet against both OpenCode Go flash models
OPENCODE_API_KEY=... \
SOMA_LIVE_MODELS=deepseek-v4.1-flash,glm-5.3-flash \
SOMA_LIVE_REPEATS=3 SOMA_LIVE_OUT=live_hard_results.jsonl \
go test -count=1 -v -timeout 45m -run TestLiveHard_Gauntlet ./benchmarks/
```

**No headline numbers are published here yet.** Earlier versions of this README reported `5/5` vs. `2/5` and a `98.6%` cache hit. Those figures were withdrawn because the baseline was not a fair one: it discarded its own summary at every compaction, never showed the summarizer the latest tool result, and was given fewer turns than `soma`. All three are fixed (and pinned by `live_baseline_test.go`), but the evaluation has not been re-run since.

When reading results from these tests, keep in mind:

- The baseline is a simple loop written for this repository. It is **not** Hermes, OpenClaw, or any other real harness.
- `Exp2`, `Exp3`, `Hard2`, and `Hard3` give `soma` an external oracle that feeds failing assertions back to the model, and give the baseline no verifier. They measure the value of verifier feedback, not of the rest of the kernel. Only `Exp1` and `Hard1` compare `soma` against the linear loop without external verifier feedback; `Hard4`–`Hard6` are `soma`-only kernel stress tests.
- Small task samples require `SOMA_LIVE_REPEATS`; report pass counts per model with the raw `SOMA_LIVE_OUT` log.

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
- **Web Dashboard & Isolated Virtual Desktop (`internal/webui`, `internal/desktop`, `docker/desktop`)**:
  - **Chat-First Task Runner**: Live SSE step & tool streaming, compact collapsible thinking traces, inline **Review Diff & Apply to Project** for isolated worktrees/snapshots (plus optional `Direct` mode), and **Approvals Inbox** with automatic branch resume.
  - **Models, Autonomy & Access**: Unified multi-provider model pool (`Worker`, `Frontier`, `GUI`, `Fallback` roles persisted in `.soma/harness.json`), opt-in sandbox internet access (`AllowSandboxNetwork`), and `Standard` vs. `High Autonomy` execution modes while keeping OS sandboxing fail-closed.
  - **Virtual Desktop (`soma-desktop`)**: Auto-launches a sandboxed Debian XFCE + Chromium container (`--rm`) when `desktop_task` needs GUI/browser access, streams via same-origin noVNC with a **View-Only / Take Over** shield, auto-stops after 5 min idle (or on exit), and supports `desktop_exec` + `sudo apt-get` / `pip` / `npm` inside the container.
  - **Connectors, Bridges & Automations**: 49+ MCP presets + stdio/HTTP/OAuth importers (`soma mcp`), 11 channel bridges with DM pairing (`soma bridge`), embedded 5-field cron & HMAC webhook scheduler, and optional Cloudflare Tunnel remote access (`soma web install-service`).
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

# 7. Launch the Web Dashboard (http://127.0.0.1:7420) or the 4-tab TUI
./bin/soma web --dir /tmp/demo
./bin/soma ui --dir /tmp/demo
make shadow
```

---

## 5. License

Released under the [MIT License](LICENSE).
