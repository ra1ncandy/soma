# Kernel Invariants

Every change to `soma` must preserve these non-negotiable systems invariants:

1. **Zero-CGO Pure Go (`CGO_ENABLED=0`)**
   - `cmd/soma`, `cmd/soma-bridge-discord`, and all `internal/...` packages must compile statically with `CGO_ENABLED=0` across `linux/{amd64,arm64}` and `darwin/{amd64,arm64}`.
   - Never introduce CGO dependencies (e.g., C SQLite drivers, ONNX C bindings, or C-backed compression libraries). Use `modernc.org/sqlite` and pure-Go packages only.

2. **Strict 4-Layer Monotonic Prompt Compilation (`internal/compiler`)**
   - `Layer 0` (Frozen Kernel & Syscalls) and `Layer 1` (Frozen Epoch Registers, Epoch-Pinned Top-K Memory & Graveyard) are **strictly immutable** for the entire duration of an epoch. `PrefixHashL0L1` must never mutate mid-epoch.
   - Any mid-epoch event — skill activation, deferred MCP schema via `tool_search`, `memory_search` / `page_search` results, or cross-branch `Blackboard` peer notices — **must** append strictly to `Layer 2` (`[]compiler.LogEntry`) and be recorded in that step's incremental `FrameDelta`.
   - `compiler.ReconstructFrame` must preserve bit-for-bit SHA-256 equality (`ModeExact` replay).

3. **2-Phase Durable Action Journaling (`internal/dag`, `internal/store`)**
   - Every tool execution must transition through `action_journal`: `prepared` → `dispatched` → `committed` or `ambiguous`.
   - An uncertain `irreversible_write` interrupted by a crash must transition to `StatusSuspendedReconcile` and **never** auto-retry on restart.

4. **Split-Pool SQLite WAL Concurrency (`internal/store`)**
   - Preserve split read/write pooling and WAL pragmas so concurrent DAG workers, TUI/WebUI readers, daemon sentinels, and bridge IPC produce zero `SQLITE_BUSY` errors under `-race`.

5. **Verification Discipline**
   - Before considering a task complete, verify with `go vet ./...`, `go test -count=1 -race ./...`, and `make build`. Regenerate `docs/benchmarks/BENCHMARK_REPORT.md` via `make shadow` whenever shadow/conformance behavior changes.
