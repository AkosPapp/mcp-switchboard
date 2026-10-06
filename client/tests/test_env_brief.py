"""W8: the client-side environment brief — hello carries it, context_update refreshes it."""

import asyncio
import os
import subprocess

from mcp_switchboard_client import environment, protocol

from test_instructions import FakeWebSocket, make_connection  # noqa: E402 - test dir on sys.path


def git(cwd, *args):
    subprocess.run(["git", *args], cwd=cwd, check=True, capture_output=True)


def test_brief_covers_the_host(tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    monkeypatch.setenv("MCP_SWITCHBOARD_HARNESS_SCRATCH", str(tmp_path / "sc"))
    git(tmp_path, "init", "-b", "main", ".")
    git(tmp_path, "config", "user.email", "t@t")
    git(tmp_path, "config", "user.name", "t")
    (tmp_path / "f.txt").write_text("a\n")
    git(tmp_path, "add", "f.txt")
    git(tmp_path, "commit", "-m", "x")
    git(tmp_path, "remote", "add", "origin", "https://user:secret@git.internal/repo.git")
    (tmp_path / "f.txt").write_text("b\n")
    (tmp_path / "u.txt").write_text("u\n")

    brief = environment.collect_environment_brief(tmp_path, instruction_paths=["AGENTS.md"])
    assert f"uid={os.getuid()}" in brief
    assert "host: hostname=" in brief
    assert "branch=main modified=1 untracked=1" in brief
    assert "secret" not in brief  # credentials are redacted, never shipped
    assert "git.internal/repo.git" in brief
    assert str(tmp_path / "sc") in brief
    assert "reachability NOT probed" in brief
    assert "instruction files loaded: AGENTS.md" in brief
    assert "sudo (non-interactive check)" in brief
    assert brief.count("\n") < environment.BRIEF_MAX_LINES


def test_brief_tool_absence_is_stated(tmp_path, monkeypatch):
    monkeypatch.setattr("shutil.which", lambda _name: None)
    brief = environment.collect_environment_brief(tmp_path)
    absent = brief.split("absent:")[1]
    for tool in environment.BRIEF_TOOLS:
        assert tool in absent


def test_brief_sudo_is_never_guessed(tmp_path, monkeypatch):
    def boom(*args, **kwargs):
        raise OSError("no sudo here")

    monkeypatch.setattr(subprocess, "run", boom)
    brief = environment.collect_environment_brief(tmp_path)
    assert "sudo (non-interactive check): unknown" in brief


async def test_hello_carries_brief(tmp_path):
    ws = FakeWebSocket([protocol.hello_ack("c1", "hub", "0.1")])
    await make_connection(tmp_path, ws).run()
    brief = ws.sent[0]["client"]["environment_brief"]
    assert "uid=" in brief


async def test_hello_omits_brief_when_disabled(tmp_path):
    ws = FakeWebSocket([protocol.hello_ack("c1", "hub", "0.1")])
    await make_connection(tmp_path, ws, env_brief=False).run()
    assert "environment_brief" not in ws.sent[0]["client"]


async def test_refresh_pushes_brief_alone_when_files_off(tmp_path, monkeypatch):
    ws = FakeWebSocket([])
    connection = make_connection(
        tmp_path, ws, instruction_root=None, instructions_interval=0.2
    )
    sent = []

    async def fake_send(frame):
        sent.append(frame)

    connection._send = fake_send
    briefs = iter(["first", "second", "third", "fourth"])
    seen_paths = []

    def fake_collect(root, instruction_paths=None):
        seen_paths.append(instruction_paths)
        return next(briefs)

    monkeypatch.setattr(environment, "collect_environment_brief", fake_collect)
    connection._brief_last = connection._collect_brief()  # consumes "first"
    task = asyncio.create_task(connection._context_refresh_loop())
    try:
        for _ in range(50):
            await asyncio.sleep(0.1)
            if sent:
                break
    finally:
        task.cancel()
    update = sent[0]
    assert update["type"] == protocol.CONTEXT_UPDATE
    assert update["environment_brief"] == "second"
    assert "instructions" not in update, "instructions are off: the key must stay absent"
    assert seen_paths and seen_paths[0] is None, "no file paths named when that feature is off"
