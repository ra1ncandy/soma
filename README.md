# soma — A Durable, Replayable, Capability-Scoped Agent Kernel

`soma` is an open-source local agent execution kernel written in pure Go (`CGO_ENABLED=0`), built as a systems-first alternative to linear chat-loop harnesses such as **Hermes** and **OpenClaw**.

Instead of mutating an append-only conversation transcript that directly executes side effects, `soma` treats every agent run as a **durable, content-addressed Merkle execution DAG** paired with a **cache-safe paged Context VM**, **fail-closed OS isolation**, **bi-temporal provenance memory**, and **external verification oracles**.

> Architecture specification: [RFC-0001 (`soma` v2.1 Architecture)](docs/rfcs/0001-soma-v2-architecture.md)

---

## Core Thesis (Five Orthogonal Primitives)

| Primitive | What It Replaces | Current Status (`v0.2`) |
|---|---|---|
| **1. Event-Sourced Execution DAG & Replay** | Mutable linear chat transcripts that lose work on crashes or HTTP 429s | Implemented (`internal/dag`, `internal/replay`, `internal/workspace`) |
| **2. Cache-Safe Hybrid Paged Context VM (`@page`)** | Raw tool dumps & lossy LLM compaction thrashing | Implemented (`internal/vmem`, `internal/compiler`) |
| **3. Typed Capability Brokers & Fail-Closed Sandbox** | Prompt-level willpower & unprotected host environments | Implemented (`internal/sandbox`, `internal/policy`, `internal/broker`, `internal/taint`) |
| **4. Bi-Temporal Memory Graph & Open Standards** | Flat `MEMORY.md` files & self-mutating prompt bloat | Implemented (`internal/memory`, `internal/skills`, `internal/mcp` for `AAIF SKILL.md` + `MCP 2026-07-28`) |
| **5. Verification Contracts & Quota Scheduler** | Self-graded completion reports & blind 429 retries | Verification oracles & quota/cooldown scheduler implemented (`internal/oracle`, `internal/dag`); TUI & channel bridges queued for `v0.3` |

---

## Implemented Kernel Subsystems (`v0.2`)

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
  - `SUPERSEDES` / `MERGES` edges, `QueryAsOf` point-in-time queries, anti-*MINJA* untrusted quarantine (`status = 'proposed'`).
  - AAIF `SKILL.md` 3-stage loader + pre-activation security linter + immutable skill patch proposals.
  - Stateless `MCP 2026-07-28` client (`MCP-Protocol-Version`, `Mcp-Method`, `Mcp-Name`, `io.modelcontextprotocol/skills`), alphabetical epoch catalog pinning, and `untrusted_external` tagging (*MCPTox* defense).
- **External Verification Oracles (`internal/oracle`)**:
  - `CommandOracle` (pinned SHA-256 invariant guard + sandboxed execution + JSON metrics), `ArtifactOracle` (stale checksum rejection + non-null JSON validation), and optional stateless `AdvisoryAudit`.

---

## Quickstart

```bash
# Build single static binary (zero CGO)
make build

# Initialize .soma/ in a workspace (proves bwrap / Linux Landlock ABI availability)
./bin/soma init --dir /tmp/demo

# Execute a command inside the fail-closed OS sandbox
./bin/soma sandbox exec --dir /tmp/demo -- /bin/sh -c 'pwd && env'

# Run a durable mock task, inspect 3-Boundary X-Ray, fork, and replay in all 3 modes
./bin/soma run --dir /tmp/demo --mock --goal "Diagnose webhook race condition"
./bin/soma status --dir /tmp/demo
./bin/soma xray <step_id> --dir /tmp/demo
./bin/soma fork <step_id> --dir /tmp/demo --label "alt-approach"
./bin/soma replay <branch_id> --dir /tmp/demo --mode exact

# Manage bi-temporal memory, lint skills, and lock verification oracles
./bin/soma memory add --dir /tmp/demo --scope global --kind rule --content "Always run -race"
./bin/soma memory list --dir /tmp/demo
./bin/soma skill lint /path/to/skill-dir
./bin/soma oracle lock <run_id> pinned_test.go --dir /tmp/demo --cmd /bin/sh
```

---

## Reproducing Benchmarks (`benchmarks/` — Suites B1–B5)

```bash
# Run full test suite + B1-B5 benchmarks under Go race detector
make test

# Run B1-B5 benchmark suites directly
make bench
```

| Suite | What It Verifies | Key Acceptance Gate |
|---|---|---|
| **B1** (`b1_log_storm_test.go`) | 50 turns, five 200 KB logs, mid-epoch skill & MCP loads | `0` compactions, `>= 94%` cache hit, `< 24k` peak tokens, `>= 40%` token savings, `< 15%` delta/naive blob storage, `100%` SHA-256 reconstruction |
| **B2** (`b2_chaos_durability_test.go`) | 20 concurrent chaos tasks + 429 storms + crashes between dispatch and commit | `20/20` completion, `0` lost steps, `0` `SQLITE_BUSY`, `0` duplicate `irreversible_write` executions |
| **B3** (`b3_memory_antipoison_test.go`) | 30 sequential rule updates (`SUPERSEDES`/`MERGES`) + *MINJA* injection + skill immutability | `100%` current & point-in-time recall, `0%` untrusted rule activation, `0` silent `SKILL.md` mutations |
| **B4** (`b4_sandbox_confused_deputy_test.go`) | Real `bwrap` isolation + poisoned `SKILL.md` + `MCP 2026-07-28` Confused Deputy + SSH injection/leases | `0` leaked bytes/network, `100%` Confused Deputy `POST` blocked until deterministic `DeclassifyStructured`, SSH injection rejected |
| **B5** (`b5_oracles_reward_hacking_test.go`) | *ImpossibleBench* pinned test tampering + Hermes stale checksum (`p181`/`p183`) & Track D collapse | `100%` premature `Done` rejected until fresh verified artifact passes |
