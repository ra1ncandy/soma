package arena

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// HardTasks is the "hard" tier: multi-file, multi-step tasks whose held-out graders probe
// invariants and adversarial edge cases the visible tests do not cover.
var HardTasks = []Task{hardConcurrency, hardMoney, hardKVRecovery, hardConfigRootCause, hardSemver}

// ---------------------------------------------------------------------------------------------
// 1. Concurrency: deadlock + lost updates across three modules.

var hardConcurrency = Task{
	ID:       "bank-concurrency",
	Tier:     "hard",
	MaxCalls: 40,
	Goal: "The bank package works in single-threaded tests (python3 -m unittest test_bank) but under concurrent load " +
		"the service sometimes hangs forever and the ledger totals drift from the real transfers. Make bank.transfer " +
		"safe for any number of threads transferring between any accounts in any direction: no deadlocks, no lost " +
		"updates, money conserved, the ledger's count and volume exact. Keep the public API and behaviour: " +
		"amount must be positive (ValueError), a transfer from an account to itself raises ValueError, and a transfer " +
		"the source cannot cover raises InsufficientFunds and changes nothing.",
	Files: map[string]string{
		"bank/__init__.py": "from bank.account import Account, InsufficientFunds\nfrom bank.ledger import Ledger\nfrom bank.transfer import transfer\n",
		"bank/account.py": `import threading


class InsufficientFunds(Exception):
    pass


class Account:
    _next = 0

    def __init__(self, owner, balance=0):
        Account._next += 1
        self.id = Account._next
        self.owner = owner
        self.balance = balance
        self.lock = threading.Lock()
`,
		"bank/ledger.py": `import time


class Ledger:
    """Counts successful transfers and the total amount moved."""

    def __init__(self):
        self.count = 0
        self.volume = 0

    def record(self, amount):
        n = self.count
        time.sleep(0)
        self.count = n + 1
        v = self.volume
        time.sleep(0)
        self.volume = v + amount
`,
		"bank/transfer.py": `import time

from bank.account import InsufficientFunds


def transfer(ledger, src, dst, amount):
    if amount <= 0:
        raise ValueError("amount must be positive")
    with src.lock:
        time.sleep(0)
        with dst.lock:
            if src.balance < amount:
                raise InsufficientFunds("%s has %d, needs %d" % (src.owner, src.balance, amount))
            src.balance -= amount
            dst.balance += amount
    ledger.record(amount)
`,
		"test_bank.py": `import unittest
from bank import Account, Ledger, transfer, InsufficientFunds


class T(unittest.TestCase):
    def test_simple(self):
        a, b, l = Account("a", 100), Account("b", 0), Ledger()
        transfer(l, a, b, 30)
        self.assertEqual((a.balance, b.balance, l.count, l.volume), (70, 30, 1, 30))

    def test_insufficient(self):
        a, b, l = Account("a", 10), Account("b", 0), Ledger()
        with self.assertRaises(InsufficientFunds):
            transfer(l, a, b, 11)
        self.assertEqual((a.balance, b.balance, l.count), (10, 0, 0))


if __name__ == "__main__":
    unittest.main()
`,
	},
	Grader: `import subprocess, sys, textwrap
root = sys.argv[1]
stress = textwrap.dedent("""
    import random, sys, threading
    sys.path.insert(0, %r)
    from bank import Account, Ledger, transfer, InsufficientFunds
    accounts = [Account("acct%%d" %% i, 1000) for i in range(6)]
    ledger = Ledger()
    ok = [0] * 24
    moved = [0] * 24
    def worker(i):
        rnd = random.Random(i)
        for _ in range(400):
            a, b = rnd.sample(accounts, 2)
            amt = rnd.randint(1, 400)
            try:
                transfer(ledger, a, b, amt)
                ok[i] += 1
                moved[i] += amt
            except InsufficientFunds:
                pass
    ts = [threading.Thread(target=worker, args=(i,)) for i in range(24)]
    for t in ts: t.start()
    for t in ts: t.join()
    total = sum(a.balance for a in accounts)
    assert total == 6000, "money not conserved: %%d" %% total
    assert all(a.balance >= 0 for a in accounts), "negative balance"
    assert ledger.count == sum(ok), "ledger count %%d != %%d" %% (ledger.count, sum(ok))
    assert ledger.volume == sum(moved), "ledger volume %%d != %%d" %% (ledger.volume, sum(moved))
    a = accounts[0]
    try:
        transfer(ledger, a, a, 1); raise SystemExit("self-transfer accepted")
    except ValueError:
        pass
    try:
        transfer(ledger, a, accounts[1], 0); raise SystemExit("zero amount accepted")
    except ValueError:
        pass
    before = (accounts[2].balance, accounts[3].balance, ledger.count)
    try:
        transfer(ledger, accounts[2], accounts[3], 10**9); raise SystemExit("overdraft accepted")
    except InsufficientFunds:
        pass
    assert before == (accounts[2].balance, accounts[3].balance, ledger.count), "failed transfer changed state"
    print("STRESS-OK")
""" % root)
for attempt in range(3):
    try:
        r = subprocess.run([sys.executable, "-c", stress], capture_output=True, text=True, timeout=20)
    except subprocess.TimeoutExpired:
        raise SystemExit("deadlock: stress test did not finish in 20s")
    assert r.returncode == 0 and "STRESS-OK" in r.stdout, (r.stdout + r.stderr)[-1500:]
r = subprocess.run([sys.executable, "-m", "unittest", "test_bank"], cwd=root, capture_output=True, text=True, timeout=60)
assert r.returncode == 0, r.stderr[-1500:]
print("PASS")
`,
}

