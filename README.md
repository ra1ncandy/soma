# soma — A Durable, Replayable, Capability-Scoped Agent Kernel

`soma` is an open-source local agent execution kernel written in pure Go (`CGO_ENABLED=0`), built as a systems-first alternative to linear chat-loop harnesses such as **Hermes** and **OpenClaw**.

Instead of mutating an append-only conversation transcript that directly executes side effects, `soma` treats every agent run as a **durable, content-addressed Merkle execution DAG** paired with a **cache-safe paged Context VM**, **fail-closed OS isolation**, **bi-temporal provenance memory**, **external verification oracles**, **zero-token Tier-0 event sentinels**, an **interactive Bubble Tea v2 console**, and **out-of-process Channel Bridges**.

> Architecture specification: [RFC-0001 (`soma` v2.1 Architecture)](docs/rfcs/0001-soma-v2-architecture.md)

---

## Core Thesis (Five Orthogonal Primitives)

| Primitive | What It Replaces | Current Status (`v0.3`) |
|---|---|---|
| **1. Event-Sourced Execution DAG & Replay** | Mutable linear chat transcripts that lose work on crashes or HTTP 429s | Implemented (`internal/dag`, `internal/replay`, `internal/workspace`) |
| **2. Cache-Safe Hybrid Paged Context VM (`@page`)** | Raw tool dumps & lossy LLM compaction thrashing | Implemented (`internal/vmem`, `internal/compiler`) |
| **3. Typed Capability Brokers & Fail-Closed Sandbox** | Prompt-level willpower & unprotected host environments | Implemented (`internal/sandbox`, `internal/policy`, `internal/broker`, `internal/taint`) |
| **4. Bi-Temporal Memory Graph & Open Standards** | Flat `MEMORY.md` files & self-mutating prompt bloat | Implemented (`internal/memory`, `internal/skills`, `internal/mcp` for `AAIF SKILL.md` + `MCP 2026-07-28`) |
| **5. Verification Contracts, Daemon, Bridges & TUI** | Self-graded prose reports, blind polling crons & sticky 429 crashes | Implemented (`internal/oracle`, `internal/daemon`, `internal/inbox`, `internal/bridge`, `internal/tui`, `cmd/soma-bridge-discord`) |

---

## Implemented Kernel Subsystems (`v0.1` – `v0.3`)

- **Split-Pool Pure-Go SQLite WAL (`modernc.org/sqlite`) + zstd Blob Store (`internal/store`)**:
  - Dedicated single-connection writer (`BEGIN IMMEDIATE`) + concurrent reader pool (`query_only`).
  - `epoch_snapshots` + incremental `FrameDelta` blobs for linear `O(T)` storage with bit-for-bit `ReconstructFrame` SHA-256 verification.
  - 2-phase `action_journal` + `SUSPENDED_RECONCILE` blocking unsafe automatic retries of crashed `irreversible_write` actions.
- **Virtual Memory `@page` & Prefix-Stable Compiler (`internal/vmem`, `internal/compiler`)**:
  - Anti-syscall-tax inline thresholds (`3 KiB` worker / `16 KiB` frontier) and deterministic previews (`log`, `code`, `json`, `tabular`, `diff`, `http`, `text`).
  - Strictly immutable Epoch `Layer0`/`Layer1`; dynamic mid-epoch loads append strictly to `Layer2` tail.
- **Fail-Closed OS Sandbox & Typed Capability Brokers (`internal/sandbox`, `internal/policy`, `internal/broker`, `internal/taint`)**:
  - Linux `bubblewrap` (`--unshare-net`, scrubbed env, zero host `~/.ssh`/`~/.aws`, kernel `EROFS` binds on pinned invariant files) + Landlock ABI probe + macOS Seatbelt SBPL generator.
  - Strict `deny > ask > allow` policy + `HTTPBroker`, `SSHBroker` (prefix-allowlisted, remote leases), and `FSBroker` gated by data/action-level taint provenance and deterministic `DeclassifyStructured` parsers.
- **Bi-Temporal Memory, Immutable AAIF Skills & `MCP 2026-07-28` (`internal/memory`, `internal/skills`, `internal/mcp`)**:
  - `SUPERSEDES` / `MERGES` edges, `QueryAsOf` point-in-time queries, anti-*MINJA* untrusted quarantine (`status = 'proposed'`), and offline workspace consolidation (`soma consolidate`).
  - AAIF `SKILL.md` 3-stage loader + pre-activation security linter + immutable skill patch proposals.
  - Stateless `MCP 2026-07-28` client (`MCP-Protocol-Version`, `Mcp-Method`, `Mcp-Name`, `io.modelcontextprotocol/skills`), alphabetical epoch catalog pinning, and `untrusted_external` tagging (*MCPTox* defense).
- **External Verification Oracles & Multi-Subscription Quota Scheduler (`internal/oracle`, `internal/dag`)**:
  - `CommandOracle`, `ArtifactOracle`, and stateless `AdvisoryAudit` gating branch completion.
  - Subscription/metered concurrency leases + cooldown persistence + clean Epoch rollover Handoff Packet failover (`AllowFailover`) that returns to primary preference once cooldowns expire.
- **Tier-0 Event Daemon, Coalescer & Inhibitory Brakes (`internal/daemon`)**:
  - Zero-token shell/SQL/hash sentinels (`wake_on: output_hash_changed`), `watch` `stop_command` termination, `fsnotify` triggers, HMAC-authenticated webhook HTTP listener, debounce coalescing, SQLite idempotency deduplication, `quiet_hours`/`max_pending_inbox`/budget/cooldown brakes, and TTL GC.
