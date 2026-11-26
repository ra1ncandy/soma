package arena

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Clean reference solutions. Each hard grader must reject the seeded repo and accept these.
var hardReferences = map[string]map[string]string{
	"bank-concurrency": {
		"bank/ledger.py": `import threading


class Ledger:
    def __init__(self):
        self.count = 0
        self.volume = 0
        self._lock = threading.Lock()

    def record(self, amount):
        with self._lock:
            self.count += 1
            self.volume += amount
`,
		"bank/transfer.py": `from bank.account import InsufficientFunds


def transfer(ledger, src, dst, amount):
    if amount <= 0:
        raise ValueError("amount must be positive")
    if src is dst:
        raise ValueError("cannot transfer to the same account")
    first, second = (src, dst) if src.id < dst.id else (dst, src)
    with first.lock:
        with second.lock:
            if src.balance < amount:
                raise InsufficientFunds("%s has %d, needs %d" % (src.owner, src.balance, amount))
            src.balance -= amount
            dst.balance += amount
    ledger.record(amount)
`,
	},
	"exact-money": {
		"shop/invoice.py": `from fractions import Fraction as F


def _half_up(x):
    return (x * 2 + 1) // 2


def total_cents(lines, tax_pct, discount_pct="0"):
    t = 0
    for price, qty in lines:
        sub = F(price) * 100 * qty
        net = sub - _half_up(sub * F(discount_pct) / 100)
        t += net + _half_up(net * F(tax_pct) / 100)
    return int(t)


def fmt(cents):
    return "%d.%02d" % (cents // 100, cents % 100)


def total(lines, tax_pct, discount_pct="0"):
    return fmt(total_cents(lines, tax_pct, discount_pct))
`,
		"shop/report.py": `from shop.invoice import fmt, total_cents


def daily_total(invoices):
    return fmt(sum(total_cents(i["lines"], i["tax_pct"], i.get("discount_pct", "0")) for i in invoices))
`,
	},
	"kv-crash-recovery": {
		"kvstore/kv.py": `import json
import os


class CorruptLog(Exception):
    pass


class KV:
    def __init__(self, path):
        self.path = path
        os.makedirs(path, exist_ok=True)
        self.log_path = os.path.join(path, "log.jsonl")
        self.snap_path = os.path.join(path, "snapshot.json")
        self.data = {}
        self._recover()
        self.log = open(self.log_path, "a")

    def _recover(self):
        if os.path.exists(self.snap_path):
            with open(self.snap_path) as f:
                self.data = json.load(f)
        if not os.path.exists(self.log_path):
            return
        with open(self.log_path, "rb") as f:
            raw = f.read()
        pos = good = 0
        while pos < len(raw):
            nl = raw.find(b"\n", pos)
            if nl == -1:
                break  # torn final record
            try:
                rec = json.loads(raw[pos:nl])
            except ValueError:
                raise CorruptLog("corrupt record at byte %d" % pos)
            self._apply(rec)
            pos = good = nl + 1
        if good < len(raw):
            with open(self.log_path, "r+b") as f:
                f.truncate(good)

    def _apply(self, rec):
        if rec["op"] == "set":
            self.data[rec["k"]] = rec["v"]
        elif rec["op"] == "del":
            self.data.pop(rec["k"], None)

    def _append(self, rec):
        self.log.write(json.dumps(rec) + "\n")
        self.log.flush()
        self._apply(rec)

    def get(self, k, default=None):
        return self.data.get(k, default)

    def set(self, k, v):
        self._append({"op": "set", "k": k, "v": v})

    def delete(self, k):
        self._append({"op": "del", "k": k})

    def compact(self):
        tmp = self.snap_path + ".tmp"
        with open(tmp, "w") as f:
            json.dump(self.data, f)
            f.flush()
            os.fsync(f.fileno())
        os.replace(tmp, self.snap_path)
        self.log.close()
        open(self.log_path, "w").close()
        self.log = open(self.log_path, "a")

    def close(self):
        self.log.close()
`,
	},
	"config-root-cause": {
		"ROOT_CAUSE.md": "deep_merge recursed with its arguments swapped, so nested keys from the defaults overrode prod.json (reports.timeout_s stayed 1s). Fixed the merge order.\n",
	},
	"semver-ranges": {
		"semver.py": `import re

_ID = r"(?:0|[1-9]\d*|\d*[A-Za-z-][0-9A-Za-z-]*)"
_V = re.compile(r"^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-(" + _ID + r"(?:\." + _ID + r")*))?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$")
_P = re.compile(r"^(0|[1-9]\d*|[xX*])(?:\.(0|[1-9]\d*|[xX*])(?:\.(0|[1-9]\d*|[xX*])(?:-(" + _ID + r"(?:\." + _ID + r")*))?(?:\+[0-9A-Za-z.-]+)?)?)?$")


def parse(v):
    m = _V.match(v.strip()) if isinstance(v, str) else None
    if not m:
        raise ValueError("invalid version %r" % (v,))
    pre = tuple(int(x) if x.isdigit() else x for x in m.group(4).split(".")) if m.group(4) else ()
    return (int(m.group(1)), int(m.group(2)), int(m.group(3)), pre)


def _cmp_pre(a, b):
    if a == b:
        return 0
    if not a:
        return 1
    if not b:
        return -1
    for x, y in zip(a, b):
        if x == y:
            continue
        xi, yi = isinstance(x, int), isinstance(y, int)
        if xi and yi:
            return -1 if x < y else 1
        if xi:
            return -1
        if yi:
            return 1
        return -1 if x < y else 1
    return -1 if len(a) < len(b) else 1


def _cmp(a, b):
    if a[:3] != b[:3]:
        return -1 if a[:3] < b[:3] else 1
    return _cmp_pre(a[3], b[3])


def _v(a=0, b=0, c=0, p=()):
    return (a, b, c, p)


def _partial(s):
    m = _P.match(s)
    if not m:
        raise ValueError("invalid version in range: %r" % s)
    parts = []
    for g in m.group(1, 2, 3):
        if g is None or g in "xX*":
            break
        parts.append(int(g))
    pre = ()
    if m.group(4):
        if len(parts) < 3:
            raise ValueError("prerelease on a partial version: %r" % s)
        pre = tuple(int(x) if x.isdigit() else x for x in m.group(4).split("."))
    return parts, pre


def _comps(tok):
    m = re.match(r"^(<=|>=|<|>|=|\^|~)?(.*)$", tok)
    op, rest = m.group(1) or "", m.group(2)
    if rest == "":
        raise ValueError("invalid comparator %r" % tok)
    parts, pre = _partial(rest)
    n = len(parts)
    if op in ("", "="):
        if n == 3:
            return [("=", _v(*parts, pre))]
        if n == 0:
            return [(">=", _v())]
        if n == 1:
            return [(">=", _v(parts[0])), ("<", _v(parts[0] + 1))]
        return [(">=", _v(parts[0], parts[1])), ("<", _v(parts[0], parts[1] + 1))]
    if op == "~":
        if n == 0:
            return [(">=", _v())]
        if n == 1:
            return [(">=", _v(parts[0])), ("<", _v(parts[0] + 1))]
        return [(">=", _v(parts[0], parts[1], parts[2] if n == 3 else 0, pre)), ("<", _v(parts[0], parts[1] + 1))]
    if op == "^":
        if n == 0:
            return [(">=", _v())]
        f = parts + [0] * (3 - n)
        lo = _v(f[0], f[1], f[2], pre)
        if f[0] > 0 or n == 1:
            hi = _v(f[0] + 1)
        elif f[1] > 0 or n == 2:
            hi = _v(0, f[1] + 1)
        else:
            hi = _v(0, 0, f[2] + 1)
        return [(">=", lo), ("<", hi)]
    if n == 0:
        return [("<", _v())] if op in ("<", ">") else [(">=", _v())]
    if n == 3:
        return [(op, _v(*parts, pre))]
    lo = _v(*(parts + [0] * (3 - n)))
    nxt = _v(parts[0] + 1) if n == 1 else _v(parts[0], parts[1] + 1)
    return {">": [(">=", nxt)], ">=": [(">=", lo)], "<": [("<", lo)], "<=": [("<", nxt)]}[op]


def _hyphen(a, b):
    pa, prea = _partial(a)
    pb, preb = _partial(b)
    cs = [(">=", _v(*(pa + [0] * (3 - len(pa))), prea))]
    if len(pb) == 3:
        cs.append(("<=", _v(*pb, preb)))
    elif len(pb) == 2:
        cs.append(("<", _v(pb[0], pb[1] + 1)))
    elif len(pb) == 1:
        cs.append(("<", _v(pb[0] + 1)))
    return cs


def _test(op, v, c):
    r = _cmp(v, c)
    return {"<": r < 0, "<=": r <= 0, ">": r > 0, ">=": r >= 0, "=": r == 0}[op]


def satisfies(version, rng):
    v = parse(version)
    if not isinstance(rng, str):
        raise ValueError("range must be a string")
    sets = []
    for s in rng.split("||"):
        toks = s.split()
        if len(toks) == 3 and toks[1] == "-":
            cs = _hyphen(toks[0], toks[2])
        else:
            cs = [c for t in toks for c in _comps(t)] or [(">=", _v())]
        sets.append(cs)
    for cs in sets:
        if all(_test(op, v, c) for op, c in cs):
            if not v[3] or any(c[3] and c[:3] == v[:3] for _, c in cs):
                return True
    return False
`,
	},
}

func TestHardGradersRejectSeedAndAcceptReference(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	if len(TasksForTier("hard")) != len(HardTasks) || len(TasksForTier("starter")) != len(Tasks) {
		t.Fatal("tier filter wrong")
	}
	for _, task := range HardTasks {
		task := task
		t.Run(task.ID, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if err := task.Seed(dir); err != nil {
				t.Fatal(err)
			}
			if ok, out := grade(context.Background(), task, dir); ok {
				t.Fatalf("seed passes the grader: %s", out)
			}
			ref, ok := hardReferences[task.ID]
			if !ok {
				t.Fatalf("no reference for %s", task.ID)
			}
			for rel, body := range ref {
				if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(rel)), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if task.ID == "config-root-cause" {
				p := filepath.Join(dir, "config", "loader.py")
				src, _ := os.ReadFile(p)
				fixed := strings.Replace(string(src), "deep_merge(v, out[k])", "deep_merge(out[k], v)", 1)
				if err := os.WriteFile(p, []byte(fixed), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if ok, out := grade(context.Background(), task, dir); !ok {
				t.Fatalf("reference solution fails the grader: %s", out)
			}
		})
	}
}
