#!/usr/bin/env python3
"""Eval harness for the hub's agent loop. Talks to a RUNNING hub over REST; stdlib only.

    python evals/run_evals.py --models all --client my-laptop --json out.json

See docs/EVALS.md.
"""
from __future__ import annotations

import argparse
import json
import os
import re
import sys
import time
import urllib.error
import urllib.request
import uuid
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from eval_tasks import TaskError, load_tasks, parse_csv  # noqa: E402

DEFAULT_HUB = "http://127.0.0.1:8099"  # the private listener's default port
TERMINAL = {"done", "cancelled", "interrupted", "error"}
PASS, FAIL, ERROR = "PASS", "FAIL", "ERROR"


class HubError(Exception):
    def __init__(self, msg: str, status: int | None = None):
        super().__init__(msg)
        self.status = status


class ToolError(Exception):
    pass


class EvalError(Exception):
    """A problem with the environment or setup, reported as ERROR for the cell."""


# ------------------------------------------------------------------ REST client

class Hub:
    def __init__(self, url: str, token: str | None = None, timeout: float = 60):
        self.url = url.rstrip("/")
        self.token = token
        self.timeout = timeout

    def request(self, method: str, path: str, body=None, headers=None, timeout=None):
        data = None if body is None else json.dumps(body).encode()
        h = {"Accept": "application/json"}
        if data is not None:
            h["Content-Type"] = "application/json"
        if self.token:
            h["Authorization"] = f"Bearer {self.token}"
        h.update(headers or {})
        req = urllib.request.Request(self.url + path, data=data, method=method, headers=h)
        try:
            with urllib.request.urlopen(req, timeout=timeout or self.timeout) as resp:
                raw = resp.read()
        except urllib.error.HTTPError as e:
            raw = e.read()
            detail = raw.decode(errors="replace")[:300]
            try:
                detail = json.loads(raw).get("detail") or json.loads(raw).get("error") or detail
            except Exception:
                pass
            raise HubError(f"{method} {path} -> {e.code}: {detail}", e.code) from None
        except (urllib.error.URLError, OSError, TimeoutError) as e:
            raise HubError(f"hub unreachable at {self.url} ({e})") from None
        if not raw:
            return None
        try:
            return json.loads(raw)
        except json.JSONDecodeError:
            raise HubError(f"{method} {path}: response is not JSON") from None

    def get(self, path, **kw):
        return self.request("GET", path, **kw)

    def post(self, path, body=None, **kw):
        return self.request("POST", path, body if body is not None else {}, **kw)

    def delete(self, path, **kw):
        return self.request("DELETE", path, **kw)


def extract_payload(record: dict) -> dict:
    """Turn a manual-call response (a call row) into the tool's output dict.
    Raises ToolError when the call failed."""
    if not isinstance(record, dict):
        raise ToolError("unexpected call response")
    result = record.get("result")
    if record.get("status") == "error" or record.get("error"):
        msg = record.get("error") or ""
        if not msg and isinstance(result, dict):
            msg = _text_of(result)
        raise ToolError(msg or "tool error")
    if not isinstance(result, dict):
        return {}
    if result.get("isError"):
        raise ToolError(_text_of(result) or "tool error")
    for key in ("structuredContent", "structured_content"):
        if isinstance(result.get(key), dict):
            return result[key]
    text = _text_of(result)
    if text:
        try:
            v = json.loads(text)
            if isinstance(v, dict):
                return v
        except json.JSONDecodeError:
            pass
        return {"text": text}
    return result


def _text_of(result: dict) -> str:
    content = result.get("content")
    if isinstance(content, list):
        return "\n".join(c.get("text", "") for c in content if isinstance(c, dict) and c.get("type") == "text")
    return ""


# -------------------------------------------------------- sandbox on the client