- **Interactive Bubble Tea v2 4-Tab Console & Out-of-Process Bridges (`internal/tui`, `internal/inbox`, `internal/bridge`, `cmd/soma-bridge-discord`)**:
  - Four-tab terminal UI (`1:DAG & HUD`, `2:Memory & Skills`, `3:X-Ray & Replay`, `4:Daemon/Inbox`) with direct `$EDITOR` edits, X-Ray step scrubbing, and one-key Inbox approvals.
  - Authenticated `0o600` Unix Domain Socket RPC (`kernel.sock` + `bridge.token`), plus fail-closed `DiscordAdapter` (Ed25519 signature verification + user allowlist, standalone `soma-bridge-discord` binary) and `TelegramAdapter`.

---

## Quickstart

```bash
# Build static pure-Go binaries (bin/soma and bin/soma-bridge-discord, zero CGO)
make build

# Initialize .soma/ in a workspace (proves bwrap / Linux Landlock ABI availability)
./bin/soma init --dir /tmp/demo

# Launch the 4-tab Bubble Tea v2 interactive TUI
./bin/soma ui --dir /tmp/demo

# Manage accounts & multi-subscription quotas
./bin/soma account add --dir /tmp/demo --id codex-plus --provider openai-codex --model gpt-6.2-sol --mode subscription --max-concurrent 1
./bin/soma account list --dir /tmp/demo
./bin/soma account reset-cooldown codex-plus --dir /tmp/demo

# Run durable DAG tasks, X-Ray, fork, replay, and sandbox commands
./bin/soma run --dir /tmp/demo --mock --goal "Diagnose webhook race condition"
./bin/soma status --dir /tmp/demo
./bin/soma xray <step_id> --dir /tmp/demo
./bin/soma fork <step_id> --dir /tmp/demo --label "alt-approach"
./bin/soma replay <branch_id> --dir /tmp/demo --mode exact
./bin/soma sandbox exec --dir /tmp/demo -- /bin/sh -c 'pwd && env'

# Memory, skills, oracles, consolidation, and operator inbox
./bin/soma memory add --dir /tmp/demo --scope global --kind rule --content "Always run -race"
./bin/soma memory graph <node_id> --dir /tmp/demo
./bin/soma memory edit <node_id> --dir /tmp/demo --content "Always run -count=1 -race"
./bin/soma consolidate --dir /tmp/demo
./bin/soma inbox list --dir /tmp/demo
./bin/soma inbox approve <item_id> --dir /tmp/demo
./bin/soma skill lint /path/to/skill-dir
./bin/soma oracle lock <run_id> pinned_test.go --dir /tmp/demo --cmd /bin/sh

# Tier-0 event daemon & out-of-process Channel Bridges
./bin/soma daemon dry-run <trigger_id> --dir /tmp/demo
./bin/soma daemon status --dir /tmp/demo
./bin/soma daemon start --dir /tmp/demo --bridge --mock
./bin/soma bridge list --dir /tmp/demo
./bin/soma-bridge-discord --soma-dir /tmp/demo/.soma --help
```

---

## Reproducing Benchmarks (`benchmarks/`)

```bash
# Run full test suite + benchmarks under Go race detector
make test

# Run benchmark suites directly
make bench
```

| Suite | What It Verifies | Key Acceptance Gate |
|---|---|---|
| **B1** (`b1_log_storm_test.go`) | 50 turns, five 200 KB logs, mid-epoch skill & MCP loads | `0` compactions, `>= 94%` cache hit, `< 24k` peak tokens, `>= 40%` token savings, `< 15%` delta/naive blob storage, `100%` SHA-256 reconstruction |
| **B2** (`b2_chaos_durability_test.go`) | 20 concurrent chaos tasks + 429 storms + crashes between dispatch and commit | `20/20` completion, `0` lost steps, `0` `SQLITE_BUSY`, `0` duplicate `irreversible_write` executions |
| **B2/v0.3** (`b2_v3_surface_test.go`) | Multi-sub 429 clean epoch failover, 100 unchanged sentinel ticks + 20 burst webhooks, Discord/Telegram Unix IPC bridge & concurrent 4-tab TUI | `0` tokens on unchanged sentinels, `20 -> 1` burst coalescing, clean Epoch 2 Handoff Packet + primary recovery, concurrent TUI/IPC zero `SQLITE_BUSY` |
| **B3** (`b3_memory_antipoison_test.go`) | 30 sequential rule updates (`SUPERSEDES`/`MERGES`) + *MINJA* injection + skill immutability | `100%` current & point-in-time recall, `0%` untrusted rule activation, `0` silent `SKILL.md` mutations |
| **B4** (`b4_sandbox_confused_deputy_test.go`) | Real `bwrap` isolation + poisoned `SKILL.md` + `MCP 2026-07-28` Confused Deputy + SSH injection/leases | `0` leaked bytes/network, `100%` Confused Deputy `POST` blocked until deterministic `DeclassifyStructured`, SSH injection rejected |
| **B5** (`b5_oracles_reward_hacking_test.go`) | *ImpossibleBench* pinned test tampering + Hermes stale checksum (`p181`/`p183`) & Track D collapse | `100%` premature `Done` rejected until fresh verified artifact passes |
