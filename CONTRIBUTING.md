# Contributing to `soma`

Thank you for contributing to **`soma`**! To preserve the kernel's deterministic replay, KV-cache stability, and single-binary deployment invariants, please follow these engineering rules:

## 1. Kernel Invariants

1. **Zero-CGO Pure Go (`CGO_ENABLED=0`)**:
   - All core packages (`cmd/soma`, `cmd/soma-bridge-discord`, `internal/...`) must compile statically with `CGO_ENABLED=0`. Never introduce dependencies requiring C/C++ shared libraries (such as CGO SQLite drivers or ONNX C bindings).
2. **Strict 4-Layer Monotonic Prompt Compilation (`internal/compiler`)**:
   - `Layer 0` (Frozen Kernel & Syscalls) and `Layer 1` (Frozen Epoch Registers, Active Memory & Graveyard) are **strictly immutable** for the duration of an epoch (`PrefixHashL0L1` must never change mid-epoch).
   - Any mid-epoch event (skill activation, deferred MCP schema via `tool_search`, `memory_search` / `page_search` results, or cross-branch `Blackboard` peer notices) **must** append strictly to `Layer 2` (`[]compiler.LogEntry`) and be recorded in that step's incremental `FrameDelta` so `compiler.ReconstructFrame` verifies SHA-256 bit-for-bit equality.
3. **2-Phase Durable Action Journaling (`internal/dag`)**:
   - Every tool execution must transition through `action_journal` (`prepared` $\to$ `dispatched` $\to$ `committed` / `ambiguous`). Uncertain `irreversible_write` actions interrupted by a crash must never auto-retry on restart (`StatusSuspendedReconcile`).

## 2. Verification Before Submitting a PR

Run the full verification suite locally:

```bash
# 1. Static analysis
go vet ./...

# 2. Full 21-package unit test + Suites B1-B6 benchmark gauntlet under race detector
go test -count=1 -race ./...

# 3. Build stripped static binaries
make build

# 4. Run reproducible Shadow Gauntlet scorecard
make shadow
```