// ---------------------------------------------------------------------------------------------
// 2. Multi-module money refactor to exact integer cents with a precise rounding spec.

var hardMoney = Task{
	ID:       "exact-money",
	Tier:     "hard",
	MaxCalls: 40,
	Goal: "Invoice totals produced by the shop package are sometimes off by a cent. SPEC.md defines exactly how " +
		"totals must be computed. Make shop.invoice.total and shop.report.daily_total follow SPEC.md exactly for all " +
		"inputs, keeping their signatures. Hidden tests check many edge cases.",
	Files: map[string]string{
		"SPEC.md": `# Money rules

All amounts are non-negative decimal strings with at most two decimals ("19.99", "0.5", "3").
Percentages (discount_pct, tax_pct) are decimal strings with at most three decimals ("8.875", "0", "100").

For each invoice line (unit_price, qty):
1. subtotal = unit_price * qty, exactly, in cents.
2. discount = subtotal * discount_pct / 100, rounded to a whole cent, half up (x.5 cents rounds up).
3. net = subtotal - discount.
4. tax = net * tax_pct / 100, rounded to a whole cent, half up.
5. line_total = net + tax.

invoice total = sum of line_totals, formatted as a string with exactly two decimals ("1234.50").
daily_total(invoices) = the sum of the totals of each invoice (each computed as above), same format.
Rounding happens per line, never on the sum.
`,
		"shop/__init__.py": "",
		"shop/money.py": `def parse(s):
    return float(s)


def fmt(x):
    return "%.2f" % x
`,
		"shop/discount.py": `def apply(amount, pct):
    return amount * (1 - pct / 100.0)
`,
		"shop/tax.py": `def line_tax(amount, pct):
    return amount * pct / 100.0
`,
		"shop/invoice.py": `from shop import money, discount, tax


def total(lines, tax_pct, discount_pct="0"):
    """lines: list of (unit_price_str, qty_int). Returns the total as a 'D.CC' string."""
    t = 0.0
    for price, qty in lines:
        sub = money.parse(price) * qty
        net = discount.apply(sub, money.parse(discount_pct))
        t += net + tax.line_tax(net, money.parse(tax_pct))
    return money.fmt(t)
`,
		"shop/report.py": `from shop import money
from shop.invoice import total


def daily_total(invoices):
    """invoices: list of dicts with lines, tax_pct, discount_pct."""
    s = 0.0
    for inv in invoices:
        s += money.parse(total(inv["lines"], inv["tax_pct"], inv.get("discount_pct", "0")))
    return money.fmt(s)
`,
		"test_shop.py": `import unittest
from shop.invoice import total


class T(unittest.TestCase):
    def test_basic(self):
        self.assertEqual(total([("10.00", 2)], "10"), "22.00")


if __name__ == "__main__":
    unittest.main()
`,
	},
	Grader: `import random, sys
from fractions import Fraction as F
sys.path.insert(0, sys.argv[1])
from shop.invoice import total
from shop.report import daily_total

def hu(x):  # round half up to an integer number of cents
    return (x * 2 + 1) // 2

def ref_total(lines, tax_pct, disc):
    t = 0
    for price, qty in lines:
        sub = F(price) * 100 * qty
        d = hu(sub * F(disc) / 100)
        net = sub - d
        tx = hu(net * F(tax_pct) / 100)
        t += net + tx
    return "%d.%02d" % (int(t) // 100, int(t) % 100)

cases = [
    ([("0.01", 1)], "50", "0"),
    ([("0.01", 1)], "49.999", "0"),
    ([("0.05", 1)], "10", "0"),
    ([("1.15", 3)], "0", "50"),
    ([("19.99", 3), ("0.07", 7)], "8.875", "12.5"),
    ([("100", 1)], "0", "100"),
    ([("0.10", 0)], "7", "0"),
    ([("2.67", 5)], "5", "0.005"),
    ([("999999.99", 1000)], "7.25", "33.333"),
    ([("0.35", 1), ("0.35", 1)], "10", "0"),
    ([("1.05", 1)], "50", "0"),
    ([("4.35", 100)], "0.5", "0"),
]
rnd = random.Random(7)
for _ in range(600):
    lines = [("%d.%02d" % (rnd.randint(0, 999), rnd.randint(0, 99)), rnd.randint(0, 50)) for _ in range(rnd.randint(1, 6))]
    tax_pct = "%d.%03d" % (rnd.randint(0, 25), rnd.randint(0, 999))
    disc = rnd.choice(["0", "5", "12.5", "33.333", "%d.%d" % (rnd.randint(0, 60), rnd.randint(0, 9))])
    cases.append((lines, tax_pct, disc))
for lines, tax_pct, disc in cases:
    want = ref_total(lines, tax_pct, disc)
    got = total(lines, tax_pct, disc)
    assert got == want, "total(%r, %r, %r) = %r, want %r" % (lines, tax_pct, disc, got, want)
invs = [{"lines": c[0], "tax_pct": c[1], "discount_pct": c[2]} for c in cases[:40]]
want = sum(F(ref_total(i["lines"], i["tax_pct"], i["discount_pct"])) for i in invs)
want_s = "%d.%02d" % (int(want * 100) // 100, int(want * 100) % 100)
got = daily_total(invs)
assert got == want_s, "daily_total = %r, want %r" % (got, want_s)
print("PASS")
`,
}

