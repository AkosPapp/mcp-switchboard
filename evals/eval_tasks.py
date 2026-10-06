"""Built-in eval tasks. Pure data plus a validator; no hub access.

A task is a dict:

  name          unique slug
  description   one line
  prompt        what the agent is told; "{sandbox}" is replaced by the task's directory
  setup         {"files": {relative_path: content}}  written before the run
  verify        list of checks (see run_evals.run_check for the types)
  timeout       seconds for the whole agent run
  max_turns     assistant turns allowed before the run is cancelled
  auto_approve  true: approve tool approvals that show up; false: the eval profile uses
                approval "never" and an approval request counts as an error
  requires_web  true: only run with --with-web

More tasks can be dropped into evals/tasks/*.json in the same shape (see docs/EVALS.md).
"""
from __future__ import annotations

import csv
import io
import json
from pathlib import Path

CHECK_TYPES = {
    "command", "file_equals", "file_contains", "file_not_contains", "file_regex",
    "file_nonempty", "csv_equals", "tool_called",
}


class TaskError(ValueError):
    pass


def validate_task(task: dict, source: str = "task") -> dict:
    """Return the task with defaults filled in, or raise TaskError."""
    if not isinstance(task, dict):
        raise TaskError(f"{source}: not an object")
    for key in ("name", "prompt", "verify"):
        if key not in task:
            raise TaskError(f"{source}: missing '{key}'")
    name = task["name"]
    if not isinstance(name, str) or not name or not all(c.isalnum() or c in "_-" for c in name):
        raise TaskError(f"{source}: name must be a slug, got {name!r}")
    if not isinstance(task["prompt"], str) or not task["prompt"].strip():
        raise TaskError(f"{name}: empty prompt")
    setup = task.setdefault("setup", {})
    files = setup.get("files", {})
    if not isinstance(files, dict) or not all(isinstance(k, str) and isinstance(v, str) for k, v in files.items()):
        raise TaskError(f"{name}: setup.files must map path -> string")
    for p in files:
        if p.startswith("/") or ".." in Path(p).parts:
            raise TaskError(f"{name}: setup path {p!r} must be relative and stay inside the sandbox")
    verify = task["verify"]
    if not isinstance(verify, list) or not verify:
        raise TaskError(f"{name}: verify must be a non-empty list")
    for chk in verify:
        if not isinstance(chk, dict) or chk.get("type") not in CHECK_TYPES:
            raise TaskError(f"{name}: bad check {chk!r}; type must be one of {sorted(CHECK_TYPES)}")
        t = chk["type"]
        need = {"command": ["command"], "file_equals": ["path", "content"],
                "file_contains": ["path", "substrings"], "file_not_contains": ["path", "substrings"],
                "file_regex": ["path", "pattern"], "file_nonempty": ["path"],
                "csv_equals": ["path", "rows"], "tool_called": ["tool"]}[t]
        for k in need:
            if k not in chk:
                raise TaskError(f"{name}: check {t} missing '{k}'")
    task.setdefault("description", "")
    for key, default in (("timeout", 300), ("max_turns", 25)):
        v = task.setdefault(key, default)
        if not isinstance(v, (int, float)) or v <= 0:
            raise TaskError(f"{name}: {key} must be a positive number")
    task.setdefault("auto_approve", False)
    task.setdefault("requires_web", False)
    return task


# ---------------------------------------------------------------- builtin tasks

def _fix_failing_test() -> dict:
    test = '''from stats import median, mean


def test_mean():
    assert mean([1, 2, 3, 4]) == 2.5


def test_median_odd():
    assert median([5, 1, 3]) == 3


def test_median_even():
    assert median([4, 1, 3, 2]) == 2.5


def test_median_does_not_mutate():
    xs = [3, 1, 2]
    median(xs)
    assert xs == [3, 1, 2]
'''
    module = '''def mean(xs):
    return sum(xs) / len(xs)


def median(xs):
    ys = sorted(xs)
    return ys[len(ys) // 2]
'''
    return {
        "name": "fix_failing_test",
        "description": "Fix a bug in stats.median so the pytest suite passes.",
        "prompt": "In {sandbox} there is a small Python module stats.py with tests in test_stats.py. "
                  "The tests fail. Fix the bug in stats.py (do not edit the tests) so that "
                  "`python3 -m pytest -q` passes when run in that directory.",
        "setup": {"files": {"stats.py": module, "test_stats.py": test}},
        "verify": [
            {"type": "command", "command": "python3 -m pytest -q -p no:cacheprovider", "expect_exit": 0},
            {"type": "file_equals", "path": "test_stats.py", "content": test},
        ],
        "timeout": 300, "max_turns": 20,
    }