class Sandbox:
    """A directory on the agent's client machine, reached only through the hub's
    manual tool-call endpoint, so it works for remote clients too."""

    def __init__(self, hub: Hub, cid: str, server: str, path: str):
        self.hub, self.cid, self.server, self.path = hub, cid, server, path.rstrip("/")

    def call(self, tool: str, args: dict, timeout: float = 180) -> dict:
        rec = self.hub.post(f"/api/connections/{self.cid}/servers/{self.server}/tools/{tool}/call",
                            {"arguments": args}, timeout=timeout)
        return extract_payload(rec)

    def abs(self, rel: str) -> str:
        return rel if rel.startswith("/") else f"{self.path}/{rel}"

    def run(self, command: str, cwd: str | None = None, timeout: int = 120) -> tuple[int, str, str]:
        out = self.call("run_command", {"command": command, "cwd": cwd or self.path, "timeout": timeout},
                        timeout=timeout + 30)
        return int(out.get("exit_code", -1)), out.get("stdout", ""), out.get("stderr", "")

    def write(self, rel: str, content: str) -> None:
        out = self.call("file_write", {"path": self.abs(rel), "content": content})
        if out.get("success") is False:
            raise ToolError(out.get("message", "file_write failed"))

    def read(self, rel: str) -> str | None:
        """File text, or None if it does not exist / cannot be read."""
        text, offset = "", 0
        while True:
            try:
                out = self.call("file_read", {"path": self.abs(rel), "offset": offset})
            except ToolError:
                return None
            chunk = out.get("raw_text", "")
            text += chunk
            if not out.get("truncated") or not chunk:
                return text
            offset += len(chunk)

    def remove(self) -> None:
        if "eval-sandbox" in self.path:  # never rm -rf something we did not make
            self.run(f"rm -rf -- '{self.path}'", cwd="/")


def discover_root(hub: Hub, cid: str, server: str) -> str:
    rec = hub.post(f"/api/connections/{cid}/servers/{server}/tools/run_command/call",
                   {"arguments": {"command": "pwd"}}, timeout=60)
    root = extract_payload(rec).get("stdout", "").strip()
    if not root.startswith("/"):
        raise EvalError(f"could not discover the harness root (pwd said {root!r})")
    return root


# --------------------------------------------------------------------- verifiers

def run_check(check: dict, sb, tool_names: list[str]) -> tuple[bool, str]:
    """Evaluate one verifier check. `sb` needs .run(cmd) and .read(path)."""
    t = check["type"]
    if t == "command":
        code, out, err = sb.run(check["command"])
        want = check.get("expect_exit", 0)
        ok = code == want
        tail = (out + err).strip()[-300:]
        return ok, f"`{check['command']}` exit {code} (want {want})" + ("" if ok else f": {tail}")
    if t == "tool_called":
        ok = any(check["tool"] in n for n in tool_names)
        return ok, f"tool matching {check['tool']!r} " + ("was called" if ok else f"was not called (saw {sorted(set(tool_names))})")
    text = sb.read(check["path"])
    if text is None:
        return False, f"{check['path']} does not exist"
    if t == "file_nonempty":
        return bool(text.strip()), f"{check['path']} " + ("has content" if text.strip() else "is empty")
    if t == "file_equals":
        a, b = text, check["content"]
        if check.get("strip"):
            a, b = a.strip(), b.strip()
        else:
            a, b = a.replace("\r\n", "\n"), b.replace("\r\n", "\n")
        return a == b, f"{check['path']} " + ("matches" if a == b else f"differs from expected ({_first_diff(a, b)})")
    if t == "file_contains":
        missing = [s for s in check["substrings"] if s not in text]
        return not missing, f"{check['path']} " + ("has all expected text" if not missing else f"is missing {missing}")
    if t == "file_not_contains":
        present = [s for s in check["substrings"] if s in text]
        return not present, f"{check['path']} " + ("has no forbidden text" if not present else f"contains forbidden {present}")
    if t == "file_regex":
        ok = re.search(check["pattern"], text, re.M) is not None
        return ok, f"{check['path']} " + ("matches" if ok else f"does not match /{check['pattern']}/")
    if t == "csv_equals":
        got = parse_csv(text)
        want = [[str(c).strip() for c in r] for r in check["rows"]]
        return got == want, f"{check['path']} " + ("matches" if got == want else f"rows differ (got {got[:4]}..., want {want[:4]}...)")
    return False, f"unknown check type {t}"


def _first_diff(a: str, b: str) -> str:
    al, bl = a.splitlines(), b.splitlines()
    for i in range(max(len(al), len(bl))):
        x = al[i] if i < len(al) else "<eof>"
        y = bl[i] if i < len(bl) else "<eof>"
        if x != y:
            return f"line {i + 1}: got {x[:60]!r}, want {y[:60]!r}"
    return "whitespace/length"


