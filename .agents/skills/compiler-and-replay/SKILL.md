---
name: compiler-and-replay
description: >-
  Guide for modifying the 4-layer monotonic prompt compiler (internal/compiler), virtual memory
  @page spilling and hybrid RRF search (internal/vmem, internal/embed), and ModeExact SHA-256
  replay or SFT/DPO trajectory export (internal/replay). Use when editing prompt compilation,
  KV-cache prefix stability, FrameDelta, or replay/export logic.
---

# Prompt Compiler, Virtual Memory & Replay

Reference architecture: [architecture.md](../../references/architecture.md) and `docs/rfcs/0001-soma-v2-architecture.md`.

## Key Files

- `internal/compiler/compiler.go` — 4-layer prompt assembly (`L0`, `L1`, `L2`, `L3`) and `PrefixHashL0L1`
- `internal/compiler/profile.go` — Model-specific token budgets and prompt rendering profiles
- `internal/compiler/reconstruct.go` — `FrameDelta` application and `ReconstructFrame` SHA-256 verification
- `internal/vmem/vmem.go`, `internal/vmem/preview.go` — `@page_XXXX` zstd blob spilling, structural previewers, `page_grep`/`page_slice`/`page_search`
- `internal/embed/embed.go` — `LocalSimHashEmbedder` + 256-dim `int8` Matryoshka `/v1/embeddings` RRF
- `internal/replay/replay.go`, `internal/replay/exporter.go` — `ModeExact` replay and verified SFT/DPO-GRPO export

## Rules When Editing

1. **Never mutate `Layer 0` or `Layer 1` mid-epoch.** Any dynamic discovery (activated skill body, deferred MCP schema from `tool_search`, `memory_search`/`page_search` output, or Blackboard peer notice) must append to `Layer 2` (`[]compiler.LogEntry`).
2. **Keep `FrameDelta` complete and deterministic.** Every change to `Layer 2` or `Layer 3` rendering must be faithfully captured in `FrameDelta` so `ReconstructFrame` reproduces the exact byte sequence and SHA-256 hash.
3. **Avoid nondeterministic iteration.** Sort map keys before serializing schemas, memories, graveyard entries, or metadata into any prompt layer.
4. **Spill large tool outputs to `@page_XXXX`.** Do not inline bulky raw tool output into `Layer 2`; spill via `vmem` with a bounded structural preview so prompt tokens stay bounded without lossy compaction.
5. **Preserve epoch rollover boundaries.** When rolling from `Epoch N` to `Epoch N+1`, emit a clean Handoff Packet into the new epoch's frozen `Layer 1` rather than rewriting history in place.

## Validation

```bash
go test -count=1 -race -v ./internal/compiler/... ./internal/vmem/... ./internal/embed/... ./internal/replay/...
go test -count=1 -race -v -run "TestB1|TestB6" ./benchmarks/...
```

Confirm:
- `PrefixHashL0L1` stays identical across every step within an epoch.
- `ModeExact` SHA-256 frame reconstruction is `100%`.
- Suite B1 delta storage remains under `15%` of full-frame storage with `0` compactions.
