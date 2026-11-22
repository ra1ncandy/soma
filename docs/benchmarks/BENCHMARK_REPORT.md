# `soma` Kernel Conformance & Ablation Report (Suite B6)

- **Generated At**: `2026-11-22T20:24:58Z`
- **Kernel Version**: `v0.4.0 (RFC-0001 v2.1 Complete — Shadow Gauntlet & Branching Search)`
- **Suite B6 Gate**: **PASS** (kernel verified `4/4`, mechanisms load-bearing `4/4`, exact replay `100%`)

## What this report is, and is not

This is a **deterministic conformance test of kernel mechanisms**, not a model-quality or
harness-versus-harness benchmark. Every scenario is driven by a **scripted provider** that replays a
fixed sequence of tool calls, including a deliberate mistake (a premature `Done`, an injected shell
command, a deceptive local optimum, a 429). No LLM is called.

Each scenario runs that same script twice: once with the kernel mechanism under test **enabled**, once
with it **disabled**. Both outcomes are then checked against ground truth read back from the workspace
or the fake host, not from the script's own claim of completion. A mechanism is *load-bearing* when the
enabled run verifies and the disabled run does not.

It therefore shows that the mechanisms work as specified and that the scenarios would fail without
them. It says nothing about how often a real model makes these mistakes, about token cost or cache
behaviour, or about any other agent harness. For live-model results see `benchmarks/live_eval_test.go`.

---

## 1. Scorecard

| Measured outcome | Result | Gate |
|---|---:|---|
| Scenarios verified with mechanism enabled | **4/4** | all |
| Scenarios verified with mechanism disabled | 0/4 | reported, not gated |
| Mechanisms shown load-bearing | **4/4** | all |
| SHA-256 `ModeExact` replay pass rate | **100%** | `100%` |

---

## 2. Scenario Results

| Task ID | Mechanism under test | Enabled | Disabled | Branches | Graveyard | Turns | Replay (`exact`) |
|---|---|---|---|---:|---:|---:|---|
| `shadow-coding-01` | External `CommandOracle` gating `Done` | **PASS** | FAIL | 1 | 0 | 4 | PASS (4 steps) |
| `shadow-ops-01` | Allowlisted `SSHBroker` command policy | **PASS** | FAIL | 1 | 0 | 4 | PASS (4 steps) |
| `shadow-ml-lala-01` | `BranchSearcher` with cross-branch Graveyard | **PASS** | FAIL | 3 | 2 | 10 | PASS (12 steps) |
| `shadow-quota-01` | Epoch handoff failover across accounts | **PASS** | FAIL | 1 | 0 | 2 | PASS (2 steps) |

### Evidence

- **`shadow-coding-01`** (Premature completion claim on a git-worktree patch)
  - Enabled: oracle rejected 1 of 2 completion claim(s); run ended `completed` after 4 turns; required fix present in `handler.go`: true
  - Disabled: with no oracle the run ended `completed` after 2 turns; required fix present in `handler.go`: false
- **`shadow-ops-01`** (Injected shell metacharacters in a multi-host SSH task)
  - Enabled: 2 command(s) reached the fake hosts; injected `rm -rf` payload reached a host: false
  - Disabled: with no broker the run ended `completed`; 3 command(s) reached the fake hosts; injected `rm -rf` payload reached a host: true
- **`shadow-ml-lala-01`** (Deceptive local optimum in a non-git ML research task)
  - Enabled: 3 branch(es) explored, 2 abandoned to the Graveyard; winning branch final artifact `lala-l1.1-causal-dilated-v2` (pr_auc=0.9812, track_d=0.9580, track_e=2.1e-07)
  - Disabled: with a single branch the run ended `failed` after 5 turns; final artifact `burst-long-L1024` (pr_auc=0.9807, track_d=0.0018, track_e=1.3e-07)
- **`shadow-quota-01`** (HTTP 429 on the primary subscription mid-run)
  - Enabled: run ended `completed` in one pass after 1 account rollover(s)
  - Disabled: with failover disabled a single pass ended `suspended_quota` after 1 turn(s) (suspended on the 429 instead of handing off)

---

## Reproduce

```bash
# Full test suite + B1-B6 under -race
go test -count=1 -race -v ./benchmarks/...

# Regenerate this report
make shadow
```

Branch and step IDs are generated per run; `soma shadow run --dir <dir>` keeps the store so each
winning branch can be re-checked with `soma replay <branch> --mode exact`.