def run_verifier(task: dict, sb, tool_names: list[str]) -> tuple[bool, list[str]]:
    notes, ok_all = [], True
    for chk in task["verify"]:
        ok, msg = run_check(chk, sb, tool_names)
        ok_all &= ok
        notes.append(("ok: " if ok else "FAIL: ") + msg)
    return ok_all, notes


# ------------------------------------------------------------------ run one task

class Ctx:
    def __init__(self, hub, cid, server, root, profile_id, tag, keep=False, poll=1.0, out=None):
        self.hub, self.cid, self.server, self.root = hub, cid, server, root
        self.profile_id, self.tag, self.keep, self.poll = profile_id, tag, keep, poll
        self.client_label = None
        self.out = out or (lambda s: None)


def collect_metrics(hub: Hub, chat_id: str) -> dict:
    m = {"tool_calls": 0, "tool_errors": 0, "turns": 0, "tokens_in": 0, "tokens_out": 0,
         "cost_usd": 0.0, "tool_names": []}
    try:
        msgs = hub.get(f"/api/chats/{chat_id}/messages").get("messages", [])
    except HubError:
        msgs = []
    for msg in msgs:
        if msg.get("role") == "assistant":
            m["turns"] += 1
            m["tokens_in"] += msg.get("tokenInput") or 0
            m["tokens_out"] += msg.get("tokenOutput") or 0
            m["cost_usd"] += (msg.get("costMicros") or 0) / 1e6
            for tc in msg.get("toolCalls") or []:
                m["tool_names"].append(tc.get("name", ""))
        for tr in msg.get("toolResults") or []:
            if tr.get("error"):
                m["tool_errors"] += 1
    m["tool_calls"] = len(m["tool_names"])
    try:
        rows = hub.get(f"/api/calls?chatId={chat_id}&limit=1000").get("calls", [])
    except HubError:
        rows = []
    call_names = [r.get("exposedName") or r.get("tool") or "" for r in rows]
    if len(call_names) > m["tool_calls"]:  # the call log is the fuller record when messages lack it
        m["tool_calls"] = len(call_names)
    m["tool_names"] += call_names
    m["tool_errors"] = max(m["tool_errors"], sum(1 for r in rows if r.get("status") == "error"))
    return m


def wait_run(hub: Hub, run_id: str, chat_id: str, task: dict, poll: float, auto_approve: bool) -> tuple[str, str]:
    """Poll until the run ends. Returns (run_status, note); run_status may be 'timeout'."""
    deadline = time.monotonic() + task["timeout"]
    last_turn_check = 0.0
    while True:
        run = hub.get(f"/api/runs/{run_id}")
        status = run.get("status")
        if status in TERMINAL:
            return status, run.get("error") or ""
        if status == "waiting":
            approvals = run.get("pendingApprovals") or []
            if not approvals:
                _cancel(hub, run_id)
                return "waiting", "run is waiting for input (a question?) that an eval cannot answer"
            if not auto_approve:
                _cancel(hub, run_id)
                return "waiting", f"approval required for {approvals[0].get('tool')} but the task has auto_approve=false"
            for ap in approvals:
                try:
                    hub.post(f"/api/runs/{run_id}/approvals/{ap['callId']}", {"approved": True})
                except HubError as e:
                    if e.status != 409:
                        raise
        now = time.monotonic()
        if now > deadline:
            _cancel(hub, run_id)
            return "timeout", f"exceeded {task['timeout']}s"
        if now - last_turn_check > 2:
            last_turn_check = now
            try:
                msgs = hub.get(f"/api/chats/{chat_id}/messages").get("messages", [])
                if sum(1 for x in msgs if x.get("role") == "assistant") > task["max_turns"]:
                    _cancel(hub, run_id)
                    return "max_turns", f"more than {task['max_turns']} assistant turns"
            except HubError:
                pass
        time.sleep(poll)


def _cancel(hub, run_id):
    try:
        hub.post(f"/api/runs/{run_id}/cancel")
    except HubError:
        pass


