package arena

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Task is one arena task. Seed writes the starting project; Grader is a Python 3 program run
// against the final project directory (argv[1]) after the contestant finishes. Graders are
// held out: they are never written into the workspace the agent sees, and both contestants get
// exactly the same Goal text and call budget.
type Task struct {
	ID string
	// Tier groups tasks: "starter" (empty means starter) or "hard".
	Tier     string
	Goal     string
	MaxCalls int
	Files    map[string]string
	Generate func(dir string) error
	Grader   string
	// GradeFunc, when set, grades the workspace instead of the Python Grader script.
	GradeFunc func(ctx context.Context, dir string) (bool, string)
	// Tools is extra tooling each contestant is given for this task.
	Tools *TaskTools
}

// Seed writes the task's starting files into dir.
func (t Task) Seed(dir string) error {
	for rel, body := range t.Files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			return err
		}
	}
	if t.Generate != nil {
		return t.Generate(dir)
	}
	return nil
}

// Tasks is the arena task registry.
var Tasks = []Task{
	{
		ID:       "fix-off-by-one",
		MaxCalls: 20,
		Goal:     "The tests in test_stats.py fail. Fix the bug in stats.py so they pass. Do not modify test_stats.py. You can run the tests with: python3 -m unittest test_stats",
		Files: map[string]string{
			"stats.py": `def moving_average(values, window):
    """Return the list of averages of each consecutive window of values."""
    if window <= 0:
        raise ValueError("window must be positive")
    out = []
    for i in range(len(values) - window):
        chunk = values[i:i + window]
        out.append(sum(chunk) / window)
    return out
`,
			"test_stats.py": `import unittest
from stats import moving_average

class T(unittest.TestCase):
    def test_basic(self):
        self.assertEqual(moving_average([1, 2, 3, 4], 2), [1.5, 2.5, 3.5])

if __name__ == "__main__":
    unittest.main()
`,
		},
		Grader: `import sys, hashlib, os
sys.path.insert(0, sys.argv[1])
os.chdir(sys.argv[1])
from stats import moving_average
assert moving_average([1, 2, 3, 4], 2) == [1.5, 2.5, 3.5]
assert moving_average([5], 1) == [5.0]
assert moving_average([1, 2, 3], 3) == [2.0]
assert moving_average([], 2) == []
try:
    moving_average([1], 0); raise SystemExit("no ValueError")
except ValueError:
    pass
src = open("test_stats.py").read()
assert "[1.5, 2.5, 3.5]" in src and "moving_average([1, 2, 3, 4], 2)" in src, "tests were modified"
print("PASS")
`,
	},
	{
		ID:       "implement-slugify",
		MaxCalls: 20,
		Goal:     "Implement slugify(text) in text_utils.py exactly as its docstring specifies.",
		Files: map[string]string{
			"text_utils.py": `def slugify(text):
    """Convert text to a URL slug.

    Rules:
    - Lowercase ASCII letters and digits are kept; uppercase letters are lowercased.
    - Accented Latin letters are reduced to their base letter (é -> e, Ü -> u, ß -> ss).
    - Every run of other characters (spaces, punctuation, symbols) becomes a single "-".
    - No leading or trailing "-".
    - Empty or all-symbol input returns "".
    """
    raise NotImplementedError
`,
		},
		Grader: `import sys
sys.path.insert(0, sys.argv[1])
from text_utils import slugify
cases = {
    "Hello, World!": "hello-world",
    "  Already-slugged  ": "already-slugged",
    "Crème Brûlée": "creme-brulee",
    "Straße 42": "strasse-42",
    "ÜBER cool!!!": "uber-cool",
    "a  --  b": "a-b",
    "***": "",
    "": "",
    "C++ & Go": "c-go",
}
for inp, want in cases.items():
    got = slugify(inp)
    assert got == want, f"slugify({inp!r}) = {got!r}, want {want!r}"
print("PASS")
`,
	},
	{
		ID:       "log-root-cause",
		MaxCalls: 25,
		Goal:     "service.log is a large application log. Exactly one request failed with a fatal error whose cause is logged several lines before the failure. Find the request id of the failed request and write only that id to answer.txt.",
		Generate: func(dir string) error {
			var b strings.Builder
			for i := 0; i < 60000; i++ {
				fmt.Fprintf(&b, "2026-11-20T10:%02d:%02d INFO req=r%06d handled path=/api/items/%d status=200 ms=%d\n", (i/60)%60, i%60, i, i%977, 3+i%41)
				if i == 41337 {
					fmt.Fprintf(&b, "2026-11-20T10:%02d:%02d WARN req=r%06d pool exhausted, waiting for connection\n", (i/60)%60, i%60, i)
					fmt.Fprintf(&b, "2026-11-20T10:%02d:%02d WARN req=r%06d retrying query (attempt 2)\n", (i/60)%60, i%60, i)
					fmt.Fprintf(&b, "2026-11-20T10:%02d:%02d ERROR req=r%06d FATAL deadlock detected, aborting\n", (i/60)%60, i%60, i)
				}
				if i%9000 == 0 {
					fmt.Fprintf(&b, "2026-11-20T10:%02d:%02d ERROR req=r%06d client disconnected (non-fatal)\n", (i/60)%60, i%60, i)
				}
			}
			return os.WriteFile(filepath.Join(dir, "service.log"), []byte(b.String()), 0o644)
		},
		Grader: `import sys, os
p = os.path.join(sys.argv[1], "answer.txt")
assert os.path.exists(p), "answer.txt missing"
got = open(p).read().strip()
assert got in ("r041337", "req=r041337"), f"wrong answer {got!r}"
print("PASS")
`,
	},
	{
		ID:       "rename-across-files",
		MaxCalls: 25,
		Goal:     "Rename the function calc_total to compute_total everywhere in this project (definition and every call site), keeping behaviour identical. The tests must still pass: python3 -m unittest discover",
		Files: map[string]string{
			"billing/__init__.py": "",
			"billing/core.py": `def calc_total(items, tax=0.0):
    subtotal = sum(price * qty for price, qty in items)
    return round(subtotal * (1 + tax), 2)
`,
			"billing/invoice.py": `from billing.core import calc_total


def invoice_total(order):
    return calc_total(order["items"], order.get("tax", 0.0))
`,
			"reports.py": `from billing import core


def daily_report(orders):
    return sum(core.calc_total(o["items"]) for o in orders)
`,
			"test_billing.py": `import unittest
from billing.invoice import invoice_total
from reports import daily_report

class T(unittest.TestCase):
    def test_invoice(self):
        self.assertEqual(invoice_total({"items": [(10.0, 2)], "tax": 0.1}), 22.0)
    def test_report(self):
        self.assertEqual(daily_report([{"items": [(1.5, 2)]}, {"items": [(2.0, 1)]}]), 5.0)

if __name__ == "__main__":
    unittest.main()
`,
		},
		Grader: `import sys, os, subprocess
root = sys.argv[1]
for dirpath, _, files in os.walk(root):
    if "/." in dirpath or "__pycache__" in dirpath:
        continue
    for f in files:
        if f.endswith(".py"):
            assert "calc_total" not in open(os.path.join(dirpath, f)).read(), f"calc_total still in {f}"
sys.path.insert(0, root)
from billing.core import compute_total
assert compute_total([(10.0, 2)], 0.1) == 22.0
r = subprocess.run([sys.executable, "-m", "unittest", "discover"], cwd=root, capture_output=True, text=True)
assert r.returncode == 0, r.stderr[-2000:]
print("PASS")
`,
	},
}

// TasksForTier returns the tasks of a tier ("starter", "hard" or "all").
func TasksForTier(tier string) []Task {
	var out []Task
	for _, t := range AllTasks() {
		tt := t.Tier
		if tt == "" {
			tt = "starter"
		}
		if tier == "all" || tier == tt {
			out = append(out, t)
		}
	}
	return out
}

// AllTasks is every registered task, starter tier first.
func AllTasks() []Task { return append(append([]Task(nil), Tasks...), HardTasks...) }

// TaskByID looks a task up.
func TaskByID(id string) (Task, bool) {
	for _, t := range AllTasks() {
		if t.ID == id {
			return t, true
		}
	}
	return Task{}, false
}
