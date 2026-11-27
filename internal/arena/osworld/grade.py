"""Grade one OSWorld-derived task on plain files, with OSWorld's own checker functions.

OSWorld runs each task inside a desktop VM. This grader replicates only the part that does not
need the VM: it takes the task definition (JSON), the files an agent left in a workspace
directory, and calls the same metric functions OSWorld uses. Paths under /home/user in the task
map to the workspace root.

Usage: grade.py --src <OSWorld checkout> --task <task.json> --workspace <dir> --cache <dir>
Prints one JSON object {"score": float, "detail": str} and exits 0 only when score >= 1.
"""
import argparse
import hashlib
import importlib
import json
import os
import sys
import types
import urllib.request

METRIC_MODULES = ["table", "docs", "slides", "gimp", "general", "vscode", "pdf", "others"]
HOME_PREFIXES = ("/home/user/", "~/")


class Unsupported(Exception):
    """The task needs something a file-only replica cannot provide."""


def load_metrics(src):
    """Import OSWorld's metric modules one by one, skipping the package __init__ (which pulls in
    browsers, OCR models and other dependencies that file checks do not use)."""
    sys.path.insert(0, src)
    for name, rel in (("desktop_env", "desktop_env"), ("desktop_env.evaluators", "desktop_env/evaluators"),
                      ("desktop_env.evaluators.metrics", "desktop_env/evaluators/metrics")):
        mod = types.ModuleType(name)
        mod.__path__ = [os.path.join(src, rel)]
        sys.modules[name] = mod
    # Only an OCR-based checker uses easyocr; it is not part of the replicated set.
    sys.modules.setdefault("easyocr", types.ModuleType("easyocr"))
    found = {}
    for short in METRIC_MODULES:
        try:
            found[short] = importlib.import_module("desktop_env.evaluators.metrics." + short)
        except Exception:  # a module with missing optional dependencies is simply unavailable
            continue
    return found


def metric_fn(mods, name):
    for mod in mods.values():
        fn = getattr(mod, name, None)
        if callable(fn):
            return fn
    raise Unsupported("checker %r is not available without the VM" % name)


def vm_to_workspace(path, workspace):
    for prefix in HOME_PREFIXES:
        if path.startswith(prefix):
            return os.path.join(workspace, path[len(prefix):])
    raise Unsupported("path outside the user's home: %s" % path)


def fetch(url, dest):
    """Download url to dest once; later calls reuse the cached copy."""
    if os.path.exists(dest) and os.path.getsize(dest) > 0:
        return dest
    os.makedirs(os.path.dirname(dest), exist_ok=True)
    tmp = dest + ".part"
    with urllib.request.urlopen(url, timeout=120) as r, open(tmp, "wb") as f:
        while True:
            chunk = r.read(1 << 20)
            if not chunk:
                break
            f.write(chunk)
    os.replace(tmp, dest)
    return dest


def cache_name(url):
    return hashlib.sha256(url.encode()).hexdigest()[:12] + "_" + os.path.basename(url.split("?")[0])


def multi(config):
    if config.get("multi", False):
        return list(config["path"]), set(config.get("gives", [0]))
    return [config["path"]], {0}


def give(items, gives):
    picked = [x for i, x in enumerate(items) if i in gives]
    return picked[0] if len(picked) == 1 else picked


def get_state(config, workspace, cache):
    kind = config["type"]
    if kind == "vm_file":
        if config.get("time_suffix"):
            raise Unsupported("time-suffixed result file")
        paths, gives = multi(config)
        out = []
        for p in paths:
            local = vm_to_workspace(p, workspace)
            out.append(local if os.path.exists(local) else None)
        res = give(out, gives)
        if res is None or (isinstance(res, list) and any(x is None for x in res)):
            raise FileNotFoundError(str(paths))
        return res
    if kind == "cloud_file":
        paths, gives = multi(config)
        return give([fetch(u, os.path.join(cache, cache_name(u))) for u in paths], gives)
    if kind == "rule":
        return config["rules"]
    raise Unsupported("state type %r needs the VM" % kind)


def evaluate(task, workspace, cache, mods):
    ev = task["evaluator"]
    funcs = ev["func"]
    if funcs == "infeasible":
        raise Unsupported("infeasible-task check")
    single = not isinstance(funcs, list)
    if single:
        funcs, results = [funcs], [ev.get("result")]
        expected, options = [ev.get("expected")], [ev.get("options") or {}]
    else:
        results = ev.get("result") or [None] * len(funcs)
        expected = ev.get("expected") or [None] * len(funcs)
        options = ev.get("options") or [{}] * len(funcs)
    conj = ev.get("conj", "and")
    scores = []
    for fn_name, res_cfg, exp_cfg, opts in zip(funcs, results, expected, options):
        fn = metric_fn(mods, fn_name)
        if not res_cfg:
            raise Unsupported("checker without a result file")
        try:
            result = get_state(res_cfg, workspace, cache)
        except FileNotFoundError:
            if conj == "and":
                return 0.0
            scores.append(0.0)
            continue
        if exp_cfg:
            score = fn(result, get_state(exp_cfg, workspace, cache), **(opts or {}))
        else:
            score = fn(result, **(opts or {}))
        score = float(score or 0)
        if conj == "and" and score == 0.0:
            return 0.0
        if conj == "or" and score == 1.0:
            return 1.0
        scores.append(score)
    if not scores:
        return 0.0
    return sum(scores) / len(scores) if conj == "and" else max(scores)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--src", required=True)
    ap.add_argument("--task", required=True)
    ap.add_argument("--workspace", required=True)
    ap.add_argument("--cache", required=True)
    a = ap.parse_args()
    task = json.load(open(a.task))
    out = {"score": 0.0, "detail": ""}
    try:
        stdout, sys.stdout = sys.stdout, sys.stderr  # checkers print; keep stdout for the verdict
        try:
            out["score"] = evaluate(task, os.path.abspath(a.workspace), os.path.abspath(a.cache), load_metrics(a.src))
        finally:
            sys.stdout = stdout
    except Unsupported as e:
        out["detail"] = "unsupported: %s" % e
    except Exception as e:  # a checker crashing on the agent's file is a failed check
        out["detail"] = "%s: %s" % (type(e).__name__, e)
    print(json.dumps(out))
    sys.exit(0 if out["score"] >= 1.0 else 1)


if __name__ == "__main__":
    main()