def run_task(ctx: Ctx, model: dict, task: dict) -> dict:
    res = {"model": f"{model['provider']}/{model['model']}", "task": task["name"], "status": ERROR,
           "note": "", "run_status": None, "tool_calls": 0, "tool_errors": 0, "turns": 0,
           "tokens_in": 0, "tokens_out": 0, "cost_usd": 0.0, "seconds": 0.0, "checks": [],
           "chat_id": None, "sandbox": None}
    hub = ctx.hub
    t0 = time.monotonic()
    sb, chat_id = None, None
    try:
        path = f"{ctx.root}/eval-sandbox/{ctx.tag}/{re.sub(r'[^A-Za-z0-9_.-]', '_', res['model'])}-{task['name']}"
        sb = Sandbox(hub, ctx.cid, ctx.server, path)
        res["sandbox"] = path
        sb.run(f"mkdir -p '{path}'", cwd="/")
        for rel, content in task["setup"].get("files", {}).items():
            sb.write(rel, content)
        prompt = task["prompt"]
        prompt = prompt.replace("{sandbox}", path) if "{sandbox}" in task["prompt"] else \
            f"{prompt}\n\n(Working directory for this task: {path}.)"
        body = {"title": f"eval {task['name']}", "model": model, "clientLabel": ctx.client_label}
        if ctx.profile_id:
            body["profileId"] = ctx.profile_id
        chat = hub.post("/api/chats", body)
        chat_id = res["chat_id"] = chat["id"]
        sent = hub.post(f"/api/chats/{chat_id}/messages", {"content": prompt},
                        headers={"Idempotency-Key": uuid.uuid4().hex})
        run_status, note = wait_run(hub, sent["runId"], chat_id, task, ctx.poll, task["auto_approve"])
        res["run_status"] = run_status
        m = collect_metrics(hub, chat_id)
        names = m.pop("tool_names")
        res.update(m)
        if run_status == "error":
            res["status"], res["note"] = ERROR, f"run failed: {note or 'unknown error'}"
        elif run_status in ("cancelled", "interrupted", "waiting"):
            res["status"], res["note"] = ERROR, f"run {run_status}: {note}".strip()
        elif run_status in ("timeout", "max_turns"):
            res["status"], res["note"] = FAIL, note
        else:
            ok, notes = run_verifier(task, sb, names)
            res["checks"] = notes
            res["status"] = PASS if ok else FAIL
            res["note"] = "" if ok else "; ".join(n for n in notes if n.startswith("FAIL"))
    except (HubError, ToolError, EvalError, KeyError) as e:
        res["status"], res["note"] = ERROR, f"{type(e).__name__}: {e}"
    finally:
        res["seconds"] = round(time.monotonic() - t0, 1)
        if not ctx.keep:
            if chat_id:
                try:
                    hub.delete(f"/api/chats/{chat_id}")
                except HubError:
                    pass
            if sb:
                try:
                    sb.remove()
                except (HubError, ToolError):
                    pass
    return res


# ----------------------------------------------------------------------- output

def _cell(r: dict | None) -> str:
    if r is None:
        return "-"
    if r["status"] == ERROR and not r["tool_calls"]:
        return f"ERROR {r['seconds']:g}s"
    return f"{r['status']} {r['tool_calls']}c {r['seconds']:g}s"


def render_table(results: list[dict], models: list[str] | None = None, tasks: list[str] | None = None) -> str:
    models = models or list(dict.fromkeys(r["model"] for r in results))
    tasks = tasks or list(dict.fromkeys(r["task"] for r in results))
    idx = {(r["model"], r["task"]): r for r in results}
    rows = [["model"] + tasks] + [[m] + [_cell(idx.get((m, t))) for t in tasks] for m in models]
    widths = [max(len(row[i]) for row in rows) for i in range(len(rows[0]))]
    lines = ["  ".join(c.ljust(w) for c, w in zip(row, widths)).rstrip() for row in rows]
    lines.insert(1, "  ".join("-" * w for w in widths))
    return "\n".join(lines) + "\n(cell: status, tool calls `c`, wall seconds)"


def pass_rates(results: list[dict]) -> dict[str, dict]:
    out: dict[str, dict] = {}
    for r in results:
        s = out.setdefault(r["model"], {"pass": 0, "fail": 0, "error": 0, "total": 0, "seconds": 0.0,
                                        "tool_calls": 0, "cost_usd": 0.0})
        s["total"] += 1
        s[r["status"].lower()] += 1
        s["seconds"] += r["seconds"]
        s["tool_calls"] += r["tool_calls"]
        s["cost_usd"] += r["cost_usd"]
    for s in out.values():
        s["rate"] = s["pass"] / s["total"] if s["total"] else 0.0
    return out


