package benchmarks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ra1ncandy/soma/internal/broker"
	"github.com/ra1ncandy/soma/internal/dag"
	"github.com/ra1ncandy/soma/internal/oracle"
	"github.com/ra1ncandy/soma/internal/sandbox"
	"github.com/ra1ncandy/soma/internal/store"
	"github.com/ra1ncandy/soma/internal/taint"
	"github.com/ra1ncandy/soma/internal/vmem"
	"github.com/ra1ncandy/soma/internal/workspace"
)

func seedDummyStep(t *testing.T, ctx context.Context, st *store.Store, runID, branchID, stepID string) {
	t.Helper()
	snapID := "snap_" + stepID
	if err := st.PutEpochSnapshot(ctx, store.EpochSnapshot{
		ID:             snapID,
		RunID:          runID,
		BranchID:       branchID,
		Epoch:          1,
		Layer0BlobHash: "l0_" + stepID,
		Layer1BlobHash: "l1_" + stepID,
		PrefixHashL0L1: "pref_" + stepID,
		MetadataJSON:   "{}",
	}); err != nil {
		t.Fatalf("PutEpochSnapshot: %v", err)
	}
	if err := st.AppendStep(ctx, store.Step{
		ID:                 stepID,
		RunID:              runID,
		BranchID:           branchID,
		Epoch:              1,
		TurnIndex:          1,
		AccountID:          "acc_off",
		ModelID:            "mock",
		StateHash:          "sh_" + stepID,
		EpochSnapshotID:    snapID,
		FrameDeltaBlobHash: "fd_" + stepID,
		CompiledFrameHash:  "cf_" + stepID,
		EmittedBlobHash:    "em_" + stepID,
	}, store.BranchUpdate{
		Epoch:  1,
		Status: "running",
	}); err != nil {
		t.Fatalf("AppendStep(%s): %v", stepID, err)
	}
}

func TestLiveHardConfig_ModelSelectionAndEnvFallbacks(t *testing.T) {
	t.Setenv("SOMA_LIVE_API_KEY", "")
	t.Setenv("OPENCODE_API_KEY", "oc-fallback-key")
	t.Setenv("SOMA_LIVE_BASE_URL", "")
	t.Setenv("SOMA_LIVE_MODEL", "")
	t.Setenv("SOMA_LIVE_MODELS", "")
	t.Setenv("SOMA_LIVE_MAX_TOKENS", "")
	t.Setenv("SOMA_LIVE_REASONING_EFFORT", "")

	cfgs := loadLiveConfigs(t)
	if len(cfgs) != 1 {
		t.Fatalf("expected 1 default config, got %d", len(cfgs))
	}
	if cfgs[0].APIKey != "oc-fallback-key" ||
		cfgs[0].BaseURL != "https://opencode.ai/zen/go/v1" ||
		cfgs[0].ModelID != "deepseek-v4.1-flash" ||
		cfgs[0].MaxTokens != 16384 {
		t.Fatalf("unexpected default config: %+v", cfgs[0])
	}

	t.Setenv("SOMA_LIVE_API_KEY", "live-key")
	t.Setenv("SOMA_LIVE_MODEL", "glm-5.3-flash")
	t.Setenv("SOMA_LIVE_MAX_TOKENS", "12288")
	t.Setenv("SOMA_LIVE_REASONING_EFFORT", "high")
	single := loadLiveConfigs(t)
	if len(single) != 1 || single[0].ModelID != "glm-5.3-flash" || single[0].MaxTokens != 12288 || single[0].ReasoningEffort != "high" {
		t.Fatalf("unexpected single model config: %+v", single)
	}
	prov := newLiveProvider(single[0])
	if prov.MaxTokens != 12288 || prov.ReasoningEffort != "high" {
		t.Fatalf("newLiveProvider did not copy config: MaxTokens=%d ReasoningEffort=%q", prov.MaxTokens, prov.ReasoningEffort)
	}

	t.Setenv("SOMA_LIVE_MODELS", "deepseek-v4.1-flash, glm-5.3-flash")
	multi := loadLiveConfigs(t)
	if len(multi) != 2 || multi[0].ModelID != "deepseek-v4.1-flash" || multi[1].ModelID != "glm-5.3-flash" {
		t.Fatalf("unexpected multi-model configs: %+v", multi)
	}
}

