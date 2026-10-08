"""Unit tests for the eval harness. No real hub or LLM: a fake in-process HTTP server
mimics the few endpoints the runner uses (the live path is validated only against it)."""
import json
import os
import subprocess
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
import eval_tasks  # noqa: E402
import run_evals as R  # noqa: E402


# ------------------------------------------------------------------ task loading

def test_builtin_tasks_valid_and_named():
    names = [t["name"] for t in eval_tasks.builtin_tasks()]
    assert {"fix_failing_test", "edit_file_precise", "find_and_report", "json_transform",
            "tool_discipline", "web_lookup"} <= set(names)
    assert len(names) == len(set(names))
    web = [t for t in eval_tasks.builtin_tasks() if t["requires_web"]]
    assert [t["name"] for t in web] == ["web_lookup"]


def test_edit_file_precise_expected_differs_by_one_line():
    t = next(t for t in eval_tasks.builtin_tasks() if t["name"] == "edit_file_precise")
    orig = t["setup"]["files"]["helpers.py"]
    exp = t["verify"][0]["content"]
    assert len(orig.splitlines()) >= 100
    diff = [(a, b) for a, b in zip(orig.splitlines(), exp.splitlines()) if a != b]
    assert diff == [("def retry(fn, attempts=3, delay=1.0):", "def retry(fn, attempts=5, delay=1.0):")]


@pytest.mark.parametrize("bad", [
    {"prompt": "x", "verify": [{"type": "file_nonempty", "path": "a"}]},
    {"name": "a b", "prompt": "x", "verify": [{"type": "file_nonempty", "path": "a"}]},
    {"name": "a", "prompt": "x", "verify": []},
    {"name": "a", "prompt": "x", "verify": [{"type": "nope"}]},
    {"name": "a", "prompt": "x", "verify": [{"type": "file_equals", "path": "a"}]},
    {"name": "a", "prompt": "x", "verify": [{"type": "file_nonempty", "path": "a"}],
     "setup": {"files": {"../evil": "x"}}},
    {"name": "a", "prompt": "x", "verify": [{"type": "file_nonempty", "path": "a"}], "timeout": 0},
])
def test_validate_rejects(bad):
    with pytest.raises(eval_tasks.TaskError):
        eval_tasks.validate_task(bad)


def test_load_tasks_from_dir(tmp_path):
    (tmp_path / "x.json").write_text(json.dumps(
        {"name": "custom", "prompt": "p", "verify": [{"type": "file_nonempty", "path": "o"}]}))
    names = [t["name"] for t in eval_tasks.load_tasks(tmp_path)]
    assert "custom" in names and "fix_failing_test" in names
    (tmp_path / "bad.json").write_text("{")
    with pytest.raises(eval_tasks.TaskError):
        eval_tasks.load_tasks(tmp_path)


# --------------------------------------------------------------------- verifiers

class LocalSb:
    def __init__(self, d):
        self.d = Path(d)

    def run(self, cmd, cwd=None, timeout=120):
        p = subprocess.run(["bash", "-c", cmd], cwd=cwd or self.d, capture_output=True, text=True)
        return p.returncode, p.stdout, p.stderr

    def read(self, rel):
        p = self.d / rel
        return p.read_text() if p.exists() else None


def test_check_types(tmp_path):
    (tmp_path / "a.txt").write_text("hello world\n")
    (tmp_path / "o.csv").write_text("id,total\n1,5\n2,7\n")
    sb = LocalSb(tmp_path)
    c = R.run_check
    assert c({"type": "command", "command": "true"}, sb, [])[0]
    assert not c({"type": "command", "command": "exit 3"}, sb, [])[0]
    assert c({"type": "command", "command": "exit 3", "expect_exit": 3}, sb, [])[0]
    assert c({"type": "file_equals", "path": "a.txt", "content": "hello world", "strip": True}, sb, [])[0]
    assert not c({"type": "file_equals", "path": "a.txt", "content": "hello world"}, sb, [])[0]
    assert c({"type": "file_contains", "path": "a.txt", "substrings": ["hello", "world"]}, sb, [])[0]
    assert not c({"type": "file_contains", "path": "a.txt", "substrings": ["nope"]}, sb, [])[0]
    assert c({"type": "file_not_contains", "path": "a.txt", "substrings": ["nope"]}, sb, [])[0]
    assert c({"type": "file_regex", "path": "a.txt", "pattern": r"^hel+o"}, sb, [])[0]
    assert c({"type": "file_nonempty", "path": "a.txt"}, sb, [])[0]
    assert not c({"type": "file_nonempty", "path": "missing"}, sb, [])[0]
    assert c({"type": "csv_equals", "path": "o.csv", "rows": [["id", "total"], ["1", "5"], ["2", "7"]]}, sb, [])[0]
    assert not c({"type": "csv_equals", "path": "o.csv", "rows": [["id", "total"]]}, sb, [])[0]
    assert c({"type": "tool_called", "tool": "web"}, sb, ["switchboard.web.search"])[0]
    assert not c({"type": "tool_called", "tool": "web"}, sb, ["x"])[0]


