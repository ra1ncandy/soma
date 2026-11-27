"""Build the manifest of OSWorld tasks that can be replicated on plain files.

For every task in the chosen domains this downloads the starting files and the gold file (cached),
then keeps the task only if OSWorld's own checker
  - fails on the untouched starting files, and
  - passes once the gold file is put in place of the result file.
Tasks that need the desktop VM (GUI state, shell setup steps, app config) are skipped.

Usage: prepare.py --root <dir>     (expects <dir>/src = OSWorld checkout; run with <dir>/venv python)
Writes <dir>/tasks.json and prints a per-domain summary.
"""
import argparse
import collections
import glob
import importlib.util
import json
import os
import shutil
import subprocess
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
GRADE = os.path.join(HERE, "grade.py")
spec = importlib.util.spec_from_file_location("osw_grade", GRADE)
g = importlib.util.module_from_spec(spec)
spec.loader.exec_module(g)

DOMAINS = ["libreoffice_calc", "libreoffice_writer", "libreoffice_impress", "gimp"]
VM_ONLY_SETUP = {"execute", "command", "googledrive", "login", "chrome_open_tabs", "execute_with_verification"}


def run_grader(src, task_file, workspace, cache):
    r = subprocess.run([sys.executable, GRADE, "--src", src, "--task", task_file, "--workspace", workspace, "--cache", cache],
                       capture_output=True, text=True, timeout=300)
    try:
        return json.loads(r.stdout.strip().splitlines()[-1])
    except Exception:
        return {"score": 0.0, "detail": "grader crashed: " + r.stderr[-200:]}


def consider(root, src, domain, task_file):
    t = json.load(open(task_file))
    cache = os.path.join(root, "cache", domain, t["id"])
    ev = t["evaluator"]
    if ev["func"] == "infeasible":
        raise g.Unsupported("infeasible-task check")
    inputs = []
    for c in t.get("config", []):
        if c["type"] == "download":
            for f in c["parameters"]["files"]:
                inputs.append((f["url"], f["path"]))
        elif c["type"] in VM_ONLY_SETUP:
            raise g.Unsupported("setup step %r needs the VM" % c["type"])
    ws = tempfile.mkdtemp(prefix="osw-prepare-")
    try:
        entries = []
        for url, vm_path in inputs:
            local = g.fetch(url, os.path.join(cache, "in_" + g.cache_name(url)))
            dest = g.vm_to_workspace(vm_path, ws)
            os.makedirs(os.path.dirname(dest), exist_ok=True)
            shutil.copy(local, dest)
            entries.append({"src": os.path.relpath(local, root), "dest": os.path.relpath(dest, ws)})
        seed = run_grader(src, task_file, ws, cache)
        if seed["detail"].startswith("unsupported"):
            raise g.Unsupported(seed["detail"][len("unsupported: "):])
        if seed["score"] >= 1.0:
            return "seed-passes", None
        res, exp = ev.get("result"), ev.get("expected")
        if isinstance(res, list):
            res, exp = res[0], (exp or [None])[0]
        if not (exp and exp.get("type") == "cloud_file" and not exp.get("multi") and not res.get("multi")):
            return "unverified", None  # no single gold file to check the pass side with
        gold = g.fetch(exp["path"], os.path.join(cache, g.cache_name(exp["path"])))
        result_path = g.vm_to_workspace(res["path"], ws)
        os.makedirs(os.path.dirname(result_path), exist_ok=True)
        shutil.copy(gold, result_path)
        if run_grader(src, task_file, ws, cache)["score"] < 1.0:
            return "gold-fails", None
        results = ev["result"] if isinstance(ev["result"], list) else [ev["result"]]
        result_files = sorted({os.path.relpath(g.vm_to_workspace(p, ws), ws) for r in results if r for p in g.multi(r)[0]})
        return "ok", {
            "id": t["id"], "domain": domain, "instruction": t["instruction"],
            "task_file": os.path.relpath(task_file, root), "cache": os.path.relpath(cache, root),
            "inputs": entries, "results": result_files,
        }
    finally:
        shutil.rmtree(ws, ignore_errors=True)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--root", required=True)
    ap.add_argument("--domains", default=",".join(DOMAINS))
    a = ap.parse_args()
    root = os.path.abspath(a.root)
    src = os.path.join(root, "src")
    manifest, counts = [], collections.Counter()
    for domain in a.domains.split(","):
        for task_file in sorted(glob.glob(os.path.join(src, "evaluation_examples", "examples", domain, "*.json"))):
            try:
                status, entry = consider(root, src, domain, task_file)
            except g.Unsupported as e:
                status, entry = "needs-vm", None
                detail = str(e)
            except Exception as e:
                status, entry = "error", None
                detail = "%s: %s" % (type(e).__name__, str(e)[:120])
            else:
                detail = ""
            counts[(domain, status)] += 1
            if entry:
                manifest.append(entry)
            print(domain, os.path.basename(task_file)[:8], status, detail, flush=True)
    with open(os.path.join(root, "tasks.json"), "w") as f:
        json.dump(manifest, f, indent=1)
    print("kept %d tasks" % len(manifest))
    for (domain, status), n in sorted(counts.items()):
        print("  %-20s %-12s %d" % (domain, status, n))


if __name__ == "__main__":
    main()