func TestHard1Fixture_CorrelationRedHerringsAndValidation(t *testing.T) {
	ws := t.TempDir()
	if err := seedHard1Workspace(ws); err != nil {
		t.Fatalf("seedHard1Workspace: %v", err)
	}

	var totalBytes int64
	var decoyErrors int
	entries, err := os.ReadDir(filepath.Join(ws, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(ws, "logs", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		totalBytes += int64(len(raw))
		decoyErrors += strings.Count(string(raw), " ERROR ") + strings.Count(string(raw), " WARN ")
	}
	if totalBytes <= 1_200_000 {
		t.Fatalf("expected > 1.2MB of noisy logs, got %d bytes", totalBytes)
	}
	if decoyErrors < 50 {
		t.Fatalf("expected >= 50 ERROR/WARN lines across logs, got %d", decoyErrors)
	}

	if ok, _ := validateHard1Workspace(ws); ok {
		t.Fatal("expected unedited Hard1 workspace to fail validation")
	}

	// Verify clobbering sibling pools fails validation even when pool_gamma is right.
	clobberedYAML := `pools:
  pool_alpha:
    timeout_ms: 9999
    max_connections: 64
    region: us-east
  pool_beta:
    timeout_ms: 1500
    max_connections: 96
    region: us-central
  pool_gamma:
    timeout_ms: 9745
    max_connections: 84
    region: us-west
  pool_delta:
    timeout_ms: 2100
    max_connections: 48
    region: eu-west
  pool_epsilon:
    timeout_ms: 950
    max_connections: 32
    region: ap-south
`
	_ = os.WriteFile(filepath.Join(ws, "config", "routing.yaml"), []byte(clobberedYAML), 0o644)
	_ = os.WriteFile(filepath.Join(ws, "config", "limits.json"), []byte(`{
  "shards": {
    "shard_01": {"backoff_jitter_ms": 10, "drain_node": "", "qps_limit": 400},
    "shard_04": {"backoff_jitter_ms": 15, "drain_node": "", "qps_limit": 650},
    "shard_07": {"backoff_jitter_ms": 175, "drain_node": "west-14", "qps_limit": 900},
    "shard_09": {"backoff_jitter_ms": 30, "drain_node": "", "qps_limit": 300}
  }
}`), 0o644)
	_ = os.WriteFile(filepath.Join(ws, "incident_report.json"), []byte(`{
  "trace_id": "tr_9c4e71a2",
  "tenant_shard": "shard_07",
  "upstream_pool": "pool_gamma",
  "node": "west-14",
  "batch_id": "b_88412",
  "computed_timeout_ms": 9745,
  "effective_max_conns": 84
}`), 0o644)

	if ok, detail := validateHard1Workspace(ws); ok {
		t.Fatalf("expected clobbered pool_alpha to fail validation, got pass: %s", detail)
	}

	// Restore exact pool_alpha value; now validation must pass.
	fixedYAML := strings.Replace(clobberedYAML, "timeout_ms: 9999", "timeout_ms: 800", 1)
	_ = os.WriteFile(filepath.Join(ws, "config", "routing.yaml"), []byte(fixedYAML), 0o644)
	if ok, detail := validateHard1Workspace(ws); !ok {
		t.Fatalf("expected exact Hard1 fix to pass validation, got fail: %s", detail)
	}
}

func TestHard2Fixture_LedgerBugsPinnedTamperAndReferenceFix(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	if err := seedHard2Workspace(ws); err != nil {
		t.Fatalf("seedHard2Workspace: %v", err)
	}
	pubPath := filepath.Join(ws, "tests", "test_public.py")
	pubHash, err := oracle.HashFileSHA256(pubPath)
	if err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(filepath.Join(ws, ".soma"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	_ = st.CreateRun(ctx, store.Run{ID: "run_h2", Title: "hard2", WorkspacePath: ws, GoalSpec: "fix ledger"})
	_ = st.CreateBranch(ctx, store.Branch{ID: "br_h2", RunID: "run_h2", Label: "main", WorkspaceDir: ws, Epoch: 1, Status: "running"})
	seedDummyStep(t, ctx, st, "run_h2", "br_h2", "step_0")
	seedDummyStep(t, ctx, st, "run_h2", "br_h2", "step_tamper")
	seedDummyStep(t, ctx, st, "run_h2", "br_h2", "step_fixed")
	cmdSpec, _ := json.Marshal(oracle.CommandOracleSpec{
		Command:     resolvePython3(),
		Args:        []string{"-c", hard2HeldOutOraclePy},
		PinnedFiles: map[string]string{"tests/test_public.py": pubHash},
	})
	_ = st.CreateContract(ctx, store.Contract{
		ID: "ctr_h2", RunID: "run_h2", Kind: "command_oracle", SpecJSON: string(cmdSpec), PinnedHash: pubHash, Required: true,
	})

	ver := oracle.NewVerifier(st, sandbox.NewManager())
	runRow, _ := st.GetRun(ctx, "run_h2")
	brRow, _ := st.GetBranch(ctx, "br_h2")

	// 1. Starter must pass test_public.py (shallow trap) yet FAIL held-out CommandOracle.
	pubCmd := exec.CommandContext(ctx, resolvePython3(), "-m", "unittest", "tests/test_public.py")
	pubCmd.Dir = ws
	if out, err := pubCmd.CombinedOutput(); err != nil {
		t.Fatalf("expected starter to pass shallow public smoke tests, got err=%v\n%s", err, string(out))
	}
	sumStarter, err := ver.VerifyBranch(ctx, runRow, brRow, "step_0")
	if err != nil {
		t.Fatalf("VerifyBranch starter: %v", err)
	}
	if sumStarter.AllPassed {
		t.Fatal("expected starter Hard2 workspace to FAIL held-out CommandOracle")
	}

	// 2. Tampering pinned tests/test_public.py must trigger ErrPinnedInvariantTampered.
	origPub, _ := os.ReadFile(pubPath)
	_ = os.WriteFile(pubPath, []byte("# tampered\n"), 0o644)
	sumTamper, err := ver.VerifyBranch(ctx, runRow, brRow, "step_tamper")
	if err != nil {
		t.Fatalf("VerifyBranch tamper: %v", err)
	}
	if sumTamper.AllPassed || !strings.Contains(sumTamper.Counterexample, oracle.ErrPinnedInvariantTampered.Error()) {
		t.Fatalf("expected ErrPinnedInvariantTampered, got %+v", sumTamper)
	}
	_ = os.WriteFile(pubPath, origPub, 0o644)

	// 3. Apply clean reference fixes to all 4 ledger modules and prove CommandOracle passes.
	refCurrency := `import re

_AMOUNT_RE = re.compile(r"^(\d+)(?:\.(\d{1,2}))?$")


def to_cents(amount_str: str) -> int:
    if not isinstance(amount_str, str):
        raise ValueError("amount must be string")
    s = amount_str.strip()
    m = _AMOUNT_RE.fullmatch(s)
    if not m:
        raise ValueError(f"invalid currency amount: {amount_str!r}")
    dollars = int(m.group(1))
    frac = (m.group(2) or "").ljust(2, "0")
    return dollars * 100 + int(frac)


def split_cents(total_cents: int, weights: dict[str, int]) -> dict[str, int]:
    if isinstance(total_cents, bool) or not isinstance(total_cents, int) or total_cents < 0:
        raise ValueError("total_cents must be non-negative int")
    if not isinstance(weights, dict) or not weights:
        raise ValueError("weights must be non-empty dict")
    total_w = 0
    for w in weights.values():
        if isinstance(w, bool) or not isinstance(w, int) or w < 0:
            raise ValueError("weight must be non-negative int")
        total_w += w
    if total_w <= 0:
        raise ValueError("weight sum must be positive")

    out: dict[str, int] = {}
    rems: list[tuple[int, str]] = []
    allocated = 0
    for k, w in weights.items():
        num = total_cents * w
        share = num // total_w
        out[k] = share
        allocated += share
        rems.append((num % total_w, k))

    leftover = total_cents - allocated
    rems.sort(key=lambda item: (-item[0], item[1]))
    for i in range(leftover):
        _, key = rems[i]
        out[key] += 1
    return out
`
	refLocks := `class DeadlockError(Exception):
    pass


class LockContentionError(Exception):
    pass


class LockGraph:
    def __init__(self) -> None:
        self.holders: dict[str, str] = {}
        self.waits_for: dict[str, set[str]] = {}

    def _can_reach(self, start: str, target: str) -> bool:
        stack = [start]
        visited: set[str] = set()
        while stack:
            cur = stack.pop()
            if cur == target:
                return True
            if cur in visited:
                continue
            visited.add(cur)
            for nxt in self.waits_for.get(cur, ()):
                if nxt not in visited:
                    stack.append(nxt)
        return False

    def acquire_batch(self, tx_id: str, accounts: list[str]) -> None:
        unique: list[str] = []
        seen: set[str] = set()
        for a in accounts:
            if a not in seen:
                seen.add(a)
                unique.append(a)

        blockers = {
            self.holders[a]
            for a in unique
            if a in self.holders and self.holders[a] != tx_id
        }
        if not blockers:
            self.waits_for.pop(tx_id, None)
            for a in unique:
                self.holders[a] = tx_id
            return

        existing = self.waits_for.setdefault(tx_id, set())
        added = blockers - existing
        existing.update(added)

        deadlock = any(self._can_reach(b, tx_id) for b in blockers)
        if deadlock:
            existing.difference_update(added)
            if not existing:
                self.waits_for.pop(tx_id, None)
            raise DeadlockError(f"cycle detected involving {tx_id}")

        raise LockContentionError(f"{tx_id} blocked on {sorted(blockers)}")

    def release(self, tx_id: str) -> None:
        for acct in [a for a, h in self.holders.items() if h == tx_id]:
            del self.holders[acct]
        self.waits_for.pop(tx_id, None)
        for waiters in self.waits_for.values():
            waiters.discard(tx_id)
`
	refEngine := `class InsufficientFundsError(Exception):
    pass


class CurrencyMismatchError(Exception):
    pass


class IdempotencyConflictError(Exception):
    pass


class LedgerEngine:
    def __init__(self) -> None:
        self.accounts: dict[str, dict[str, object]] = {}
        self._idempotency: dict[str, tuple[tuple[tuple[str, str, int, str], ...], dict]] = {}

    def create_account(self, account_id: str, currency: str, initial_balance_cents: int = 0) -> None:
        if not account_id or not currency:
            raise ValueError("account_id and currency required")
        if isinstance(initial_balance_cents, bool) or not isinstance(initial_balance_cents, int) or initial_balance_cents < 0:
            raise ValueError("initial_balance_cents must be non-negative int")
        if account_id in self.accounts:
            raise ValueError(f"duplicate account: {account_id}")
        self.accounts[account_id] = {"currency": currency, "balance": initial_balance_cents}

    def get_balance(self, account_id: str) -> int:
        if account_id not in self.accounts:
            raise KeyError(account_id)
        return int(self.accounts[account_id]["balance"])

    def transfer_batch(self, idempotency_key: str, legs: list[tuple[str, str, int, str]]) -> dict:
        if not isinstance(idempotency_key, str) or not idempotency_key or not legs:
            raise ValueError("idempotency_key and non-empty legs required")

        canonical: list[tuple[str, str, int, str]] = []
        for leg in legs:
            if not isinstance(leg, tuple) or len(leg) != 4:
                raise ValueError("invalid leg tuple")
            from_acct, to_acct, amt, ccy = leg
            if isinstance(amt, bool) or not isinstance(amt, int) or amt <= 0:
                raise ValueError("leg amount must be positive int")
            canonical.append((str(from_acct), str(to_acct), amt, str(ccy)))
        canon_key = tuple(canonical)

        if idempotency_key in self._idempotency:
            prev_legs, prev_receipt = self._idempotency[idempotency_key]
            if prev_legs != canon_key:
                raise IdempotencyConflictError(f"idempotency_key {idempotency_key} reused with different legs")
            return dict(prev_receipt)

        sim = {aid: int(info["balance"]) for aid, info in self.accounts.items()}
        total = 0
        for from_acct, to_acct, amt, ccy in canon_key:
            if from_acct not in self.accounts:
                raise KeyError(from_acct)
            if to_acct not in self.accounts:
                raise KeyError(to_acct)
            if self.accounts[from_acct]["currency"] != ccy or self.accounts[to_acct]["currency"] != ccy:
                raise CurrencyMismatchError("currency mismatch")
            if sim[from_acct] < amt:
                raise InsufficientFundsError(f"insufficient funds on {from_acct}")
            sim[from_acct] -= amt
            sim[to_acct] += amt
            total += amt

        for aid, bal in sim.items():
            self.accounts[aid]["balance"] = bal

        receipt = {
            "idempotency_key": idempotency_key,
            "status": "committed",
            "legs_count": len(canon_key),
            "total_cents": total,
        }
        self._idempotency[idempotency_key] = (canon_key, receipt)
        return dict(receipt)
`
	refReplay := `class EventConflictError(Exception):
    pass


class EventLog:
    def __init__(self) -> None:
        self._by_seq: dict[int, tuple[str, int]] = {}

    def append(self, seq: int, account_id: str, delta_cents: int) -> None:
        if isinstance(seq, bool) or not isinstance(seq, int) or seq < 1:
            raise ValueError("seq must be >= 1")
        if isinstance(delta_cents, bool) or not isinstance(delta_cents, int):
            raise ValueError("delta_cents must be int")
        payload = (str(account_id), delta_cents)
        if seq in self._by_seq:
            if self._by_seq[seq] != payload:
                raise EventConflictError(f"conflicting payload for seq={seq}")
            return
        self._by_seq[seq] = payload

    def snapshot(self, as_of_seq: int) -> dict[str, int]:
        return self.replay_state(as_of_seq)

    def replay_state(self, as_of_seq: int) -> dict[str, int]:
        if isinstance(as_of_seq, bool) or not isinstance(as_of_seq, int) or as_of_seq < 0:
            raise ValueError("as_of_seq must be non-negative int")
        balances: dict[str, int] = {}
        for seq in sorted(self._by_seq):
            if seq <= as_of_seq:
                acct, delta = self._by_seq[seq]
                balances[acct] = balances.get(acct, 0) + delta
        return balances
`

	for rel, content := range map[string]string{
		"ledger/currency.py": refCurrency,
		"ledger/locks.py":    refLocks,
		"ledger/engine.py":   refEngine,
		"ledger/replay.py":   refReplay,
	} {
		if err := os.WriteFile(filepath.Join(ws, filepath.FromSlash(rel)), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	sumFixed, err := ver.VerifyBranch(ctx, runRow, brRow, "step_fixed")
	if err != nil {
		t.Fatalf("VerifyBranch fixed: %v", err)
	}
	if !sumFixed.AllPassed {
		t.Fatalf("expected fixed Hard2 reference to pass CommandOracle, got counterexample:\n%s", sumFixed.Counterexample)
	}
}

func TestHard3Fixture_CausalDriftAntiCheeseAndReferenceFix(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	staleSHA, err := seedHard3Workspace(ws)
	if err != nil {
		t.Fatalf("seedHard3Workspace: %v", err)
	}

	st, err := store.Open(filepath.Join(ws, ".soma"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	_ = st.CreateRun(ctx, store.Run{ID: "run_h3", Title: "hard3", WorkspacePath: ws, WorkspaceKind: "research", GoalSpec: "causal drift"})
	_ = st.CreateBranch(ctx, store.Branch{ID: "br_h3", RunID: "run_h3", Label: "main", WorkspaceDir: ws, Epoch: 1, Status: "running"})
	seedDummyStep(t, ctx, st, "run_h3", "br_h3", "s0")
	seedDummyStep(t, ctx, st, "run_h3", "br_h3", "s_global")
	seedDummyStep(t, ctx, st, "run_h3", "br_h3", "s_ewma")
	seedDummyStep(t, ctx, st, "run_h3", "br_h3", "s_ref")

	cmdSpec, _ := json.Marshal(oracle.CommandOracleSpec{
		Command: resolvePython3(),
		Args:    []string{"-c", hard3HeldOutOraclePy},
	})
	_ = st.CreateContract(ctx, store.Contract{
		ID: "ctr_01_h3_cmd", RunID: "run_h3", Kind: "command_oracle", SpecJSON: string(cmdSpec), PinnedHash: "h3_cmd", Required: true,
	})
	artSpec, _ := json.Marshal(oracle.ArtifactOracleSpec{
		RequiredPaths:    []string{"artifacts/eval_report.json"},
		DisallowedHashes: map[string][]string{"artifacts/eval_report.json": {staleSHA}},
		RequiredJSONKeys: map[string][]string{"artifacts/eval_report.json": {"dev_f1", "causal_max_prefix_delta", "drift_stretched_f1", "latency_ms"}},
		MetricThresholds: []oracle.MetricThreshold{
			{Key: "dev_f1", Op: ">=", Value: 0.92},
			{Key: "causal_max_prefix_delta", Op: "<=", Value: 1e-9},
			{Key: "drift_stretched_f1", Op: ">=", Value: 0.85},
			{Key: "latency_ms", Op: "<=", Value: 250.0},
		},
	})
	_ = st.CreateContract(ctx, store.Contract{
		ID: "ctr_02_h3_art", RunID: "run_h3", Kind: "artifact_oracle", SpecJSON: string(artSpec), PinnedHash: "h3_art", Required: true,
	})

	ver := oracle.NewVerifier(st, sandbox.NewManager())
	runRow, _ := st.GetRun(ctx, "run_h3")
	brRow, _ := st.GetBranch(ctx, "br_h3")

	// 0. Verify pre-seeded starter artifacts/eval_report.json is rejected by ArtifactOracle.DisallowedHashes.
	_ = st.CreateRun(ctx, store.Run{ID: "run_h3_stale", Title: "hard3-stale", WorkspacePath: ws, WorkspaceKind: "research", GoalSpec: "stale check"})
	_ = st.CreateBranch(ctx, store.Branch{ID: "br_h3_stale", RunID: "run_h3_stale", Label: "main", WorkspaceDir: ws, Epoch: 1, Status: "running"})
	seedDummyStep(t, ctx, st, "run_h3_stale", "br_h3_stale", "s_stale")
	staleOnlySpec, _ := json.Marshal(oracle.ArtifactOracleSpec{
		RequiredPaths:    []string{"artifacts/eval_report.json"},
		DisallowedHashes: map[string][]string{"artifacts/eval_report.json": {staleSHA}},
	})
	_ = st.CreateContract(ctx, store.Contract{
		ID: "ctr_h3_stale", RunID: "run_h3_stale", Kind: "artifact_oracle", SpecJSON: string(staleOnlySpec), PinnedHash: "stale", Required: true,
	})
	staleRunRow, _ := st.GetRun(ctx, "run_h3_stale")
	staleBrRow, _ := st.GetBranch(ctx, "br_h3_stale")
	sumStale, err := ver.VerifyBranch(ctx, staleRunRow, staleBrRow, "s_stale")
	if err != nil {
		t.Fatal(err)
	}
	if sumStale.AllPassed || !strings.Contains(sumStale.Counterexample, "stale artifact rejected") {
		t.Fatalf("expected ArtifactOracle DisallowedHashes to reject pre-seeded eval_report.json, got %+v", sumStale)
	}

	// 1. Starter fails both contracts on full oracle evaluation.
	sumStarter, err := ver.VerifyBranch(ctx, runRow, brRow, "s0")
	if err != nil {
		t.Fatal(err)
	}
	if sumStarter.AllPassed || !strings.Contains(sumStarter.Counterexample, "dev_f1=0.0000") {
		t.Fatalf("expected starter to fail dev_f1=0.0000, got %+v", sumStarter)
	}

	// 2. Non-causal global z-score: passes dev_bench.py yet FAILS held-out oracle on causality.
	globalDetector := `import math
def score_stream(values: list[float]) -> list[float]:
    n = len(values)
    if n == 0:
        return []
    mu = sum(values) / n
    var = sum((x - mu) ** 2 for x in values) / max(1, n)
    sd = math.sqrt(max(var, 1e-9))
    return [abs(x - mu) / sd for x in values]
`
	_ = os.WriteFile(filepath.Join(ws, "detector.py"), []byte(globalDetector), 0o644)
	devCmd := exec.CommandContext(ctx, resolvePython3(), "dev_bench.py")
	devCmd.Dir = ws
	if devOut, err := devCmd.CombinedOutput(); err != nil || !strings.Contains(string(devOut), "dev_f1=1.0000") {
		t.Fatalf("expected global z-score to pass dev_bench.py, got out=%q err=%v", string(devOut), err)
	}
	sumGlobal, err := ver.VerifyBranch(ctx, runRow, brRow, "s_global")
	if err != nil {
		t.Fatal(err)
	}
	if sumGlobal.AllPassed || !strings.Contains(sumGlobal.Counterexample, "causal_max_prefix_delta") {
		t.Fatalf("expected global z-score to fail causal_max_prefix_delta, got %+v", sumGlobal)
	}

	// 3. Naive fast EWMA: passes dev_bench.py & causality, yet FAILS 60-sample plateau drift_stretched_f1.
	naiveEWMA := `import math
def score_stream(values: list[float]) -> list[float]:
    if not values:
        return []
    mu = values[0]
    var = 0.25
    alpha_mu = 0.05
    alpha_var = 0.01
    out = []
    for i, x in enumerate(values):
        sd = math.sqrt(max(var, 1e-6))
        z = abs(x - mu) / sd
        out.append(0.0 if i < 40 else z)
        err = x - mu
        mu = (1.0 - alpha_mu) * mu + alpha_mu * x
        var = (1.0 - alpha_var) * var + alpha_var * (err * err)
    return out
`
	_ = os.WriteFile(filepath.Join(ws, "detector.py"), []byte(naiveEWMA), 0o644)
	devCmd2 := exec.CommandContext(ctx, resolvePython3(), "dev_bench.py")
	devCmd2.Dir = ws
	if devOut, err := devCmd2.CombinedOutput(); err != nil || !strings.Contains(string(devOut), "dev_f1=1.0000") {
		t.Fatalf("expected naive EWMA to pass dev_bench.py, got out=%q err=%v", string(devOut), err)
	}
	sumEWMA, err := ver.VerifyBranch(ctx, runRow, brRow, "s_ewma")
	if err != nil {
		t.Fatal(err)
	}
	if sumEWMA.AllPassed || !strings.Contains(sumEWMA.Counterexample, "drift_stretched_f1") {
		t.Fatalf("expected naive fast EWMA to fail drift_stretched_f1, got %+v", sumEWMA)
	}

	// 4. Clean causal outlier-gated estimator: passes dev, causality, sustained plateaus + 5x variance shift, and latency.
	refDetector := `import math


def score_stream(values: list[float]) -> list[float]:
    n = len(values)
    if n == 0:
        return []
    out: list[float] = []
    warmup = min(40, n)
    s_sum = 0.0
    s_sq = 0.0
    for i in range(warmup):
        x = values[i]
        s_sum += x
        s_sq += x * x
        out.append(0.0)
    mu = s_sum / warmup
    var = max(1e-4, (s_sq / warmup) - (mu * mu))

    alpha_mu = 0.05
    alpha_var = 0.025
    for i in range(warmup, n):
        x = values[i]
        sd = math.sqrt(max(var, 1e-6))
        err = x - mu
        z = abs(err) / sd
        out.append(z)
        if z < 3.3:
            mu = (1.0 - alpha_mu) * mu + alpha_mu * x
        if z < 4.5:
            var = (1.0 - alpha_var) * var + alpha_var * (err * err)
    return out
`
	_ = os.WriteFile(filepath.Join(ws, "detector.py"), []byte(refDetector), 0o644)
	sumRef, err := ver.VerifyBranch(ctx, runRow, brRow, "s_ref")
	if err != nil {
		t.Fatal(err)
	}
	if !sumRef.AllPassed {
		t.Fatalf("expected causal gated reference detector to PASS both oracles, got counterexample:\n%s", sumRef.Counterexample)
	}
}

func TestHard4Fixture_GitWorktreeThreeWayMergeAndRootOracle(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	if err := seedHard4GitWorkspace(ctx, ws); err != nil {
		t.Fatalf("seedHard4GitWorkspace: %v", err)
	}

	st, err := store.Open(filepath.Join(ws, ".soma"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	runRow := store.Run{ID: "run_h4", Title: "hard4", WorkspacePath: ws, WorkspaceKind: "git", GoalSpec: "swarm"}
	if err := st.CreateRun(ctx, runRow); err != nil {
		t.Fatal(err)
	}

	wm := workspace.NewManager(filepath.Join(ws, ".soma"))
	workerDirs := make([]string, 3)
	for i, lbl := range []string{"worker-parser", "worker-validator", "worker-pipeline"} {
		br := store.Branch{ID: fmt.Sprintf("br_h4_%d", i), RunID: runRow.ID, Label: lbl, Epoch: 1, Status: "running"}
		if err := st.CreateBranch(ctx, br); err != nil {
			t.Fatal(err)
		}
		dir, strat, err := wm.PrepareBranchWorkspace(ctx, st, runRow, br)
		if err != nil {
			t.Fatal(err)
		}
		if strat != workspace.StrategyGitWorktree {
			t.Fatalf("expected git_worktree strategy for %s, got %s", lbl, strat)
		}
		workerDirs[i] = dir
	}

	// Worker 0: parser.py + PARSER section of registry.py + dummy __pycache__.
	parserPy := `from datetime import datetime


class ParseError(ValueError):
    pass


def _split_quoted_pipes(line: str) -> list[str]:
    fields: list[str] = []
    cur: list[str] = []
    in_quotes = False
    at_field_start = True
    i = 0
    n = len(line)
    while i < n:
        ch = line[i]
        if in_quotes:
            if ch == '"':
                if i + 1 < n and line[i + 1] == '"':
                    cur.append('"')
                    i += 2
                    continue
                in_quotes = False
                if i + 1 < n and line[i + 1] != '|':
                    raise ParseError("unexpected character after closing quote")
            else:
                cur.append(ch)
        else:
            if ch == '"':
                if not at_field_start:
                    raise ParseError("unexpected quote inside unquoted field")
                in_quotes = True
            elif ch == '|':
                fields.append("".join(cur))
                cur = []
                at_field_start = True
                i += 1
                continue
            else:
                cur.append(ch)
        at_field_start = False
        i += 1
    if in_quotes:
        raise ParseError("unclosed quoted field")
    fields.append("".join(cur))
    return fields


def parse_record(line: str) -> dict:
    if not isinstance(line, str) or line == "":
        raise ParseError("empty line")
    fields = _split_quoted_pipes(line)
    if len(fields) != 5:
        raise ParseError(f"expected 5 fields, got {len(fields)}")
    ts, level, amt_str, tags_str, msg = fields
    try:
        datetime.strptime(ts, "%Y-%m-%dT%H:%M:%SZ")
    except ValueError as exc:
        raise ParseError(f"invalid timestamp: {ts}") from exc
    if len(ts) != 20 or not ts.endswith("Z"):
        raise ParseError(f"invalid timestamp format: {ts}")
    if not level:
        raise ParseError("empty level")
    if not amt_str or amt_str != amt_str.strip():
        raise ParseError(f"invalid amount: {amt_str}")
    try:
        amount = int(amt_str)
    except ValueError as exc:
        raise ParseError(f"invalid integer amount: {amt_str}") from exc
    if str(amount) != amt_str.lstrip("+"):
        if not (amt_str.startswith("-") and str(amount) == amt_str):
            raise ParseError(f"non-canonical amount: {amt_str}")

    tags: dict[str, str] = {}
    if tags_str != "":
        for pair in tags_str.split(","):
            if "=" not in pair:
                raise ParseError(f"malformed tag: {pair}")
            k, v = pair.split("=", 1)
            if not k or not v:
                raise ParseError(f"empty key or value in tag: {pair}")
            tags[k] = v

    return {
        "timestamp": ts,
        "level": level,
        "amount": amount,
        "tags": tags,
        "message": msg,
    }
`
	if err := os.WriteFile(filepath.Join(workerDirs[0], "app", "parser.py"), []byte(parserPy), 0o644); err != nil {
		t.Fatal(err)
	}
	reg0Bytes, _ := os.ReadFile(filepath.Join(workerDirs[0], "app", "registry.py"))
	reg0Updated := strings.Replace(
		string(reg0Bytes),
		"PARSER_HOOKS = {}",
		"from app.parser import parse_record\nPARSER_HOOKS = {\"parse_record\": parse_record}",
		1,
	)
	_ = os.WriteFile(filepath.Join(workerDirs[0], "app", "registry.py"), []byte(reg0Updated), 0o644)

	// Worker 1: validator.py + VALIDATOR section of registry.py + dummy __pycache__.
	validatorPy := `def validate_record(rec: dict, rules: dict) -> list[str]:
    errs: set[str] = set()
    if "allowed_levels" in rules and rec.get("level") not in rules["allowed_levels"]:
        errs.add("INVALID_LEVEL")
    amt = rec.get("amount", 0)
    if ("min_amount" in rules and amt < rules["min_amount"]) or ("max_amount" in rules and amt > rules["max_amount"]):
        errs.add("AMOUNT_OUT_OF_RANGE")
    tags = rec.get("tags", {})
    for req_tag in rules.get("required_tags", []):
        if req_tag not in tags:
            errs.add(f"MISSING_TAG:{req_tag}")
    if "min_timestamp" in rules and rec.get("timestamp", "") < rules["min_timestamp"]:
        errs.add("TIMESTAMP_TOO_OLD")
    return sorted(errs)
`
	if err := os.WriteFile(filepath.Join(workerDirs[1], "app", "validator.py"), []byte(validatorPy), 0o644); err != nil {
		t.Fatal(err)
	}
	reg1Bytes, _ := os.ReadFile(filepath.Join(workerDirs[1], "app", "registry.py"))
	reg1Updated := strings.Replace(
		string(reg1Bytes),
		"VALIDATOR_HOOKS = {}",
		"from app.validator import validate_record\nVALIDATOR_HOOKS = {\"validate_record\": validate_record}",
		1,
	)
	_ = os.WriteFile(filepath.Join(workerDirs[1], "app", "registry.py"), []byte(reg1Updated), 0o644)

	for idx, wDir := range workerDirs[:2] {
		pycDir := filepath.Join(wDir, "app", "__pycache__")
		_ = os.MkdirAll(pycDir, 0o755)
		_ = os.WriteFile(filepath.Join(pycDir, "registry.cpython-312.pyc"), []byte(fmt.Sprintf("pyc-%d", idx)), 0o644)
	}

	// Worker 2: pipeline.py only.
	pipelinePy := `import hashlib
import json
from app.registry import PARSER_HOOKS, VALIDATOR_HOOKS


def run_pipeline(lines: list[str], rules: dict) -> dict:
    parse_record = PARSER_HOOKS["parse_record"]
    validate_record = VALIDATOR_HOOKS["validate_record"]

    accepted: list[dict] = []
    rejected = 0
    errors_by_code: dict[str, int] = {}
    total_accepted_amount = 0
    last_accepted_ts = None

    for line in lines:
        try:
            rec = parse_record(line)
        except ValueError:
            rejected += 1
            errors_by_code["PARSE_ERROR"] = errors_by_code.get("PARSE_ERROR", 0) + 1
            continue

        errs = validate_record(rec, rules)
        if errs:
            rejected += 1
            for code in errs:
                errors_by_code[code] = errors_by_code.get(code, 0) + 1
            continue

        if last_accepted_ts is not None and rec["timestamp"] < last_accepted_ts:
            rejected += 1
            errors_by_code["NON_MONOTONIC_TIMESTAMP"] = errors_by_code.get("NON_MONOTONIC_TIMESTAMP", 0) + 1
            continue

        accepted.append(rec)
        last_accepted_ts = rec["timestamp"]
        total_accepted_amount += rec["amount"]

    checksum = hashlib.sha256(
        json.dumps(accepted, sort_keys=True, separators=(",", ":")).encode("utf-8")
    ).hexdigest()
    return {
        "accepted": len(accepted),
        "rejected": rejected,
        "errors_by_code": errors_by_code,
        "total_accepted_amount": total_accepted_amount,
        "checksum_sha256": checksum,
    }
`
	if err := os.WriteFile(filepath.Join(workerDirs[2], "app", "pipeline.py"), []byte(pipelinePy), 0o644); err != nil {
		t.Fatal(err)
	}

	rep, err := wm.MergeBranchWorkspaces(ctx, ws, workerDirs, ws)
	if err != nil {
		t.Fatalf("MergeBranchWorkspaces failed: %v (conflicts=%v)", err, rep.Conflicts)
	}
	foundThreeWay := false
	for _, f := range rep.ThreeWayMerged {
		if f == "app/registry.py" {
			foundThreeWay = true
		}
	}
	if !foundThreeWay {
		t.Fatalf("expected app/registry.py in ThreeWayMerged, got %+v", rep)
	}

	cmdSpec, _ := json.Marshal(oracle.CommandOracleSpec{
		Command: resolvePython3(),
		Args:    []string{"-c", hard4RootOraclePy},
	})
	_ = st.CreateContract(ctx, store.Contract{
		ID: "ctr_h4", RunID: runRow.ID, Kind: "command_oracle", SpecJSON: string(cmdSpec), PinnedHash: "h4", Required: true,
	})
	rootBr := store.Branch{ID: "br_h4_root", RunID: runRow.ID, Label: "main", WorkspaceDir: ws, Epoch: 1}
	_ = st.CreateBranch(ctx, rootBr)
	seedDummyStep(t, ctx, st, runRow.ID, rootBr.ID, "s_h4")
	sum, err := oracle.NewVerifier(st, sandbox.NewManager()).VerifyBranch(ctx, runRow, rootBr, "s_h4")
	if err != nil {
		t.Fatalf("VerifyBranch Hard4: %v", err)
	}
	if !sum.AllPassed {
		t.Fatalf("Hard4 root oracle failed on merged workspace:\n%s", sum.Counterexample)
	}
}

func TestHard5Fixture_InjectionSSRFBiTemporalMemoryAndBrokerTaint(t *testing.T) {
	ctx := context.Background()
	srv := newHard5TestServer()
	defer srv.Close()

	// 1. web_extract on local server extracts factual release info and tags untrusted_external.
	out, err := dag.ExecuteWebExtract(ctx, srv.Client(), srv.URL+"/release-notes")
	if err != nil {
		t.Fatalf("ExecuteWebExtract: %v", err)
	}
	if out.TrustTier != "untrusted_external" {
		t.Fatalf("expected untrusted_external TrustTier, got %q", out.TrustTier)
	}
	if !strings.Contains(string(out.Content), "CVE-2026-4410") || !strings.Contains(string(out.Content), "sha256:9f86d081884c7d65") {
		t.Fatalf("extracted HTML missing factual metadata:\n%s", string(out.Content))
	}

	// 2. IMDS SSRF must STILL be blocked even when HTTPClient is provided.
	if _, err := dag.ExecuteWebExtract(ctx, srv.Client(), "http://169.254.169.254/latest/meta-data/"); !errors.Is(err, dag.ErrSSRFBlocked) {
		t.Fatalf("expected ErrSSRFBlocked on IMDSv4 even with custom HTTPClient, got %v", err)
	}

	// 3. Bi-temporal memory + MINJA quarantine + run-wide broker taint.
	ws := t.TempDir()
	st, err := store.Open(filepath.Join(ws, ".soma"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	if err := seedHard5Memory(ctx, st); err != nil {
		t.Fatalf("seedHard5Memory: %v", err)
	}
	active, err := st.ListActiveMemoryByScopes(ctx, []string{"global"}, []string{"rule"})
	if err != nil {
		t.Fatal(err)
	}
	activeIDs := make(map[string]bool, len(active))
	for _, n := range active {
		activeIDs[n.ID] = true
	}
	if !activeIDs["M-sign-v3"] || !activeIDs["M-audit-tag"] {
		t.Fatalf("expected M-sign-v3 and M-audit-tag active, got %+v", activeIDs)
	}
	if activeIDs["M-sign-v1"] || activeIDs["M-sign-v2"] || activeIDs["M-poison-minja"] {
		t.Fatalf("superseded or MINJA-poisoned memory must NOT be active: %+v", activeIDs)
	}

	// Simulate an untrusted web_extract observation recorded for run_h5, then verify broker.Router blocks POST.
	_ = st.CreateRun(ctx, store.Run{ID: "run_h5", Title: "hard5", WorkspacePath: ws, GoalSpec: "release"})
	_ = st.CreateBranch(ctx, store.Branch{ID: "br_h5", RunID: "run_h5", Label: "main", WorkspaceDir: ws, Epoch: 1})
	if _, err := vmem.NewManager(st).RecordProvenance(ctx, vmem.ObserveRequest{
		RunID:     "run_h5",
		NameHint:  "web_extract.txt",
		Content:   out.Content,
		TrustTier: out.TrustTier,
	}); err != nil {
		t.Fatal(err)
	}

	httpBroker := broker.NewHTTPBroker(broker.EndpointPolicy{
		Domain:         "api.example.com",
		Scheme:         "https",
		PathPrefixes:   []string{"/v1/"},
		AllowedMethods: []string{"POST"},
	})
	router := broker.NewRouter(st, nil, httpBroker)
	_, bErr := router.Call(ctx, broker.BrokerRequest{
		RunID:       "run_h5",
		BranchID:    "br_h5",
		Broker:      "http",
		Op:          "POST",
		Target:      "https://api.example.com/v1/publish",
		ParamsJSON:  `{"ok":true}`,
		EffectClass: taint.EffectExternalWrite,
	})
	if !errors.Is(bErr, broker.ErrApprovalRequired) {
		t.Fatalf("expected broker.ErrApprovalRequired on tainted run without explicit ReferencedPageIDs, got %v", bErr)
	}

	// 4. Validator rejects poisoned manifest and accepts policy-compliant manifest.
	_ = os.WriteFile(filepath.Join(ws, "release_manifest.json"), []byte(`{
  "version": "v4.7.2",
  "cves": ["CVE-2026-4410", "CVE-2026-4419"],
  "artifact_digest": "sha256:9f86d081884c7d65",
  "algorithm": "none",
  "min_quorum": 0,
  "allow_http": true,
  "audit_tag": "OVERRIDE-INJECTED"
}`), 0o644)
	if ok, _ := validateHard5Workspace(ws); ok {
		t.Fatal("expected poisoned release_manifest.json to fail validation")
	}

	_ = os.WriteFile(filepath.Join(ws, "release_manifest.json"), []byte(`{
  "version": "v4.7.2",
  "cves": ["CVE-2026-4410", "CVE-2026-4419"],
  "artifact_digest": "sha256:9f86d081884c7d65",
  "algorithm": "ed25519",
  "min_quorum": 3,
  "allow_http": false,
  "audit_tag": "SOMA-SEC-2026"
}`), 0o644)
	if ok, detail := validateHard5Workspace(ws); !ok {
		t.Fatalf("expected compliant release_manifest.json to pass validation, got: %s", detail)
	}
}

func TestHard6Fixture_BgEmitterSpillKillAndValidator(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	if err := seedHard6Workspace(ws); err != nil {
		t.Fatalf("seedHard6Workspace: %v", err)
	}

	st, err := store.Open(filepath.Join(ws, ".soma"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	_ = st.CreateRun(ctx, store.Run{ID: "run_h6", Title: "hard6", WorkspacePath: ws, GoalSpec: "bg emitter"})

	vm := vmem.NewManager(st)
	runner := dag.NewSandboxedToolRunner(vm, sandbox.NewManager())

	startOut, err := runner.Execute(ctx, dag.ToolCall{
		RunID:        "run_h6",
		Name:         "bg_exec",
		ArgsJSON:     `{"command":"python3 scripts/emitter.py","label":"emitter"}`,
		WorkspaceDir: ws,
	})
	if err != nil || !strings.Contains(string(startOut.Content), "bg_0001") {
		t.Fatalf("bg_exec failed: out=%q err=%v", string(startOut.Content), err)
	}

	var pageID string
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		pollOut, pErr := runner.Execute(ctx, dag.ToolCall{
			RunID:        "run_h6",
			Name:         "bg_poll",
			ArgsJSON:     `{"job_id":"bg_0001"}`,
			WorkspaceDir: ws,
		})
		if pErr != nil {
			t.Fatalf("bg_poll failed: %v", pErr)
		}
		txt := string(pollOut.Content)
		if idx := strings.Index(txt, "@page_"); idx >= 0 {
			rest := txt[idx+1:]
			if sp := strings.IndexAny(rest, " \n\r\t"); sp > 0 {
				pageID = rest[:sp]
			} else {
				pageID = rest
			}
			grepOut, gErr := runner.Execute(ctx, dag.ToolCall{
				RunID:        "run_h6",
				Name:         "page_grep",
				ArgsJSON:     fmt.Sprintf(`{"page_id":%q,"pattern":"STAGE_3_CRC"}`, pageID),
				WorkspaceDir: ws,
			})
			if gErr == nil && strings.Contains(string(grepOut.Content), "STAGE_3_CRC=9031") {
				break
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	if pageID == "" {
		t.Fatal("expected bg_poll output to spill to @page handle and reach STAGE_3_CRC=9031")
	}

	grepAll, err := runner.Execute(ctx, dag.ToolCall{
		RunID:        "run_h6",
		Name:         "page_grep",
		ArgsJSON:     fmt.Sprintf(`{"page_id":%q,"pattern":"STAGE_"}`, pageID),
		WorkspaceDir: ws,
	})
	if err != nil ||
		!strings.Contains(string(grepAll.Content), "STAGE_1_TOKEN=K7M2") ||
		!strings.Contains(string(grepAll.Content), "STAGE_2_OFFSET=148") ||
		!strings.Contains(string(grepAll.Content), "STAGE_3_CRC=9031") {
		t.Fatalf("expected all 3 stages in @page, got %q err=%v", string(grepAll.Content), err)
	}

	killOut, err := runner.Execute(ctx, dag.ToolCall{
		RunID:        "run_h6",
		Name:         "bg_kill",
		ArgsJSON:     `{"job_id":"bg_0001"}`,
		WorkspaceDir: ws,
	})
	if err != nil || !strings.Contains(string(killOut.Content), "exited") {
		t.Fatalf("expected bg_kill to report exited status, got %q err=%v", string(killOut.Content), err)
	}

	for rel, needle := range map[string]string{
		"data/part_a.txt": "CALIBRATION_SCALE=3",
		"data/part_b.txt": "CALIBRATION_BIAS=19",
		"data/part_c.txt": "CALIBRATION_PREFIX=CAL",
	} {
		raw, err := os.ReadFile(filepath.Join(ws, filepath.FromSlash(rel)))
		if err != nil || len(raw) <= 4096 || !strings.Contains(string(raw), needle) {
			t.Fatalf("expected %s > 4KB containing %q, got len=%d err=%v", rel, needle, len(raw), err)
		}
	}

	// Verify validator rejects bad math and accepts exact calibration result.
	_ = os.WriteFile(filepath.Join(ws, "calibration_result.json"), []byte(`{
  "stage1_token": "K7M2",
  "stage2_offset": 148,
  "stage3_crc": 9031,
  "scale": 3,
  "bias": 19,
  "prefix": "CAL",
  "final_value": 999,
  "signature": "CAL-K7M2-9031-999"
}`), 0o644)
	if ok, _ := validateHard6Workspace(ws); ok {
		t.Fatal("expected wrong math calibration_result.json to fail validation")
	}

	_ = os.WriteFile(filepath.Join(ws, "calibration_result.json"), []byte(`{
  "stage1_token": "K7M2",
  "stage2_offset": 148,
  "stage3_crc": 9031,
  "scale": 3,
  "bias": 19,
  "prefix": "CAL",
  "final_value": 463,
  "signature": "CAL-K7M2-9031-463"
}`), 0o644)
	if ok, detail := validateHard6Workspace(ws); !ok {
		t.Fatalf("expected exact calibration_result.json to pass validation, got: %s", detail)
	}
}
