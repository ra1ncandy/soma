# soma — Durable, Capability-Scoped Agent Kernel & Workspace

[![CI](https://github.com/ra1ncandy/soma/actions/workflows/ci.yml/badge.svg)](https://github.com/ra1ncandy/soma/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/ra1ncandy/soma)](https://goreportcard.com/report/github.com/ra1ncandy/soma)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

`soma` is an open-source, pure-Go (`CGO_ENABLED=0`) autonomous agent kernel, web workspace, and isolated virtual desktop built from first principles.

Instead of running agents as linear chat loops that inline giant tool logs, overwrite project files blindly, and self-grade their own work, `soma` executes every task on a **durable, replayable Merkle DAG** with **fail-closed OS sandboxing**, **isolated branch workspaces with diff/apply**, and **real-time web, CLI, TUI, and messaging surfaces**.

> - **Architecture Spec**: [RFC-0001 (`soma` v2.1)](docs/rfcs/0001-soma-v2-architecture.md)
> - **Conformance Report**: [Suite B6 Benchmark Report](docs/benchmarks/BENCHMARK_REPORT.md)
> - **Threat Model & Security**: [SECURITY.md](SECURITY.md)

---

## Quickstart

### 1. Build & Launch the Web Dashboard

```bash
make build
./bin/soma web --dir /path/to/your/project
```

Open **http://127.0.0.1:7420** in your browser:
- Add an AI provider/model in **Settings → Models & Providers** (OpenAI-compatible, OpenRouter, Ollama, or `--mock` offline simulation).
- Ask a task in **Tasks** — edits run inside an isolated Git worktree or snapshot by default so you can click **Review Diff → Apply to Project** before touching your main folder (or switch to **Direct Project Folder** in Settings).

### 2. Run from the Terminal

```bash
# One-shot task (auto-initializes .soma/ and supports --browser / --desktop)
./bin/soma ask --dir /tmp/demo "Summarize this repository and run its tests"

# Interactive terminal REPL or 4-tab Bubble Tea TUI
./bin/soma chat --dir /tmp/demo
./bin/soma ui --dir /tmp/demo

# Durable DAG run with live OpenAI-compatible model
SOMA_API_BASE="https://api.openai.com/v1" SOMA_API_KEY="$OPENAI_API_KEY" SOMA_MODEL="gpt-4.1" \
./bin/soma run --dir /tmp/demo --goal "Inspect logs and fix timeout configuration"
```

---

## What `soma` Includes

### Web Dashboard (`soma web`)
- **Chat-First Task Runner**: Live step and tool updates over SSE (model text appears per step, not token by token yet), compact collapsible thinking/tool traces (`Thought for Xs · N steps · M tools`), branch switching, task rename/delete, and `Enter` to send.
- **Safe Workspace Isolation + Diff/Apply**: Runs tasks in isolated `git worktree`s or copy snapshots with an inline **Review Diff & Apply to Project** bar (unified diffs + 3-way merge), or optional `direct` in-place mode.
- **Approvals Inbox**: Rich context (task title, tool name, arguments, proposed memory content) with **Approve & Resume** that automatically unblocks and resumes paused branches.
- **Unified Model Pool & Role Routing**: Configure multiple providers/models in `.soma/harness.json` tagged by role (`default`, `worker`, `frontier`, `gui`, `fallback`) with automatic persistence across restarts and per-message account selection. The CLI (`soma ask/chat/run`) uses the same default account when `SOMA_API_BASE` is not set. All providers are reached through the OpenAI-compatible chat-completions API today.
- **Remote Access**: Built-in Cloudflare Tunnel integration with token auth, `HttpOnly` session cookies, and `soma web install-service` systemd user service support.

### Isolated Virtual Desktop (`soma desktop`)
- **Auto-Launching GUI Sandbox**: Spins up an isolated Debian XFCE + Chromium + noVNC Docker container (`docker/desktop`) automatically whenever a task calls `desktop_task(goal, start_url?)` (or manually from the **Virtual Desktop** tab).
- **Cursor, Keyboard & Screen Perception**: Powered by `desktop_mcp.py` (`xdotool` mouse/keyboard/scroll, AT-SPI2 `@e1` accessibility refs, Set-of-Mark screenshots, and `desktop_exec`).
- **View-Only / Take Over Protection**: The embedded noVNC viewer defaults to **View-Only (Agent Control)** so accidental clicks don't interrupt the agent, with a **Take Over / Hand Back Control** button when you want to step in.
- **Container-Scoped Package Managers & Lifecycle**: Includes passwordless `sudo`, `apt-get`, `pip`, `npm`, `git`, `wget`, and `unzip` inside the ephemeral `--rm` container (never touching your host OS), plus automatic 5-minute idle stop and clean shutdown on exit.

### Connectors, Channel Bridges & Automations
- **MCP Connectors (`soma mcp`)**: 47 built-in presets (plus 2 Docker desktop presets in the dashboard), Claude Code / Cursor / Codex / Hermes config importers, stdio, SSE & streamable HTTP transports, OAuth 2.1 (PKCE), tool allow/deny filters, and partial env-var merging without wiping saved secrets. Tool `destructiveHint` annotations are honoured: such tools always need approval.
- **Messaging Channel Bridges (`soma bridge`)**: text bridges with DM pairing/allowlists, supervised inside `soma web` or via CLI. Two-way today: Telegram, Slack (Events API), Matrix, Mattermost, Google Chat. Discord handles slash commands only. Teams and Email are send-only. WhatsApp, Signal and SMS need an external sidecar that speaks soma's signed webhook protocol. Each message starts a new run (no per-chat conversation memory yet).
- **Tier-0 Automations (`soma daemon`)**: Embedded zero-token sentinel, `fsnotify`, HMAC-SHA256 webhooks, 5-field cron (`0 9 * * 1-5`) + duration schedules (`15m`, `7d`), optional per-trigger debounce, and safety brakes (quiet hours, max pending approvals, daily budget). The agent can schedule its own cron jobs (`cronjob_manage`); they are notify-only by default, cannot touch operator-defined triggers, and need approval in a tainted run.

### Fail-Closed Security & Autonomy Controls
- **OS Sandboxing (`internal/sandbox`)**: Linux `bubblewrap` (`--unshare-pid/ipc/uts/cgroup`, `--cap-drop ALL`, `--clearenv`, read-only `/usr`; oracle files are mounted read-only while the oracle runs) and macOS Seatbelt. Agent shell tools always fail closed — no backend, no execution. Docker and SSH backends exist in code but are not wired into runs yet. Daemon sentinel commands from `triggers.yaml` (written by the operator) run on the host.
- **Granular Access Controls**: Opt-in **"Allow outbound internet access inside sandboxed commands"** (`AllowNetwork`; output of networked commands is then treated as untrusted) and **Standard vs. High Autonomy** modes (auto-approving non-irreversible external/MCP actions while still gating irreversible writes such as MCP tools marked destructive).
- **Provenance Taint (`internal/taint`)**: Zero raw API keys inside sandboxes; *CaMeL*-style run-wide provenance tainting for `untrusted_external` web/MCP reads, egress limits on web fetches and searches once a run is tainted (long or encoded arguments need approval), and *MINJA* anti-poisoning quarantine for learned memories and peer findings. Secretless HTTP/SSH/FS brokers are implemented and tested but not yet reachable from agent runs.
- **Skills Under Review**: Agent-proposed `SKILL.md` changes are linted and staged; they take effect only after operator approval, with the previous version kept in `.history/`. Project instructions (`AGENTS.md`, `CLAUDE.md`, `.cursorrules`, `.soma/persona.md`) and the skill index are frozen into the prompt each epoch; files matching injection patterns are blocked.

### Durable Kernel Core
- **4-Layer Monotonic Prompt Compiler (`L0`–`L3`)**: Keeps prompt prefixes byte-stable within an epoch for provider-side KV-cache reuse.
- **Content-Addressed Virtual Memory (`@page_XXXX`)**: Zstd blob spilling with `page_grep`, `page_slice`, and hybrid Code-BM25 + `int8` vector RRF (`page_search`). Vectors are lexical feature hashes by default; configure an embeddings endpoint (`SOMA_EMBED_URL`) for semantic retrieval with 256-dim Matryoshka truncation.
- **Provenance Memory**: memory nodes with validity intervals, `SUPERSEDES`/`MERGES` edges, and `global` / `workspace` / `repo` scopes.
- **Durability & Replay**: 2-phase action journal; a crash during any external write suspends the branch for reconciliation instead of retrying; rate-limited branches resume automatically in `soma web`; running out of turns records `budget_exhausted` (resumable); exact replay verifies every model step's frame hash, including multi-turn conversations.
- **External Oracles, Speculative Search & Swarms**: `CommandOracle` (hash-pinned files), `ArtifactOracle`, `soma search` with Graveyard backtracking, `soma swarm` with Layer-2 Blackboard + 3-way `git merge-file`, and `soma export-trajectories` for oracle-verified SFT / contrastive DPO datasets.

---

## Architecture Overview

```text
┌───────────────────────────────────────────────────────────────────────────────┐
│                            OPERATOR & BRIDGE SURFACE                          │
│  Web Dashboard (`soma web` · SSE, Diff/Apply, Virtual Desktop noVNC, Tunnel)  │
│  CLI (`soma ask/chat/run/search/swarm/mcp/bridge/...`) │ 4-Tab Bubble Tea TUI │
│  Channel Bridges (Telegram, Slack, Matrix, Discord, Webhook sidecars, IPC)    │
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
│  │  L3: Ephemeral Live Tail        │  │  • Role & Quota Account Router     │  │
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
│  • Linux bwrap (--unshare-net optional)│  │  • 0-Token Shell/SQL Sentinels    │
│  • macOS Seatbelt (Landlock probe only) │  │  • 5-Field Cron + Webhooks (HMAC) │
│  • Ephemeral Docker Desktop (noVNC)    │  │  • fsnotify + Debounce Coalescer  │
│  • Secretless HTTP/SSH/FS Brokers      │  │  • Embedded in `soma web` & CLI   │
│  • Data/Action Provenance Taint        │  │  • 4 Hard Circuit Brakes & TTL GC │
└────────────────────────────────────────┘  └───────────────────────────────────┘
```

---

## CLI Reference

| Category | Commands |
|---|---|
| **Everyday Use** | `soma web`, `soma ask`, `soma chat`, `soma ui`, `soma init`, `soma status` |
| **Workflows & Swarms** | `soma run`, `soma search`, `soma swarm`, `soma fork`, `soma resume` |
| **Desktop, MCP & Bridges** | `soma desktop`, `soma mcp`, `soma bridge`, `soma daemon`, `soma import-hermes` |
| **Memory, Skills & Safety** | `soma memory`, `soma consolidate`, `soma skill`, `soma inbox`, `soma reconcile`, `soma oracle`, `soma sandbox` |
| **Replay & Training Data** | `soma replay`, `soma xray`, `soma shadow`, `soma export-trajectories`, `soma account` |

Run `soma <command> --help` for flags and examples, or see [Quickstart](#quickstart) for common workflows:

```bash
# Speculative branch search with Graveyard backtracking
./bin/soma search --dir /tmp/demo --mock --goal "Explore competing hypotheses safely"

# Concurrent multi-agent swarm (Blackboard + 3-way worktree merge + oracle)
./bin/soma swarm --dir /tmp/demo --mock --mode cooperative \
  --goal "Implement RPC timeout handler and integration tests" \
  --subgoal "backend:Implement compute_timeout() in backend.py and publish finding" \
  --subgoal "tests:Write test_app.py verifying backend.py contract from Blackboard"

# Export oracle-verified SFT and contrastive DPO trajectories
./bin/soma export-trajectories --dir /tmp/demo --format sft --verified-only --out sft.jsonl
./bin/soma export-trajectories --dir /tmp/demo --format dpo --verified-only --out dpo.jsonl
```

---

## Verification & Evaluation

`soma` separates **deterministic offline kernel conformance** from **live LLM evaluations**.

### 1. Offline Conformance Suites (`benchmarks/` — Suites B1–B6)

Run with scripted providers (no LLM) in CI (`go test -count=1 -race ./...` and `make shadow`):

| Suite | What It Verifies | Key Acceptance Gate |
|---|---|---|
| **B1** (`b1_log_storm_test.go`) | 50 turns, five 200 KB logs, mid-epoch skill & MCP loads | `0` compactions, `100%` SHA-256 frame reconstruction, byte-stable prefix (`>= 94%` simulated cache hit), `>= 40%` prompt token savings, delta storage `< 15%` |
| **B2** (`b2_chaos_durability_test.go`) | 20 concurrent chaos tasks + 429 storms + crashes between dispatch and commit | `20/20` completion, `0` lost steps, `0` `SQLITE_BUSY`, `0` duplicate `irreversible_write` executions |
| **B2/v0.3** (`b2_v3_surface_test.go`) | Multi-sub 429 failover, 100 unchanged sentinel ticks + 20 burst webhooks, Discord/Telegram IPC & 4-tab TUI | `0` tokens on unchanged sentinels, `20 -> 1` burst coalescing, clean Epoch 2 handoff, zero `SQLITE_BUSY` |
| **B3** (`b3_memory_antipoison_test.go`) | 30 sequential rule updates (`SUPERSEDES`/`MERGES`) + *MINJA* injection + skill immutability | `100%` current & point-in-time recall, `0%` untrusted rule activation, `0` silent `SKILL.md` mutations |
| **B4** (`b4_sandbox_confused_deputy_test.go`) | Real `bwrap` isolation + poisoned `SKILL.md` + Confused Deputy + SSH injection/leases | `0` leaked bytes/network, `100%` Confused Deputy `POST` blocked until deterministic `DeclassifyStructured` |
| **B5** (`b5_oracles_reward_hacking_test.go`) | *ImpossibleBench* pinned test tampering + stale checksum (`p181`/`p183`) & Track D collapse | `100%` premature `Done` rejected until fresh verified artifact passes |
| **B6** (`b6_branching_shadow_gauntlet_test.go`) | Speculative search + 4-scenario [conformance gauntlet](docs/benchmarks/BENCHMARK_REPORT.md) (enabled vs. disabled ablation) | `4/4` verified enabled, `0/4` disabled, `100%` `ModeExact` SHA-256 replay |

### 2. Live LLM A/B Evaluation & Hard Gauntlet (`benchmarks/live_eval_test.go`, `benchmarks/live_hard_test.go`)

Skipped unless `SOMA_LIVE_API_KEY` (or `OPENCODE_API_KEY`) is set:

```bash
# 3-task A/B evaluation against a minimal linear chat-loop baseline
SOMA_LIVE_API_KEY=... SOMA_LIVE_BASE_URL=https://opencode.ai/zen/go/v1 \
SOMA_LIVE_MODEL=deepseek-v4.1-flash SOMA_LIVE_REPEATS=10 SOMA_LIVE_OUT=live_results.jsonl \
go test -count=1 -v -run TestLiveEvaluation_HypothesisAB ./benchmarks/

# 6-scenario Hard Gauntlet
OPENCODE_API_KEY=... SOMA_LIVE_MODELS=deepseek-v4.1-flash,glm-5.3-flash \
SOMA_LIVE_REPEATS=3 SOMA_LIVE_OUT=live_hard_results.jsonl \
go test -count=1 -v -timeout 45m -run TestLiveHard_Gauntlet ./benchmarks/
```

> **Note**: No headline live A/B numbers are published here yet while the post-fix baseline (`live_baseline_test.go`) awaits a full re-run. Only `Exp1` and `Hard1` compare `soma` against the linear loop without external verifier feedback; `Exp2`, `Exp3`, `Hard2`, and `Hard3` measure verifier feedback; `Hard4`–`Hard6` are `soma`-only stress tests.

---

## License

Released under the [MIT License](LICENSE).