def _edit_file_precise() -> dict:
    lines = ['"""Generated helpers."""', ""]
    n = 0
    while len(lines) < 100:
        n += 1
        if n == 7:
            lines += ["def retry(fn, attempts=3, delay=1.0):",
                      "    for i in range(attempts):",
                      "        try:",
                      "            return fn()",
                      "        except Exception:",
                      "            if i == attempts - 1:",
                      "                raise",
                      "    return None", "", ""]
        else:
            lines += [f"def helper_{n}(x, factor={n}):", f"    return x * factor + {n}", "", ""]
    original = "\n".join(lines) + "\n"
    expected = original.replace("def retry(fn, attempts=3, delay=1.0):", "def retry(fn, attempts=5, delay=1.0):")
    assert original != expected
    return {
        "name": "edit_file_precise",
        "description": "Change one default argument in a ~100-line file, touching nothing else.",
        "prompt": "In {sandbox}/helpers.py, change the default value of the `attempts` parameter of "
                  "the function `retry` from 3 to 5. Do not change anything else in the file.",
        "setup": {"files": {"helpers.py": original}},
        "verify": [{"type": "file_equals", "path": "helpers.py", "content": expected}],
        "timeout": 240, "max_turns": 12,
    }


def _find_and_report() -> dict:
    files = {
        "src/util/hash.py": "def compute_checksum(data):\n    return sum(data) % 251\n\n\ndef compute_checksum_v2(data):\n    return sum(data) % 257\n",
        "src/util/__init__.py": "",
        "src/app.py": "from util.hash import compute_checksum\n\n\ndef main():\n    print(compute_checksum(b'abc'))\n",
        "src/jobs/sync.py": "from util import hash as h\n\n\ndef run(blob):\n    return h.compute_checksum(blob)\n",
        "src/jobs/report.py": "def run():\n    return 'no checksum needed here'\n",
        "src/legacy/old.py": "def compute_checksum_legacy(data):\n    return len(data)\n",
        "README.md": "# demo\nSee src/app.py.\n",
    }
    return {
        "name": "find_and_report",
        "description": "Locate where a function is defined and every file that calls it.",
        "prompt": "The repository in {sandbox} has a function named exactly `compute_checksum`. Find the "
                  "one file that defines it and every file that calls it (similarly named functions do "
                  "not count). Write the answer to {sandbox}/answer.txt as lines "
                  "`defined: <path relative to the repo>` and `used: <path relative to the repo>` "
                  "(one `used:` line per calling file).",
        "setup": {"files": files},
        "verify": [
            {"type": "file_contains", "path": "answer.txt",
             "substrings": ["src/util/hash.py", "src/app.py", "src/jobs/sync.py"]},
            {"type": "file_not_contains", "path": "answer.txt", "substrings": ["old.py", "report.py"]},
        ],
        "timeout": 240, "max_turns": 15,
    }