def render_summary(results: list[dict]) -> str:
    lines = []
    for model, s in pass_rates(results).items():
        lines.append(f"{model}: {s['pass']}/{s['total']} passed ({s['rate']:.0%}), "
                     f"{s['fail']} fail, {s['error']} error, {s['tool_calls']} tool calls, "
                     f"{s['seconds']:.0f}s" + (f", ${s['cost_usd']:.4f}" if s["cost_usd"] else ""))
    return "\n".join(lines)


def failed_threshold(results: list[dict], min_pass: float) -> list[str]:
    if min_pass > 1:
        min_pass /= 100
    return [m for m, s in pass_rates(results).items() if s["rate"] < min_pass]


# ------------------------------------------------------------------------- main

def load_models(spec: str, path: Path) -> list[dict]:
    if spec == "all":
        try:
            entries = json.loads(path.read_text())["models"]
        except (OSError, KeyError, json.JSONDecodeError) as e:
            raise EvalError(f"cannot read model list {path}: {e}")
        return [{"provider": e["provider"], "model": e["model"]} for e in entries]
    out = []
    for part in filter(None, (p.strip() for p in spec.split(","))):
        if "/" not in part:
            raise EvalError(f"model {part!r} must be provider/model")
        prov, name = part.split("/", 1)
        out.append({"provider": prov, "model": name})
    return out


def pick_connection(conns: list[dict], label: str | None, server: str) -> tuple[dict, str]:
    def harness_of(c):
        for s in c.get("servers", []):
            names = {t.get("name") for t in s.get("tools", [])}
            if s.get("name") == server or "run_command" in names:
                return s.get("name")
        return None
    if label:
        for c in conns:
            if c.get("label") == label:
                h = harness_of(c)
                if not h:
                    raise EvalError(f"client {label!r} has no harness server (no server named {server!r} or with run_command)")
                return c, h
        raise EvalError(f"no connected client labelled {label!r}; connected: {[c.get('label') for c in conns]}")
    for c in conns:
        h = harness_of(c)
        if h:
            return c, h
    raise EvalError("no connected client offers the harness server")


def parse_args(argv=None):
    p = argparse.ArgumentParser(description="Eval the hub's agent loop with real models and tools.")
    p.add_argument("--hub", default=os.environ.get("EVAL_HUB_URL", DEFAULT_HUB))
    p.add_argument("--token", default=os.environ.get("EVAL_HUB_TOKEN"))
    p.add_argument("--client", help="label of the MCP client to use (default: first with a harness server)")
    p.add_argument("--server", default="harness", help="name of the harness server on the client")
    p.add_argument("--models", default="all", help="'all' (llm-models.json) or provider/model,...")
    p.add_argument("--models-file", default=str(Path(__file__).resolve().parent.parent / "llm-models.json"))
    p.add_argument("--tasks", help="comma separated task names (default: all)")
    p.add_argument("--tasks-dir", help="extra task json directory (default evals/tasks)")
    p.add_argument("--profile", help="profile name or id to clone (default: the default profile)")
    p.add_argument("--timeout", type=float, help="override every task's timeout (seconds)")
    p.add_argument("--json", dest="json_out", help="write full results here")
    p.add_argument("--keep", action="store_true", help="keep chats and sandboxes")
    p.add_argument("--with-web", action="store_true", help="also run tasks that need switchboard.web.*")
    p.add_argument("--min-pass", type=float, default=0.0, help="fail (exit 1) if a model's pass rate is lower (0-1)")
    p.add_argument("--dry-run", action="store_true", help="check connectivity and list what would run")
    p.add_argument("--poll", type=float, default=1.0, help=argparse.SUPPRESS)
    return p.parse_args(argv)


def make_profile(hub: Hub, base_ref: str | None, model: dict, tag: str) -> str | None:
    """Clone the base profile with approval=never and the model pinned, so the model
    under test is what runs even though chats reference profiles live."""
    profiles = hub.get("/api/profiles").get("profiles", [])
    base = None
    for pr in profiles:
        if (base_ref and base_ref in (pr["id"], pr["name"])) or (not base_ref and pr.get("isDefault")):
            base = pr
    if base_ref and not base:
        raise EvalError(f"profile {base_ref!r} not found")
    body = {"name": f"eval-{tag}-{model['model']}"[:60], "description": "temporary eval profile",
            "systemPrompt": (base or {}).get("systemPrompt") or "You are a helpful assistant.",
            "model": model, "approval": "never",
            "capabilities": (base or {}).get("capabilities") or {"canSpawn": False, "canMessage": False},
            "budget": (base or {}).get("budget") or {}}
    return hub.post("/api/profiles", body)["id"]


