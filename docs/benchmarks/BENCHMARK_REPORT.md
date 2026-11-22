# `soma` v0.4 Reproducible Shadow Gauntlet Benchmark Report (Suite B6)

- **Generated At**: `2026-11-22T15:46:05Z`
- **Kernel Version**: `v0.4.0 (RFC-0001 v2.1 Complete — Shadow Gauntlet & Branching Search)`
- **Reference Baseline**: Hermes Forensic Audit (hackuj/lila/pchan, Nov 2026)
- **Suite B6 Acceptance Gate**: **PASS** (`soma` `100%` vs. Linear `25%` = **4.00x**, `ExactReplay=100%`, `Compactions=0`)

---

## 1. Executive Scorecard (`soma` vs. Linear Harness)

| Metric | `soma` | Linear Baseline | RFC-0001 v2.1 Gate |
|---|---:|---:|---|
| Verified Task Completion Rate | **100.0%** | 25.0% | >= 2.0x linear (**4.00x**) |
| Average Prefix Cache Hit Rate | **93.12%** | < 85% (lossy compaction) | >= 85% |
| Total LLM Text Compactions | **0** | High (`567` observed) | `0` |
| SHA-256 `ModeExact` Replay Pass Rate | **100%** | N/A (unhashed log) | `100%` |
| Net Token Reduction vs. Unpaged Linear | **59.9%** | Baseline | >= 40% on churny runs |

---

## 2. Empirical Hermes Baseline Telemetry

| Failure Mode | Observed Production Telemetry (`state.db` / `agent.log`) | `soma` Architectural Eliminator |
|---|---|---|
| Lossy Compactions | `567` compactions across `324` sessions | `@page` Virtual Memory + Immutable L0/L1 Epochs (`0` compactions) |
| HTTP 429 & Orphan Reaps | `417/781` (53.4%) 429 deaths; `739/1662` (44.5%) orphan reaps | SQLite Cooldown Scheduler + Epoch Handoff Failover |
| Unattended Approvals & Curator Bloat | `547` `BLOCKED:` tool denials; `150` `.curator_ledger.jsonl` edits | Scoped `bwrap`/Broker Policies + Immutable Lint-Gated Skills |

---

## 3. Domain-by-Domain Shadow Gauntlet Results

| Task ID | Domain | `soma` | Linear | Branches | Graveyard | Turns | Cache Hit | Oracles | Replay (`exact`) | Notes |
|---|---|---|---|---:|---:|---:|---:|---|---|---|
| `shadow-coding-01` | `coding` | **PASS** | FAIL | 1 | 0 | 4 | 93.0% | 1/2 runs | PASS (4 steps) | Oracle caught Turn-2 premature Done cleanly; repaired & passed in git_worktree |
| `shadow-ops-01` | `ssh_ops` | **PASS** | FAIL | 1 | 0 | 4 | 93.4% | policy+lease | PASS (4 steps) | Blocked metacharacter injection; ran lila & pchan checks under remote_leases (vs Hermes 547 BLOCKED errors) |
| `shadow-ml-lala-01` | `ml_rd` | **PASS** | FAIL | 3 | 2 | 10 | 94.4% | 2/2 tracks | PASS (12 steps) | Abandoned 2 trapped branches into Graveyard; 3rd copy_snapshot branch passed Track D+E |
| `shadow-quota-01` | `quota_chaos` | **PASS** | PASS (degraded) | 1 | 0 | 2 | 91.7% | 0 lost steps | PASS (2 steps) | Epoch 2 Handoff Packet avoided 429 reap; primary preference restored after cooldown (linear stuck on fallback) |

---

## 4. Reproducible Verification & Time-Travel Inspection

```bash
# Reproduce full test suite + B1-B6 benchmarks under -race
go test -count=1 -race -v ./benchmarks/...

# Inspect or regenerate this shadow gauntlet report cleanly
make shadow
# Exact SHA-256 replay for shadow-coding-01 (coding): soma replay br_18ea688f2958de5a_0002 --mode exact (head step: step_9f1bd5e850da81d741d5799b)
# Exact SHA-256 replay for shadow-ops-01 (ssh_ops): soma replay br_18ea688f2b1656ba_0002 --mode exact (head step: step_d7ef18d0a93ca938ac456de8)
# Exact SHA-256 replay for shadow-ml-lala-01 (ml_rd): soma replay br_18ea688f2bfcac54_0009 --mode exact (head step: step_86289b45fd84226ebedc6d09)
# Exact SHA-256 replay for shadow-quota-01 (quota_chaos): soma replay br_18ea688f2ca7237a_0002 --mode exact (head step: step_71b58a39dc10e5e7b173a39d)
```