// ---------------------------------------------------------------------------------------------
// 3. Crash recovery of a write-ahead-logged key-value store.

var hardKVRecovery = Task{
	ID:       "kv-crash-recovery",
	Tier:     "hard",
	MaxCalls: 40,
	Goal: "kvstore/kv.py is a small key-value store with a write-ahead log (log.jsonl) and compaction into a snapshot " +
		"(snapshot.json). Users report that after a power loss the store refuses to open, and that a crash during " +
		"compact() can lose data. Make the store crash-safe with these guarantees: (1) a crash at any byte while a " +
		"record is being appended leaves the store openable with exactly the records that were fully written; " +
		"(2) writes made after such a recovery are durable and readable after reopening; (3) a crash at any point " +
		"during compact() never loses or corrupts data; (4) corruption anywhere other than a torn final record must " +
		"raise kvstore.kv.CorruptLog instead of being silently skipped. Keep the KV API (get/set/delete/compact/close).",
	Files: map[string]string{
		"kvstore/__init__.py": "",
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
        if os.path.exists(self.log_path):
            with open(self.log_path) as f:
                for line in f:
                    self._apply(json.loads(line))

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
        with open(self.snap_path, "w") as f:
            json.dump(self.data, f)
        self.log.close()
        open(self.log_path, "w").close()
        self.log = open(self.log_path, "a")

    def close(self):
        self.log.close()
`,
		"test_kv.py": `import tempfile, unittest
from kvstore.kv import KV


class T(unittest.TestCase):
    def test_roundtrip(self):
        d = tempfile.mkdtemp()
        kv = KV(d)
        kv.set("a", 1)
        kv.set("b", 2)
        kv.delete("a")
        kv.compact()
        kv.set("c", 3)
        kv.close()
        kv = KV(d)
        self.assertEqual((kv.get("a"), kv.get("b"), kv.get("c")), (None, 2, 3))


if __name__ == "__main__":
    unittest.main()
`,
	},
	Grader: `import builtins, os, shutil, sys, tempfile
sys.path.insert(0, sys.argv[1])
import kvstore.kv as kvmod
from kvstore.kv import KV, CorruptLog

def build(d):
    kv = KV(d)
    expect = {}
    for i in range(30):
        kv.set("k%d" % i, {"n": i, "s": "v" * (i % 7)})
        expect["k%d" % i] = {"n": i, "s": "v" * (i % 7)}
        if i % 4 == 3:
            kv.delete("k%d" % (i - 2))
            expect.pop("k%d" % (i - 2), None)
    kv.close()
    return expect

KEYS = ["k%d" % i for i in range(30)] + ["after"]

def snapshot(kv):
    return {k: kv.get(k) for k in KEYS if kv.get(k) is not None}

def state(d):
    kv = KV(d)
    out = snapshot(kv)
    kv.close()
    return out

base = tempfile.mkdtemp()
src = os.path.join(base, "src")
expect = build(src)
assert state(src) == expect, "store does not round-trip"

# (1)+(2): torn final record at every byte offset of the last record.
logp = os.path.join(src, "log.jsonl")
raw = open(logp, "rb").read()
last_start = raw.rstrip(b"\n").rfind(b"\n") + 1
for cut in range(last_start + 1, len(raw)):
    d = os.path.join(base, "torn%d" % cut)
    shutil.copytree(src, d)
    with open(os.path.join(d, "log.jsonl"), "r+b") as f:
        f.truncate(cut)
    kv = KV(d)
    st = snapshot(kv)
    kv.set("after", cut)
    kv.close()
    kv = KV(d)
    assert kv.get("after") == cut, "write after torn-record recovery lost (cut %d)" % cut
    full = {k: v for k, v in expect.items() if k != "k29"}
    assert {k: v for k, v in st.items() if k != "k29"} == full, "records lost or wrong after torn recovery (cut %d)" % cut
    assert st.get("k29") in (None, expect["k29"]), "torn record applied with a wrong value"
    kv.close()

# (4): corruption in the middle must raise CorruptLog.
d = os.path.join(base, "mid")
shutil.copytree(src, d)
lines = open(os.path.join(d, "log.jsonl"), "rb").read().split(b"\n")
lines[5] = b'{"op": "set", "k": "broken'
open(os.path.join(d, "log.jsonl"), "wb").write(b"\n".join(lines))
try:
    KV(d)
    raise SystemExit("mid-log corruption was silently accepted")
except CorruptLog:
    pass

# (3): crash at every write point during compact().
real_open = builtins.open
class Crash(Exception):
    pass
for budget in range(0, 4000, 37):
    d = os.path.join(base, "cmp%d" % budget)
    shutil.copytree(src, d)
    kv = KV(d)
    written = [0]
    def fake_open(file, mode="r", *a, **kw):
        f = real_open(file, mode, *a, **kw)
        if any(c in mode for c in "wa+x") and str(file).startswith(d):
            w = f.write
            def write(s, w=w):
                written[0] += len(s)
                if written[0] > budget:
                    raise Crash()
                return w(s)
            try:
                f.write = write
            except AttributeError:
                class Wrap:
                    def __init__(s2, inner): s2.inner = inner
                    def write(s2, s): return write(s)
                    def __getattr__(s2, n): return getattr(s2.inner, n)
                    def __enter__(s2): return s2
                    def __exit__(s2, *e): s2.inner.close()
                return Wrap(f)
        return f
    builtins.open = fake_open
    crashed = False
    try:
        kv.compact()
    except Crash:
        crashed = True
    finally:
        builtins.open = real_open
    try:
        kv.close()
    except Exception:
        pass
    got = state(d)
    assert got == expect, "data lost or corrupted after a crash during compact (budget %d, crashed=%s)" % (budget, crashed)
print("PASS")
`,
}

// ---------------------------------------------------------------------------------------------
// 4. Large noisy codebase: production-only timeout caused by a config deep-merge bug.

var hardConfigRootCause = Task{
	ID:       "config-root-cause",
	Tier:     "hard",
	MaxCalls: 40,
	Goal: "In production (APP_ENV=prod) requests to /reports time out, while they work in staging and dev. The " +
		"service code is under svc/, configuration under config/, and recent production logs under logs/. Find the " +
		"root cause, fix it properly (other configuration behaviour must stay correct for every environment), and " +
		"write a short explanation of the root cause to ROOT_CAUSE.md.",
	Files: map[string]string{
		"config/loader.py": `import json
import os

HERE = os.path.dirname(os.path.abspath(__file__))


def deep_merge(base, override):
    """Return base updated with override; values in override win, nested dicts merge."""
    out = dict(base)
    for k, v in override.items():
        if isinstance(v, dict) and isinstance(out.get(k), dict):
            out[k] = deep_merge(v, out[k])
        else:
            out[k] = v
    return out


def load(env=None):
    env = env or os.environ.get("APP_ENV", "dev")
    with open(os.path.join(HERE, "default.json")) as f:
        cfg = json.load(f)
    p = os.path.join(HERE, env + ".json")
    if os.path.exists(p):
        with open(p) as f:
            cfg = deep_merge(cfg, json.load(f))
    return cfg
`,
		"config/__init__.py": "",
		"config/default.json": `{"service": {"name": "acme", "port": 8080},
 "reports": {"timeout_s": 1, "page_size": 50, "backend": {"host": "localhost", "pool": {"size": 2, "recycle_s": 300}}},
 "search": {"timeout_s": 5, "backend": {"host": "localhost"}},
 "features": {"beta": false}}
`,
		"config/staging.json": `{"service": {"port": 9090}, "features": {"beta": true}}
`,
		"config/prod.json": `{"service": {"port": 80},
 "reports": {"timeout_s": 30, "backend": {"host": "reports.internal", "pool": {"size": 16}}},
 "search": {"backend": {"host": "search.internal"}}}
`,
		"svc/__init__.py":         "",
		"svc/reports/__init__.py": "",
		"svc/reports/client.py": `from config.loader import load


def fetch_report(report_id, transport):
    cfg = load()["reports"]
    return transport.get("/reports/%s" % report_id, host=cfg["backend"]["host"],
                         timeout=cfg.get("timeout_s", 1), pool=cfg["backend"]["pool"]["size"])
`,
		"svc/net/__init__.py": "",
		"svc/net/retry.py": `DEFAULT_TIMEOUT = 1  # seconds; used only by health checks


def with_retry(fn, attempts=3):
    last = None
    for _ in range(attempts):
        try:
            return fn()
        except TimeoutError as e:
            last = e
    raise last
`,
	},
	Generate: func(dir string) error {
		// Noise: many plausible modules and a large production log.
		for i := 0; i < 36; i++ {
			name := []string{"billing", "users", "audit", "cache", "metrics", "queue"}[i%6]
			p := filepath.Join(dir, "svc", fmt.Sprintf("%s%d", name, i/6), "handlers.py")
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			body := fmt.Sprintf("from config.loader import load\n\n\ndef handle_%s_%d(req):\n    cfg = load()\n    timeout = cfg.get(%q, {}).get(\"timeout_s\", 5)\n    return {\"ok\": True, \"timeout\": timeout, \"n\": %d}\n", name, i, name, i)
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(filepath.Dir(p), "__init__.py"), nil, 0o644); err != nil {
				return err
			}
		}
		var b strings.Builder
		for i := 0; i < 40000; i++ {
			switch {
			case i%97 == 0:
				fmt.Fprintf(&b, "2026-11-20T09:%02d:%02d WARN svc.reports.client report=%d host=reports.internal pool=2 timeout=1s exceeded (backend p50=4.2s)\n", (i/60)%60, i%60, i)
			case i%13 == 0:
				fmt.Fprintf(&b, "2026-11-20T09:%02d:%02d WARN svc.net.retry health check slow (DEFAULT_TIMEOUT=1s) target=cache%d\n", (i/60)%60, i%60, i%5)
			default:
				fmt.Fprintf(&b, "2026-11-20T09:%02d:%02d INFO svc.%s%d handled req=%d status=200\n", (i/60)%60, i%60, []string{"billing", "users", "audit"}[i%3], i%6, i)
			}
		}
		if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, "logs", "prod.log"), []byte(b.String()), 0o644)
	},
	Grader: `import json, os, random, sys
root = sys.argv[1]
sys.path.insert(0, root)
from config.loader import deep_merge, load

def ref(base, over):
    out = dict(base)
    for k, v in over.items():
        if isinstance(v, dict) and isinstance(out.get(k), dict):
            out[k] = ref(out[k], v)
        else:
            out[k] = v
    return out

prod = load("prod")
assert prod["reports"]["timeout_s"] == 30, "prod reports timeout is %r" % prod["reports"]["timeout_s"]
assert prod["reports"]["page_size"] == 50
assert prod["reports"]["backend"]["host"] == "reports.internal"
assert prod["reports"]["backend"]["pool"] == {"size": 16, "recycle_s": 300}, prod["reports"]["backend"]["pool"]
assert prod["search"] == {"timeout_s": 5, "backend": {"host": "search.internal"}}
assert prod["service"] == {"name": "acme", "port": 80}
st = load("staging")
assert st["features"]["beta"] is True and st["service"]["port"] == 9090 and st["reports"]["timeout_s"] == 1
dev = load("dev")
assert dev["reports"]["timeout_s"] == 1 and dev["features"]["beta"] is False

rnd = random.Random(3)
def rand_tree(depth):
    d = {}
    for i in range(rnd.randint(1, 4)):
        k = "k%d" % rnd.randint(0, 5)
        d[k] = rand_tree(depth - 1) if depth > 0 and rnd.random() < 0.5 else rnd.randint(0, 9)
    return d
for _ in range(400):
    a, b = rand_tree(4), rand_tree(4)
    a0 = json.dumps(a, sort_keys=True)
    assert deep_merge(a, b) == ref(a, b), "deep_merge disagrees with its documented semantics"
    assert json.dumps(a, sort_keys=True) == a0, "deep_merge mutated its input"
rc = os.path.join(root, "ROOT_CAUSE.md")
assert os.path.exists(rc), "ROOT_CAUSE.md missing"
text = open(rc).read().lower()
assert "merge" in text, "ROOT_CAUSE.md does not mention the merge"
print("PASS")
`,
}

// ---------------------------------------------------------------------------------------------
// 5. Spec compliance with adversarial held-out cases: semver range matching.

var hardSemver = Task{
	ID:       "semver-ranges",
	Tier:     "hard",
	MaxCalls: 40,
	Goal: "semver.py implements satisfies(version, range) but only partially follows SPEC.md. Make it follow " +
		"SPEC.md exactly. The visible tests are a small sample; hidden tests cover every rule, including " +
		"prerelease handling and invalid input.",
	Files: map[string]string{
		"SPEC.md": `# Version ranges

A version is MAJOR.MINOR.PATCH with an optional -PRERELEASE (dot-separated identifiers of [0-9A-Za-z-]; numeric parts and numeric
prerelease identifiers have no leading zeros)
and optional +BUILD metadata, which is ignored. satisfies() raises ValueError for an invalid version
(e.g. "1.2") or an invalid range.

Precedence: compare MAJOR, MINOR, PATCH numerically. A version with a prerelease is lower than the same
version without one. Prerelease identifiers compare left to right: numeric identifiers numerically,
alphanumeric ones lexically (ASCII), numeric lower than alphanumeric, and a shorter list is lower when
all preceding identifiers are equal (1.0.0-alpha < 1.0.0-alpha.1 < 1.0.0-alpha.beta < 1.0.0-beta < 1.0.0-beta.2 < 1.0.0-beta.11 < 1.0.0-rc.1 < 1.0.0).

A range is one or more comparator sets joined by "||"; it matches if any set matches. A set is
whitespace-separated comparators that must all match. Comparators:
- Primitive: <, <=, >, >=, = or no operator, followed by a version (no space after the operator).
- Partial versions and X-ranges: "1.2" = "1.2.x" = "1.2.*" means >=1.2.0 <1.3.0; "1" = "1.x" means
  >=1.0.0 <2.0.0; "*", "x" and the empty range mean >=0.0.0.
  With an operator: ">1.2" means >=1.3.0, ">1" means >=2.0.0, ">=1.2" means >=1.2.0,
  "<1.2" means <1.2.0, "<=1.2" means <1.3.0, "<=1" means <2.0.0.
- Tilde: ~1.2.3 := >=1.2.3 <1.3.0; ~1.2 := >=1.2.0 <1.3.0; ~1 := >=1.0.0 <2.0.0;
  ~1.2.3-beta.2 := >=1.2.3-beta.2 <1.3.0.
- Caret (do not change the left-most non-zero part): ^1.2.3 := >=1.2.3 <2.0.0; ^0.2.3 := >=0.2.3 <0.3.0;
  ^0.0.3 := >=0.0.3 <0.0.4; ^1.2 := >=1.2.0 <2.0.0; ^0.2 := >=0.2.0 <0.3.0; ^0.0 := >=0.0.0 <0.1.0;
  ^1 := >=1.0.0 <2.0.0; ^0 := >=0.0.0 <1.0.0; ^1.2.3-beta.2 := >=1.2.3-beta.2 <2.0.0.
- Hyphen: "A - B" := >=A <=B, where a partial A is filled with zeros and a partial B is an exclusive
  upper bound on the next version ("1.2.3 - 2.3" := >=1.2.3 <2.4.0; "1.2 - 2" := >=1.2.0 <3.0.0).

Prereleases: a version with a prerelease only matches a comparator set if at least one comparator in that
set is on the same MAJOR.MINOR.PATCH and itself has a prerelease (so "*" and "<1.0.0" never match
1.0.0-rc.1, but ">=1.2.3-alpha.1" matches 1.2.3-alpha.3 and not 1.2.4-alpha.1).
`,
		"semver.py": `import re

_V = re.compile(r"^(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?(?:\+[0-9A-Za-z.-]+)?$")


def parse(v):
    m = _V.match(v.strip())
    if not m:
        raise ValueError("invalid version %r" % v)
    return (int(m.group(1)), int(m.group(2)), int(m.group(3)), m.group(4) or "")


def _cmp(a, b):
    return (a > b) - (a < b)


def _test(op, v, c):
    r = _cmp(v, c)
    return {"<": r < 0, "<=": r <= 0, ">": r > 0, ">=": r >= 0, "=": r == 0}[op]


def satisfies(version, rng):
    v = parse(version)
    for comp in rng.split():
        m = re.match(r"^(<=|>=|<|>|=|\^|~)?(.*)$", comp)
        op, rest = m.group(1) or "=", m.group(2)
        if rest.endswith(".x"):
            major = int(rest[:-2])
            if not (major, 0, 0, "") <= v < (major + 1, 0, 0, ""):
                return False
            continue
        c = parse(rest)
        if op == "^":
            if not (c <= v < (c[0] + 1, 0, 0, "")):
                return False
        elif op == "~":
            if not (c <= v < (c[0], c[1] + 1, 0, "")):
                return False
        elif not _test(op, v, c):
            return False
    return True
`,
		"test_semver.py": `import unittest
from semver import satisfies


class T(unittest.TestCase):
    def test_sample(self):
        self.assertTrue(satisfies("1.2.3", ">=1.2.0 <1.3.0"))
        self.assertTrue(satisfies("1.9.0", "^1.2.3"))
        self.assertFalse(satisfies("2.0.0", "^1.2.3"))
        self.assertTrue(satisfies("1.2.9", "~1.2.3"))


if __name__ == "__main__":
    unittest.main()
`,
	},
	Grader: `import sys
sys.path.insert(0, sys.argv[1])
from semver import satisfies
T, F = True, False
cases = [
 ("1.2.3","1.2.3",T),("1.2.4","1.2.3",F),("1.2.3","=1.2.3",T),("1.2.3",">1.2.2",T),("1.2.3",">1.2.3",F),
 ("1.2.3","<=1.2.3",T),("1.2.3","<1.2.3",F),
 ("1.9.9","1.x",T),("2.0.0","1.x",F),("1.2.9","1.2.*",T),("1.3.0","1.2.*",F),("1.2.9","1.2.x",T),
 ("0.0.1","*",T),("3.4.5","",T),("3.4.5","x",T),
 ("1.2.9","1.2",T),("1.3.0","1.2",F),("1.3.0","1",T),("2.0.0","1",F),
 ("1.2.9","~1.2.3",T),("1.3.0","~1.2.3",F),("1.2.2","~1.2.3",F),("1.2.0","~1.2",T),("1.3.0","~1.2",F),
 ("1.9.0","~1",T),("2.0.0","~1",F),
 ("1.9.9","^1.2.3",T),("2.0.0","^1.2.3",F),("1.2.2","^1.2.3",F),("0.2.9","^0.2.3",T),("0.3.0","^0.2.3",F),
 ("0.2.2","^0.2.3",F),("0.0.3","^0.0.3",T),("0.0.4","^0.0.3",F),("0.1.5","^0.1",T),("0.2.0","^0.1",F),
 ("0.0.9","^0.0",T),("0.1.0","^0.0",F),("0.9.9","^0",T),("1.0.0","^0",F),("1.5.0","^1.2",T),("1.1.9","^1.2",F),
 ("1.9.9","^1",T),("2.0.0","^1",F),
 ("1.2.3","1.2.3 - 2.3.4",T),("2.3.4","1.2.3 - 2.3.4",T),("2.3.5","1.2.3 - 2.3.4",F),("1.2.2","1.2.3 - 2.3.4",F),
 ("2.3.9","1.2.3 - 2.3",T),("2.4.0","1.2.3 - 2.3",F),("2.9.0","1.2 - 2",T),("3.0.0","1.2 - 2",F),
 ("1.1.9","1.2 - 2",F),("1.2.0","1.2 - 2",T),
 ("1.3.0",">1.2",T),("1.2.9",">1.2",F),("2.0.0",">1",T),("1.9.9",">1",F),("1.2.9","<=1.2",T),
 ("1.3.0","<=1.2",F),("1.9.9","<=1",T),("2.0.0","<=1",F),("1.1.9","<1.2",T),("1.2.0","<1.2",F),
 ("1.2.0",">=1.2",T),("1.1.9",">=1.2",F),
 ("1.2.3",">=1.2.0 <1.3.0",T),("1.3.0",">=1.2.0 <1.3.0",F),
 ("1.5.0","<1.2.0 || >=1.4.0",T),("1.3.0","<1.2.0 || >=1.4.0",F),("1.0.0","<1.2.0 || >=1.4.0",T),
 ("2.1.0","^1.2.3 || ^2.0.0",T),("3.0.0","^1.2.3 || ^2.0.0",F),("1.2.4","1.2.3 - 1.2.5 || 2.x",T),
 ("1.2.3-alpha.3",">=1.2.3-alpha.1",T),("1.2.3-alpha.1",">=1.2.3-alpha.3",F),("1.2.4-alpha.1",">=1.2.3-alpha.1",F),
 ("1.2.3-beta",">1.2.3-alpha.1",T),("1.2.3","^1.2.3-beta.2",T),("1.2.3-beta.4","^1.2.3-beta.2",T),
 ("1.2.4-beta.4","^1.2.3-beta.2",F),("1.2.3-alpha","1.2.3",F),("1.2.3-alpha","*",F),("2.0.0-rc.1","^1.0.0",F),
 ("1.0.0-rc.1","<1.0.0",F),("1.0.0-alpha.10",">1.0.0-alpha.9",T),("1.0.0-alpha.beta",">1.0.0-alpha.1",T),
 ("1.0.0-alpha","<1.0.0-alpha.1",T),("1.0.0-rc.1",">=1.0.0-beta.11",T),("1.0.0-beta.2","<1.0.0-beta.11",T),
 ("1.0.0-beta.11","<1.0.0-beta.2",F),("1.0.0-alpha.1","<1.0.0-alpha",F),
 ("1.2.3+build.5","1.2.3",T),("1.2.3","~1.2.3-beta.2",T),("1.2.3-beta.3","~1.2.3-beta.2",T),
 ("1.2.3-beta.1","~1.2.3-beta.2",F),("1.2.3-beta.3","1.2.3-beta.2 - 1.2.3",T),
]
for v, r, want in cases:
    got = satisfies(v, r)
    assert got is want, "satisfies(%r, %r) = %r, want %r" % (v, r, got, want)
for v, r in [("1.2", "1.x"), ("1.2.3.4", "*"), ("01.2.3", "*"), ("1.2.3", ">>1"), ("1.2.3", "~~1"), ("1.2.3", "1.2.3 -"), ("1.2.3", "abc")]:
    try:
        satisfies(v, r)
    except ValueError:
        continue
    raise SystemExit("satisfies(%r, %r) accepted invalid input" % (v, r))
print("PASS")
`,
}
