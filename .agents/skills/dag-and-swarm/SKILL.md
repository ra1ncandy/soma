---
name: dag-and-swarm
description: >-
  Guide for modifying the Merkle DAG executor, canonical tools, 2-phase action journal, LLM
  provider / multi-turn KV cache, BranchSearcher, SwarmCoordinator, Layer-2 Blackboard, and
  3-way Git worktree merging (internal/dag, internal/workspace). Use when adding tools, changing
  execution flow, debugging provider/retry behavior, or updating swarm/merge logic.
---

# DAG Executor, Tools, Branching & Swarm

## Key Files

- `internal/dag/executor.go` — Step loop, 16 canonical tools, 2-phase `action_journal`, taint & oracle gating
- `internal/dag/provider.go` — OpenAI-compatible HTTP provider, native multi-turn KV cache framing, streaming/retries
- `internal/dag/patch.go` — Forgiving multi-hunk `fs_patch` (SEARCH/REPLACE, JSON, unified diff) with atomic rollback
- `internal/dag/bgproc.go` (`_unix.go`, `_windows.go`) — Sandboxed `bg_exec`, `bg_poll` (`@page` spilling), `bg_kill`
- `internal/dag/search.go` — `BranchSearcher`, stall breaker, and Graveyard (`negative_path`) backtracking
- `internal/dag/swarm.go` & `internal/dag/blackboard.go` — Cooperative & competitive swarms and cache-safe `Layer 2` Blackboard bus
- `internal/dag/scheduler.go` — Multi-account `QuotaScheduler` and 429 failover
- `internal/workspace/workspace.go` & `internal/workspace/merge.go` — Isolated per-branch `git worktree`s and 3-way `git merge-file` (`MergeBranchWorkspaces`)

## Checklist for Changes

1. **Adding or Modifying a Tool (`internal/dag/executor.go`)**:
   - Classify its effect explicitly in `internal/policy` (`read_only`, `workspace_write`, `external_write`, `irreversible_write`).
   - Route execution through the 2-phase `action_journal` (`prepared` → `dispatched` → `committed` / `ambiguous`).
   - If the tool executes shell commands, route it through the fail-closed `internal/sandbox` backend (including background jobs).
   - If the tool fetches external/untrusted data, persist its output as an `@page` with `TrustTier: "untrusted_external"`.
2. **Provider & KV-Cache Changes (`internal/dag/provider.go`)**:
   - Keep prompt prefix bytes deterministic across turns in the same epoch so provider-side KV caches remain warm.
   - Respect `QuotaScheduler` failover and never silently swallow 429/truncation errors.
3. **Swarm & 3-Way Worktree Merge (`internal/dag/swarm.go`, `internal/workspace/merge.go`)**:
   - Cooperative workers must share `PrefixHashL0L1` and communicate discoveries strictly via `Layer 2` Blackboard (`publish_finding`).
   - Competitive workers must broadcast `graveyard_notice` on stall/failure.
   - Always test conflicting and non-conflicting multi-branch edits in `internal/workspace/merge_test.go` and verify the root `ContractVerifier` oracle gates final completion.

## Validation

```bash
go test -count=1 -race -v ./internal/dag/... ./internal/workspace/... ./internal/store/...
go test -count=1 -race -v -run "TestB2|TestB6" ./benchmarks/...
```
