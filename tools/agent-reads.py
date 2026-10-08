#!/usr/bin/env python3
"""Rewrite .harness/agent-reads.md from the hub's call log.

Polls GET /api/calls and renders the read-only harness tool calls (the
readOnlyHint set) as a Markdown file a VS Code preview tab reloads on change.
Stdlib only; talk to the loopback hub API directly, never through a tunnel.

Usage: agent-reads.py [--hub URL] [--interval SECONDS] [--out FILE]
                      [--chat ID] [--once]
Auth: $MCP_SWITCHBOARD_PRIVATE_TOKEN is sent as a Bearer token when the hub
has one (the private listener is unauthenticated by default).
"""

import argparse
import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

# The harness's readOnlyHint tools (servers/harness .../server.py TOOLS), split
# by whether the call opens a file or merely searches: grep and glob
# return many paths inside their result, not in the arguments.
OPENED_TOOLS = {"read", "edit", "write", "file_move", "file_delete", "apply_patch"}
SEARCHED_TOOLS = {"grep", "glob"}
READ_TOOLS = OPENED_TOOLS | SEARCHED_TOOLS
MAX_ROWS = 400


def fetch_json(hub, path, token):
    req = urllib.request.Request(hub.rstrip("/") + path)
    if token:
        req.add_header("Authorization", "Bearer " + token)
    with urllib.request.urlopen(req, timeout=10) as resp:
        return json.load(resp)


def workspace_map(hub, token):
    """label -> the client's absolute cwd, to resolve root-relative paths."""
    try:
        conns = fetch_json(hub, "/api/connections", token).get("connections") or []
    except (urllib.error.URLError, OSError, json.JSONDecodeError):
        time.sleep(0.5)
        conns = fetch_json(hub, "/api/connections", token).get("connections") or []
    out = {}
    for c in conns:
        env = c.get("environment") or {}
        if c.get("label") and env.get("workspace"):
            out[c["label"]] = env["workspace"]
    return out


def subject(call):
    """What to show for a call: the path it opened, or what it searched."""
    args = call.get("arguments") or {}
    tool = call.get("tool")
    if tool in ("read", "edit", "write", "file_delete"):
        return str(args.get("path", "?"))
    if tool == "grep":
        return f'"{args.get("pattern", "?")}" in {args.get("path", ".")}'
    if tool == "glob":
        return f'"{args.get("pattern", "?")}" in {args.get("path", ".")}'
    return "?"


def absolute(call, workspaces):
    path = subject(call)
    if not path.startswith("/"):
        ws = workspaces.get(call.get("label") or "")
        if ws:
            path = os.path.join(ws, path)
    return path


def render(rows, hub):
    now = time.strftime("%Y-%m-%d %H:%M:%S UTC", time.gmtime())
    out = [
        "# Agent reads",
        "",
        f"<!-- live: rewritten by tools/agent-reads.py (polling {hub}); newest first, cap {MAX_ROWS} -->",
        f"Updated {now}. Reads appear when the tool returns.",
        "",
        "## Opened",
        "",
        "| time (UTC) | tool | path |",
        "| --- | --- | --- |",
    ]
    for r in rows["opened"][:MAX_ROWS]:
        out.append("| {} | {} | {} |".format(r["when"], r["tool"], r["what"].replace("|", "\\|")))
    out += ["", "## Searched", "", "| time (UTC) | tool | query |", "| --- | --- | --- |"]
    for r in rows["searched"][:MAX_ROWS]:
        out.append("| {} | {} | {} |".format(r["when"], r["tool"], r["what"].replace("|", "\\|")))
    out.append("")
    return "\n".join(out)


def poll_once(args, seen, rows):
    workspaces = workspace_map(args.hub, args.token)
    path = "/api/calls?limit={}".format(args.limit)
    if args.chat:
        path += "&chatId=" + urllib.parse.quote(args.chat, safe="")
    page = fetch_json(args.hub, path, args.token)
    changed = False
    for call in reversed(page.get("calls") or []):
        if call.get("id") in seen or call.get("tool") not in READ_TOOLS:
            continue
        seen.add(call["id"])
        bucket = "opened" if call["tool"] in OPENED_TOOLS else "searched"
        when = (call.get("startedAt") or "")[:19].replace("T", " ")
        what = absolute(call, workspaces) if bucket == "opened" else subject(call)
        if call.get("error"):
            what += " (error)"
        rows[bucket].append({"when": when, "tool": call["tool"], "what": what})
        changed = True
    for bucket in rows:
        rows[bucket].sort(key=lambda r: r["when"], reverse=True)
        del rows[bucket][MAX_ROWS:]
    text = render(rows, args.hub)
    old = None
    if os.path.exists(args.out):
        try:
            with open(args.out, encoding="utf-8") as fh:
                old = fh.read()
        except OSError:
            pass
    if changed or text != old:
        tmp = args.out + ".tmp"
        os.makedirs(os.path.dirname(os.path.abspath(args.out)), exist_ok=True)
        with open(tmp, "w", encoding="utf-8") as fh:
            fh.write(text)
        os.replace(tmp, args.out)


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--hub", default=os.environ.get("MCP_SWITCHBOARD_HUB", "http://127.0.0.1:8099"))
    ap.add_argument("--interval", type=float, default=5.0)
    ap.add_argument("--out", default=".harness/agent-reads.md")
    ap.add_argument("--chat", default="", help="restrict to one chat id")
    ap.add_argument("--limit", type=int, default=200, help="page size per poll")
    ap.add_argument("--once", action="store_true", help="rewrite once and exit")
    args = ap.parse_args()
    args.token = os.environ.get("MCP_SWITCHBOARD_PRIVATE_TOKEN", "")

    seen, rows = set(), {"opened": [], "searched": []}
    while True:
        try:
            poll_once(args, seen, rows)
        except (urllib.error.URLError, OSError, json.JSONDecodeError) as exc:
            print(f"agent-reads: hub unreachable ({exc}); retrying", file=sys.stderr)
        if args.once:
            return
        time.sleep(args.interval)


if __name__ == "__main__":
    main()