def _json_transform() -> dict:
    orders = [
        {"id": 1, "customer": "ada", "status": "paid", "total": 120.5},
        {"id": 2, "customer": "bob", "status": "refunded", "total": 80.0},
        {"id": 3, "customer": "cyd", "status": "paid", "total": 49.99},
        {"id": 4, "customer": "dee", "status": "paid", "total": 200.0},
        {"id": 5, "customer": "eli", "status": "pending", "total": 300.0},
        {"id": 6, "customer": "fay", "status": "paid", "total": 50.0},
        {"id": 7, "customer": "gus", "status": "paid", "total": 75.25},
        {"id": 8, "customer": "hal", "status": "cancelled", "total": 60.0},
        {"id": 9, "customer": "ivy", "status": "paid", "total": 10.0},
        {"id": 10, "customer": "jon", "status": "paid", "total": 99.0},
    ]
    keep = sorted((o for o in orders if o["status"] == "paid" and o["total"] >= 50),
                  key=lambda o: -o["total"])
    rows = [["id", "customer", "total"]] + [[str(o["id"]), o["customer"], f"{o['total']:g}"] for o in keep]
    return {
        "name": "json_transform",
        "description": "Filter and sort a JSON array into a CSV file.",
        "prompt": "{sandbox}/orders.json holds a JSON array of orders. Write {sandbox}/paid_orders.csv "
                  "with the header `id,customer,total` and one row per order whose status is `paid` "
                  "and whose total is at least 50, sorted by total descending. Write totals as plain "
                  "numbers without trailing zeros (120.5, 200, 50).",
        "setup": {"files": {"orders.json": json.dumps(orders, indent=2) + "\n"}},
        "verify": [{"type": "csv_equals", "path": "paid_orders.csv", "rows": rows}],
        "timeout": 240, "max_turns": 12,
    }


def _tool_discipline() -> dict:
    files = {
        "start.txt": "The next file to read is notes/k7.txt\n",
        "notes/k7.txt": "code: 4821\nThe vault file is vault/4821.txt\n",
        "notes/k3.txt": "code: 1111\nThe vault file is vault/1111.txt\n",
        "vault/4821.txt": "SECRET=maple-9917\n",
        "vault/1111.txt": "SECRET=decoy-0000\n",
    }
    return {
        "name": "tool_discipline",
        "description": "Follow a chain of three files where each tool call needs the previous result.",
        "prompt": "Start by reading {sandbox}/start.txt. It points to a next file; each file points to "
                  "the following one (paths are relative to {sandbox}). Follow the chain until you "
                  "find a line `SECRET=<value>`, then write just the value (nothing else) to "
                  "{sandbox}/final.txt. Do not guess paths; only read files you were pointed to.",
        "setup": {"files": files},
        "verify": [{"type": "file_equals", "path": "final.txt", "content": "maple-9917", "strip": True}],
        "timeout": 240, "max_turns": 12,
    }


def _web_lookup() -> dict:
    return {
        "name": "web_lookup",
        "description": "Answer a factual question using the hub's web search/fetch tools.",
        "prompt": "Use your web search or fetch tool (not memory) to find in which year Python 3.0 "
                  "was first released, and write one sentence with the answer and the source URL "
                  "to {sandbox}/answer.txt.",
        "setup": {"files": {}},
        "verify": [
            {"type": "file_nonempty", "path": "answer.txt"},
            {"type": "tool_called", "tool": "web"},
        ],
        "timeout": 300, "max_turns": 15, "requires_web": True,
    }


def builtin_tasks() -> list[dict]:
    return [validate_task(f(), f.__name__) for f in (
        _fix_failing_test, _edit_file_precise, _find_and_report, _json_transform,
        _tool_discipline, _web_lookup)]


def load_tasks(tasks_dir: str | Path | None = None) -> list[dict]:
    """Builtins plus every *.json in tasks_dir (default evals/tasks). A json file
    holds one task object or a list of them; a name clash with a builtin overrides it."""
    tasks = {t["name"]: t for t in builtin_tasks()}
    d = Path(tasks_dir) if tasks_dir else Path(__file__).parent / "tasks"
    if d.is_dir():
        for f in sorted(d.glob("*.json")):
            try:
                data = json.loads(f.read_text())
            except json.JSONDecodeError as e:
                raise TaskError(f"{f.name}: invalid JSON: {e}") from e
            for item in data if isinstance(data, list) else [data]:
                t = validate_task(item, f.name)
                tasks[t["name"]] = t
    return list(tasks.values())


def parse_csv(text: str) -> list[list[str]]:
    return [[c.strip() for c in row] for row in csv.reader(io.StringIO(text.strip())) if row]