def test_builtin_verifiers_accept_correct_solutions(tmp_path):
    """Sanity: writing the expected outputs makes each deterministic verifier pass, and the
    untouched setup does not (except where nothing is required yet)."""
    for t in eval_tasks.builtin_tasks():
        if t["requires_web"]:
            continue
        d = tmp_path / t["name"]
        for rel, content in t["setup"]["files"].items():
            (d / rel).parent.mkdir(parents=True, exist_ok=True)
            (d / rel).write_text(content)
        sb = LocalSb(d)
        ok_before, _ = R.run_verifier(t, sb, [])
        assert not ok_before, t["name"]
        for chk in t["verify"]:
            if chk["type"] == "file_equals" and chk["path"] not in t["setup"]["files"] or \
               chk["type"] == "file_equals" and chk["path"] == "helpers.py":
                (d / chk["path"]).write_text(chk["content"])
            elif chk["type"] == "csv_equals":
                (d / chk["path"]).write_text("\n".join(",".join(r) for r in chk["rows"]) + "\n")
        if t["name"] == "find_and_report":
            (d / "answer.txt").write_text("defined: src/util/hash.py\nused: src/app.py\nused: src/jobs/sync.py\n")
        if t["name"] == "fix_failing_test":
            (d / "stats.py").write_text(
                "def mean(xs):\n    return sum(xs)/len(xs)\n\n\ndef median(xs):\n    ys = sorted(xs)\n"
                "    n = len(ys)\n    return ys[n//2] if n % 2 else (ys[n//2-1]+ys[n//2])/2\n")
            if subprocess.run([sys.executable, "-m", "pytest", "--version"], capture_output=True).returncode:
                continue
            # the verifier uses python3; make sure it is this interpreter's pytest
            t = dict(t, verify=[dict(v, command=v["command"].replace("python3", sys.executable))
                                if v["type"] == "command" else v for v in t["verify"]])
        ok, notes = R.run_verifier(t, sb, [])
        assert ok, (t["name"], notes)


# ----------------------------------------------------------- payload / rendering

def test_extract_payload_shapes():
    assert R.extract_payload({"status": "ok", "result": {"structuredContent": {"a": 1}}}) == {"a": 1}
    assert R.extract_payload({"status": "ok", "result": {"content": [{"type": "text", "text": '{"b": 2}'}]}}) == {"b": 2}
    assert R.extract_payload({"status": "ok", "result": {"content": [{"type": "text", "text": "hi"}]}}) == {"text": "hi"}
    with pytest.raises(R.ToolError):
        R.extract_payload({"status": "error", "error": "boom"})
    with pytest.raises(R.ToolError):
        R.extract_payload({"status": "ok", "result": {"isError": True, "content": [{"type": "text", "text": "bad"}]}})


def _res(model, task, status, calls=3, secs=1.5, cost=0.0):
    return {"model": model, "task": task, "status": status, "tool_calls": calls, "seconds": secs, "cost_usd": cost}


def test_table_and_summary():
    rs = [_res("p/a", "t1", "PASS"), _res("p/a", "t2", "FAIL"), _res("p/b", "t1", "ERROR", 0),
          _res("p/b", "t2", "PASS", cost=0.01)]
    table = R.render_table(rs)
    assert "PASS 3c 1.5s" in table and "FAIL 3c 1.5s" in table and "ERROR 1.5s" in table
    assert table.splitlines()[0].split() == ["model", "t1", "t2"]
    rates = R.pass_rates(rs)
    assert rates["p/a"]["rate"] == 0.5 and rates["p/b"]["error"] == 1
    assert "p/a: 1/2 passed (50%)" in R.render_summary(rs)
    assert R.failed_threshold(rs, 0.5) == []
    assert R.failed_threshold(rs, 0.75) == ["p/a", "p/b"]
    assert R.failed_threshold(rs, 60) == ["p/a", "p/b"]  # percent form


def test_load_models_and_pick_connection(tmp_path):
    f = tmp_path / "m.json"
    f.write_text(json.dumps({"models": [{"provider": "x", "model": "m:1"}]}))
    assert R.load_models("all", f) == [{"provider": "x", "model": "m:1"}]
    assert R.load_models("a/b,c/d/e", f) == [{"provider": "a", "model": "b"}, {"provider": "c", "model": "d/e"}]
    with pytest.raises(R.EvalError):
        R.load_models("nomodel", f)
    conns = [{"id": "1", "label": "a", "servers": []},
             {"id": "2", "label": "b", "servers": [{"name": "harness", "tools": []}]}]
    assert R.pick_connection(conns, None, "harness")[0]["id"] == "2"
    with pytest.raises(R.EvalError):
        R.pick_connection(conns, "a", "harness")
    with pytest.raises(R.EvalError):
        R.pick_connection(conns, "zzz", "harness")


