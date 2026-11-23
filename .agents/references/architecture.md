# `soma` Repository Architecture Map

`soma` is a pure-Go (`CGO_ENABLED=0`, Go 1.26) durable, replayable, capability-scoped agent execution kernel.

## Binaries (`cmd/`)

| Path | Binary | Purpose |
|---|---|---|
| `cmd/soma/main.go` | `bin/soma` | Main CLI (`init`, `run`, `resume`, `fork`, `replay`, `reconcile`, `xray`, `status`, `memory`, `skill`, `oracle`, `sandbox`, `ui`, `web`, `daemon`, `bridge`, `account`, `inbox`, `consolidate`, `shadow`, `search`, `swarm`, `export-trajectories`) |
| `cmd/soma-bridge-discord/main.go` | `bin/soma-bridge-discord` | Standalone Discord bridge daemon communicating with `soma` over Unix-socket JSON-RPC |

## Core Kernel Packages (`internal/`)

| Package | Responsibility | Key Files |
|---|---|---|
| `internal/compiler` | 4-Layer Monotonic Prompt Compiler (`L0`–`L3`), model profiles, incremental `FrameDelta`, and SHA-256 `ReconstructFrame` | `compiler.go`, `profile.go`, `reconstruct.go` |
| `internal/dag` | Merkle DAG executor, 16 canonical tools, 2-phase `action_journal`, `bg_exec` lifecycle, `BranchSearcher`, `SwarmCoordinator`, `Layer 2` Blackboard, LLM `Provider` & multi-turn KV cache, SSRF-guarded web/multimodal tools, multi-account `QuotaScheduler` | `executor.go`, `provider.go`, `patch.go`, `bgproc.go`, `blackboard.go`, `search.go`, `swarm.go`, `scheduler.go`, `web_multimodal.go` |
| `internal/store` | Pure-Go split-pool SQLite WAL (`modernc.org/sqlite`) + zstd content-addressed blob store (`.soma/state.db` + `blobs/`) | `db.go`, `migrations.sql`, `models.go`, `repo.go`, `blobs.go` |
| `internal/vmem` | Content-addressed virtual memory (`@page_XXXX`), zstd spilling, structural previewers, `page_grep`, `page_slice`, `page_search` | `vmem.go`, `preview.go` |
| `internal/embed` | Hybrid RRF retrieval (`LocalSimHashEmbedder` offline + OpenAI-compatible `/v1/embeddings` with 256-dim `int8` Matryoshka quantization) | `embed.go` |
| `internal/memory` | Bi-temporal provenance memory graph (`QueryAsOf`, `SUPERSEDES`/`MERGES`, *MINJA* untrusted quarantine, epoch-pinned Top-K retrieval) | `memory.go` |
| `internal/sandbox` | Fail-closed OS sandboxing (`bwrap` on Linux, Seatbelt on macOS, ephemeral Docker, remote SSH, env scrubbing; Landlock probe-only) | `sandbox.go`, `bwrap_linux.go`, `landlock_linux.go`, `seatbelt.go`, `remote_backends.go` |
| `internal/broker` | Secretless host-side capability brokers (`HTTPBroker`, `SSHBroker`, `FSBroker`) with run-wide taint parity and remote mutual-exclusion leases | `broker.go`, `http_broker.go`, `ssh_broker.go`, `fs_broker.go` |
| `internal/taint` | Data/action-level provenance taint (`trusted_user`, `trusted_repo`, `untrusted_external`), `DeclassifyStructured`, Confused Deputy defense | `taint.go` |
| `internal/policy` | Effect classification (`read_only`, `workspace_write`, `external_write`, `irreversible_write`) and allow/ask/deny policy decisions | `policy.go` |
| `internal/oracle` | External verification contracts (`CommandOracle` with `EROFS` pinning, `ArtifactOracle`, zero-history `AdvisoryReviewer`) | `oracle.go`, `command_oracle.go`, `artifact_oracle.go`, `advisory.go` |
| `internal/replay` | 3-mode replay (`ModeExact` SHA-256 verification) and oracle-verified SFT / contrastive DPO-GRPO trajectory export | `replay.go`, `exporter.go` |
| `internal/workspace` | Isolated per-branch Git worktrees/snapshots and 3-way `git merge-file` merging (`MergeBranchWorkspaces`) | `workspace.go`, `merge.go` |
| `internal/skills` | AAIF `SKILL.md` parser, progressive Stage-1 index loader, and pre-activation security linter | `skills.go`, `lint.go` |
| `internal/mcp` | Deferred MCP client and `tool_search` schema discovery | `client.go` |
| `internal/daemon` | Tier-0 proactivity daemon: 0-token sentinels, `fsnotify` + HMAC-SHA256 webhooks, 30s debounce coalescer, 4 circuit brakes | `daemon.go`, `coalescer.go`, `brakes.go`, `config.go` |
| `internal/inbox` | Operator approval & quarantine inbox (`soma inbox`) for tainted actions and quarantined memory proposals | `inbox.go` |
| `internal/shadow` | Deterministic conformance gauntlet runner & Markdown report generator (`docs/benchmarks/BENCHMARK_REPORT.md`) | `runner.go`, `report.go`, `models.go` |
| `internal/cli` | Cobra CLI wiring and platform sandbox probes | `cli.go`, `v2_commands.go`, `v3_commands.go`, `v4_commands.go`, `web_command.go` |
| `internal/tui` | 4-tab Bubble Tea v2 interactive operator TUI (`soma ui`) | `model.go`, `data.go`, `styles.go` |
| `internal/webui` | Embedded single-binary localhost web UI (`soma web`) | `server.go`, `static/*` |
| `internal/bridge` | Unix-socket JSON-RPC bridge protocol + Discord/Telegram adapters | `protocol.go`, `server.go`, `client.go`, `discord.go`, `telegram.go` |

## Verification & Benchmarks (`benchmarks/` & `docs/`)

- `benchmarks/b1_*` – `benchmarks/b6_*`: Offline deterministic conformance suites (scripted providers, no LLM) run in CI under `go test -count=1 -race ./...`.
- `benchmarks/live_baseline_test.go`: Offline unit pins for the live evaluation's linear baseline fairness invariants.
- `benchmarks/live_hard_offline_test.go`: Offline pins for Hard Gauntlet fixtures, anti-cheese traps, and reference solutions.
- `benchmarks/live_eval_test.go`, `benchmarks/live_hard_test.go`: Live LLM evaluations (skipped unless `SOMA_LIVE_API_KEY` or `OPENCODE_API_KEY` is set).
- `docs/rfcs/0001-soma-v2-architecture.md`: Authoritative architecture specification.
- `docs/benchmarks/BENCHMARK_REPORT.md`: Generated Suite B6 conformance report (`make shadow`).