def main(argv=None, out=print) -> int:
    args = parse_args(argv)
    hub = Hub(args.hub, args.token)
    try:
        try:
            hub.get("/api/stats")
        except HubError as e:
            raise EvalError(str(e))
        try:
            avail = {(m["provider"], m["model"]) for m in hub.get("/api/models").get("models", [])}
        except HubError as e:
            raise EvalError(f"orchestrator not available ({e}); is AGENTS_ENABLED on?")
        conn, server = pick_connection(hub.get("/api/connections").get("connections", []), args.client, args.server)
        models = load_models(args.models, Path(args.models_file))
        if not models:
            raise EvalError("no models selected")
        tasks = load_tasks(args.tasks_dir)
        if args.tasks:
            want = [t.strip() for t in args.tasks.split(",") if t.strip()]
            unknown = [w for w in want if w not in {t["name"] for t in tasks}]
            if unknown:
                raise EvalError(f"unknown tasks {unknown}; have {[t['name'] for t in tasks]}")
            tasks = [t for t in tasks if t["name"] in want]
        elif not args.with_web:
            tasks = [t for t in tasks if not t["requires_web"]]
        if args.timeout:
            for t in tasks:
                t["timeout"] = args.timeout
    except (EvalError, TaskError) as e:
        out(f"error: {e}")
        return 2

    out(f"hub {args.hub}; client {conn['label']} (server {server}); {len(models)} model(s) x {len(tasks)} task(s)")
    for m in models:
        out(f"  {m['provider']}/{m['model']}" + ("" if (m["provider"], m["model"]) in avail else "  [NOT available on the hub]"))
    out("  tasks: " + ", ".join(t["name"] for t in tasks))
    if args.dry_run:
        return 0

    tag = uuid.uuid4().hex[:8]
    try:
        root = discover_root(hub, conn["id"], server)
    except (HubError, ToolError, EvalError) as e:
        out(f"error: cannot run commands on the client: {e}")
        return 2
    results: list[dict] = []
    for model in models:
        name = f"{model['provider']}/{model['model']}"
        if (model["provider"], model["model"]) not in avail:
            for t in tasks:
                results.append({"model": name, "task": t["name"], "status": ERROR, "note": "model unavailable on the hub",
                                "run_status": None, "tool_calls": 0, "tool_errors": 0, "turns": 0, "tokens_in": 0,
                                "tokens_out": 0, "cost_usd": 0.0, "seconds": 0.0, "checks": [], "chat_id": None,
                                "sandbox": None})
            continue
        profile_id = None
        try:
            profile_id = make_profile(hub, args.profile, model, tag)
        except (HubError, EvalError) as e:
            out(f"warning: no temporary profile for {name} ({e}); using the default profile and auto-approving")
        ctx = Ctx(hub, conn["id"], server, root, profile_id, tag, keep=args.keep, poll=args.poll)
        ctx.client_label = conn["label"]
        try:
            for t in tasks:
                task = dict(t)
                if profile_id is None:
                    task["auto_approve"] = True
                out(f"running {name} / {t['name']} ...")
                r = run_task(ctx, model, task)
                out(f"  -> {r['status']} {r['note']}".rstrip())
                results.append(r)
        finally:
            if profile_id and not args.keep:
                try:
                    hub.delete(f"/api/profiles/{profile_id}")
                except HubError:
                    pass
    out("")
    out(render_table(results, [f"{m['provider']}/{m['model']}" for m in models], [t["name"] for t in tasks]))
    out("")
    out(render_summary(results))
    if args.json_out:
        Path(args.json_out).write_text(json.dumps({"hub": args.hub, "client": conn["label"], "results": results,
                                                   "summary": pass_rates(results)}, indent=2))
    bad = failed_threshold(results, args.min_pass) if args.min_pass else []
    if bad:
        out(f"below --min-pass {args.min_pass}: {', '.join(bad)}")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