# ------------------------------------------------------------------ fake hub

class FakeHub:
    """Minimal hub: harness tools run on the local disk, 'agent' is a callback."""

    def __init__(self, root, agent, models=(("fake", "m1"),), connections=True, run_status="done"):
        self.root, self.agent, self.models = Path(root), agent, models
        self.run_status = run_status
        self.chats, self.messages, self.profiles, self.calls = {}, {}, {}, {}
        self.connections = connections
        self.deleted_chats, self.auth = [], []
        outer = self

        class H(BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def _send(self, code, obj=None):
                data = b"" if obj is None else json.dumps(obj).encode()
                self.send_response(code)
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

            def _body(self):
                n = int(self.headers.get("Content-Length") or 0)
                return json.loads(self.rfile.read(n)) if n else {}

            def do_GET(self):
                outer.auth.append(self.headers.get("Authorization"))
                outer.get(self, self.path)

            def do_POST(self):
                outer.post(self, self.path, self._body())

            def do_DELETE(self):
                outer.delete(self, self.path)

        self.srv = ThreadingHTTPServer(("127.0.0.1", 0), H)
        self.url = f"http://127.0.0.1:{self.srv.server_address[1]}"
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()

    def close(self):
        self.srv.shutdown()
        self.srv.server_close()

    def get(self, h, path):
        base = path.split("?")[0]
        if base == "/api/stats":
            return h._send(200, {"calls": {}})
        if base == "/api/models":
            return h._send(200, {"models": [{"provider": p, "model": m} for p, m in self.models]})
        if base == "/api/profiles":
            return h._send(200, {"profiles": [{"id": "p0", "name": "Assistant", "isDefault": True,
                                                "systemPrompt": "Be useful.", "capabilities": {}, "budget": {}}]})
        if base == "/api/connections":
            tools = [{"name": "bash"}, {"name": "write"}, {"name": "read"}]
            return h._send(200, {"connections": [{"id": "c1", "label": "laptop",
                                                   "servers": [{"name": "harness", "tools": tools}]}] if self.connections else []})
        parts = base.strip("/").split("/")
        if parts[:2] == ["api", "runs"]:
            return h._send(200, {"id": parts[2], "status": self.run_status, "pendingApprovals": []})
        if parts[:2] == ["api", "chats"] and parts[3] == "messages":
            return h._send(200, {"messages": self.messages.get(parts[2], [])})
        if base == "/api/calls":
            return h._send(200, {"calls": []})
        h._send(404, {"detail": "nope"})

    def post(self, h, path, body):
        parts = path.strip("/").split("/")
        if path == "/api/profiles":
            self.profiles["pf1"] = body
            return h._send(201, {"id": "pf1", **body})
        if path == "/api/chats":
            cid = f"chat{len(self.chats) + 1}"
            self.chats[cid] = body
            self.messages[cid] = []
            return h._send(201, {"id": cid, **body})
        if parts[:2] == ["api", "chats"] and parts[3] == "messages":
            cid = parts[2]
            self.messages[cid] = self.agent(self, body["content"])
            return h._send(202, {"messageId": "m1", "runId": "run1"})
        if parts[:2] == ["api", "runs"]:
            return h._send(204)
        if parts[:2] == ["api", "connections"]:
            tool = parts[6]
            args = body["arguments"]
            return h._send(200, self.tool(tool, args))
        h._send(404, {"detail": "nope"})

    def delete(self, h, path):
        parts = path.strip("/").split("/")
        if parts[1] == "chats":
            self.deleted_chats.append(parts[2])
            return h._send(200, {"deletedChats": 1})
        if parts[1] == "profiles":
            self.profiles.pop(parts[2], None)
        h._send(204)

    def tool(self, tool, a):
        def ok(sc):
            return {"status": "ok", "result": {"structuredContent": sc}}
        if tool == "bash":
            p = subprocess.run(["bash", "-c", a["command"]], cwd=a.get("cwd") or self.root,
                               capture_output=True, text=True)
            return ok({"stdout": p.stdout, "stderr": p.stderr, "exit_code": p.returncode})
        if tool == "write":
            p = Path(a["path"])
            p.parent.mkdir(parents=True, exist_ok=True)
            p.write_text(a["content"])
            return ok({"success": True, "message": "ok"})
        if tool == "read":
            p = Path(a["path"])
            if not p.exists():
                return {"status": "error", "error": "no such file", "result": None}
            return ok({"text": p.read_text()})
        return {"status": "error", "error": "unknown tool"}


TRIVIAL = {"name": "trivial", "prompt": "Write hello to {sandbox}/out.txt",
           "setup": {"files": {"in.txt": "seed"}},
           "verify": [{"type": "file_equals", "path": "out.txt", "content": "hello", "strip": True}],
           "timeout": 10, "max_turns": 5}


def good_agent(fake, prompt):
    sandbox = prompt.split("to ")[1].split("/out.txt")[0]
    Path(sandbox, "out.txt").write_text("hello\n")
    return [{"role": "user", "content": []},
            {"role": "assistant", "toolCalls": [{"id": "1", "name": "harness__write", "arguments": {}}],
             "tokenInput": 10, "tokenOutput": 5, "costMicros": 2000},
            {"role": "tool", "toolResults": [{"tool_call_id": "1", "error": None}]},
            {"role": "assistant", "toolCalls": [{"id": "2", "name": "harness__x", "arguments": {}}],
             "tokenInput": 4, "tokenOutput": 1, "costMicros": 0},
            {"role": "tool", "toolResults": [{"tool_call_id": "2", "error": "bad"}]}]


@pytest.fixture
def root(tmp_path):
    r = tmp_path / "root"
    r.mkdir()
    return r


def run_main(fake, tmp_path, *extra, tasks_dir=None):
    td = tmp_path / "tasks"
    td.mkdir(exist_ok=True)
    (td / "trivial.json").write_text(json.dumps(TRIVIAL))
    lines = []
    code = R.main(["--hub", fake.url, "--token", "sekret", "--models", "fake/m1", "--tasks", "trivial",
                   "--tasks-dir", str(td), "--poll", "0.01", *extra], out=lines.append)
    return code, "\n".join(lines)


def test_end_to_end_pass(root, tmp_path):
    fake = FakeHub(root, good_agent)
    try:
        out_json = tmp_path / "res.json"
        code, text = run_main(fake, tmp_path, "--json", str(out_json), "--min-pass", "1")
    finally:
        fake.close()
    assert code == 0, text
    assert "PASS 2c" in text and "fake/m1: 1/1 passed (100%)" in text
    data = json.loads(out_json.read_text())
    r = data["results"][0]
    assert r["status"] == "PASS" and r["tool_calls"] == 2 and r["tool_errors"] == 1
    assert r["tokens_in"] == 14 and r["tokens_out"] == 6 and abs(r["cost_usd"] - 0.002) < 1e-9
    assert fake.deleted_chats == ["chat1"]
    assert fake.profiles == {}                      # temp profile removed
    assert fake.chats["chat1"]["clientLabel"] == "laptop" and fake.chats["chat1"]["model"] == {"provider": "fake", "model": "m1"}
    assert "Bearer sekret" in fake.auth
    left = list((root / "eval-sandbox").rglob("out.txt")) if (root / "eval-sandbox").exists() else []
    assert left == []                               # sandbox cleaned


def test_end_to_end_fail_and_min_pass(root, tmp_path):
    fake = FakeHub(root, lambda f, p: [])           # the "agent" does nothing
    try:
        code, text = run_main(fake, tmp_path, "--min-pass", "0.5")
    finally:
        fake.close()
    assert code == 1
    assert "FAIL" in text and "out.txt does not exist" not in text.split("running")[0]


def test_keep_leaves_sandbox_and_chat(root, tmp_path):
    fake = FakeHub(root, good_agent)
    try:
        code, _ = run_main(fake, tmp_path, "--keep")
    finally:
        fake.close()
    assert code == 0 and fake.deleted_chats == [] and list((root / "eval-sandbox").rglob("out.txt"))


def test_run_error_status_is_error(root, tmp_path):
    fake = FakeHub(root, good_agent, run_status="error")
    try:
        _, text = run_main(fake, tmp_path)
    finally:
        fake.close()
    assert "ERROR" in text


def test_model_unavailable_is_error_not_crash(root, tmp_path):
    fake = FakeHub(root, good_agent, models=(("other", "z"),))
    try:
        code, text = run_main(fake, tmp_path)
    finally:
        fake.close()
    assert code == 0 and "ERROR" in text and "NOT available" in text


def test_no_client_and_unreachable_and_dry_run(root, tmp_path):
    fake = FakeHub(root, good_agent, connections=False)
    try:
        code, text = run_main(fake, tmp_path)
        assert code == 2 and "no connected client" in text
    finally:
        fake.close()
    lines = []
    assert R.main(["--hub", "http://127.0.0.1:1", "--dry-run"], out=lines.append) == 2
    assert "unreachable" in lines[0]
    fake = FakeHub(root, good_agent)
    try:
        code, text = run_main(fake, tmp_path, "--dry-run")
    finally:
        fake.close()
    assert code == 0 and "trivial" in text and not fake.chats
