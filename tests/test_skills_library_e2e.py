"""End-to-end: the client's SKILL.md scan reaches the hub, the hub stores the
library on disk (import, raw round-trip, hand edit wins), the persona seeds,
and the tunnelled harness really exposes the opencode-shaped tool set.

Real hub binary, real client process, real harness — the same scaffolding as
test_end_to_end.py, with the orchestrator on (the skills/prompts API lives
there) and a fake HOME so the scan is deterministic on any machine. No LLM
provider is configured: these are API-level assertions, no run is started.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
import time
from pathlib import Path

import httpx
import pytest

REPO = Path(__file__).resolve().parent.parent

from test_end_to_end import TOKEN, hub_binary, start_hub, stop_hub, wait_for  # noqa: E402,F401 (hub_binary: fixture re-export)

pytestmark = pytest.mark.anyio

SKILL = "---\nname: {name}\ndescription: does {name} things\n---\nDo {name} well.\n"


def _write_skill(root: Path, name: str) -> None:
    d = root / name
    d.mkdir(parents=True)
    (d / "SKILL.md").write_text(SKILL.format(name=name))


@pytest.fixture
async def skill_hub(tmp_path, hub_binary, request):
    hub = await start_hub(hub_binary, tmp_path / "state", MCP_SWITCHBOARD_AGENTS_ENABLED="true")
    try:
        yield hub
    finally:
        log = hub.log_text()
        stop_hub(hub)
        report = getattr(request.node, "report_call", None)
        if report is not None and report.failed:
            print(f"\n--- hub log ---\n{log}")


@pytest.fixture
async def skill_client(tmp_path, skill_hub, request):
    home = tmp_path / "home"
    _write_skill(home / ".claude" / "skills", "pdf")
    project = tmp_path / "proj"
    _write_skill(project / ".opencode" / "skills", "deploy")

    env = dict(os.environ)
    env["HOME"] = str(home)
    env["PYTHONPATH"] = str(REPO / "client" / "src")
    env["PYTHONUNBUFFERED"] = "1"
    proc = subprocess.Popen(
        [
            sys.executable, "-m", "mcp_switchboard_client",
            "--hub-url", f"ws://127.0.0.1:{skill_hub.tunnel_port}",
            "--token", TOKEN,
            "--label", "skillbox",
            "--no-instructions",
            "--log-level", "INFO",
        ],
        cwd=project, env=env,
        stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True,
    )
    try:
        yield proc
    finally:
        proc.terminate()
        try:
            out, _ = proc.communicate(timeout=10)
        except subprocess.TimeoutExpired:
            proc.kill()
            out, _ = proc.communicate()
        report = getattr(request.node, "report_call", None)
        if report is not None and report.failed:
            print(f"\n--- client log ---\n{out}")


async def test_scan_import_disk_library_and_opencode_toolset(skill_hub, skill_client):
    base = skill_hub.base
    async with httpx.AsyncClient(base_url=base, timeout=20.0) as http:

        # 1. The harness arrives under the opencode names; the retired ones stay gone.
        async def harness_up():
            snap = (await http.get("/api/connections")).json()
            for conn in snap["connections"]:
                for srv in conn["servers"]:
                    if srv["name"] == "harness" and srv["toolCount"]:
                        return {t["name"] for t in srv["tools"]}, conn["id"]
            return False

        tools, cid = await wait_for(harness_up, timeout=40.0)
        assert {"bash", "read", "write", "edit", "glob", "grep", "git_status"} <= tools, tools
        assert not tools & {"run_command", "file_read", "file_write", "edit_file", "dir_list",
                            "read_lines", "ripgrep", "find_files", "run_python", "multi_edit",
                            "tree_of_files", "data_query", "run_bash", "list_dir"}

        # 2. The host scan reached the hub: mirror rows in both locations.
        async def hosts():
            body = (await http.get("/api/skills/hosts")).json()["skills"]
            return body if {"pdf", "deploy"} <= {s["name"] for s in body} else False

        scanned = await wait_for(hosts, timeout=30.0)
        pdf = next(s for s in scanned if s["name"] == "pdf")
        assert pdf["host"] == "skillbox" and pdf["source"].startswith("global:")
        assert not pdf["shadowed"]
        deploy = next(s for s in scanned if s["name"] == "deploy")
        assert deploy["source"].startswith("project:")

        # 3. The renamed tools execute over the real tunnel.
        bash = (await http.post(f"/api/connections/{cid}/servers/harness/tools/bash/call",
                                json={"arguments": {"command": "printf hi"}})).json()
        assert "hi" in json.dumps(bash), bash
        await http.post(f"/api/connections/{cid}/servers/harness/tools/write/call",
                        json={"arguments": {"path": "note.txt", "content": "hello library\nsecond line\n"}})
        read = (await http.post(f"/api/connections/{cid}/servers/harness/tools/read/call",
                                json={"arguments": {"path": "note.txt"}})).json()
        assert "1: hello library" in json.dumps(read), read

        # 4. Import copies the mirror into the managed library, as a real file.
        created = (await http.post("/api/skills/import",
                                   json={"host": "skillbox", "name": "pdf"})).json()
        assert created["name"] == "pdf" and created["id"]
        on_disk = skill_hub.data_dir / "skills" / "pdf" / "SKILL.md"

        async def on_file():
            return on_disk.is_file()

        await wait_for(on_file, timeout=10.0)
        assert "Do pdf well." in on_disk.read_text()

        # The mirror is shadowed by the managed copy now.
        body = (await http.get("/api/skills/hosts")).json()["skills"]
        assert next(s for s in body if s["name"] == "pdf")["shadowed"]

        # 5. Raw round-trip, files-as-record: a hand edit on disk wins on the
        #    next read, and unknown frontmatter survives hub rewrites.
        raw = (await http.get(f"/api/skills/{created['id']}/raw")).json()["markdown"]
        assert raw.startswith("---") and "name: pdf" in raw
        time.sleep(0.01)
        on_disk.write_text("---\nname: pdf\ndescription: edited by hand\nlicense: MIT\n---\nnew body\n")

        async def edited():
            skills = (await http.get("/api/skills")).json()["skills"]
            sk = next((s for s in skills if s["name"] == "pdf"), None)
            return sk if sk and sk["description"] == "edited by hand" else False

        await wait_for(edited, timeout=15.0)
        raw = (await http.get(f"/api/skills/{created['id']}/raw")).json()["markdown"]
        # The hub re-renders unknown frontmatter keys as quoted scalars (valid
        # YAML, readable by opencode): the key must survive the rewrite.
        assert 'license: "MIT"' in raw

        # 6. The persona seeded on first start: opencode-flavoured, on disk.
        persona = (await http.get("/api/persona")).json()["persona"]
        assert "opencode" in persona
        assert (skill_hub.data_dir / "prompts" / "base.md").is_file()
