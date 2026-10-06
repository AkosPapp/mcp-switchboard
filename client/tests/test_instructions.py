"""W5 (P1-A): instruction files are collected, budgeted, sent in hello, and
refreshed with context_update — without ever leaving the client's own process
guessing (collection never raises; a hub that lacks the frame is unaffected).
"""

import asyncio
import json
from pathlib import Path

import pytest

from mcp_switchboard_client import environment, protocol
from mcp_switchboard_client.config import ServerSpec
from mcp_switchboard_client.tunnel import HubConnection, TunnelSettings

pytestmark = pytest.mark.asyncio


# ---------- collection (pure) ------------------------------------------


def test_find_is_root_first_and_skips_caches(tmp_path: Path):
    (tmp_path / "AGENTS.md").write_text("root\n")
    (tmp_path / ".github").mkdir()
    (tmp_path / ".github" / "copilot-instructions.md").write_text("copilot\n")
    (tmp_path / "src").mkdir()
    (tmp_path / "src" / "AGENTS.md").write_text("deep\n")
    (tmp_path / "src" / ".cursorrules").write_text("rules\n")
    (tmp_path / "node_modules").mkdir()
    (tmp_path / "node_modules" / "AGENTS.md").write_text("junk\n")
    (tmp_path / ".hidden").mkdir()
    (tmp_path / ".hidden" / "AGENTS.md").write_text("junk\n")
    (tmp_path / ".git").mkdir()
    (tmp_path / ".git" / "AGENTS.md").write_text("junk\n")
    # a stray copilot file outside .github does not count
    (tmp_path / "copilot-instructions.md").write_text("no\n")

    found = [p.relative_to(tmp_path).as_posix() for p in environment.find_instruction_files(tmp_path)]
    assert found == [
        "AGENTS.md",
        ".github/copilot-instructions.md",
        "src/.cursorrules",
        "src/AGENTS.md",
    ]


def test_budgets_truncate_and_omit(tmp_path: Path):
    (tmp_path / "AGENTS.md").write_text("x" * (environment.INSTRUCTION_MAX_FILE_CHARS + 500))
    for i in range(environment.INSTRUCTION_MAX_FILES):
        (tmp_path / f"d{i}").mkdir()
        (tmp_path / f"d{i}" / "AGENTS.md").write_text("y" * 10)
    # one more past the file cap
    (tmp_path / "zz").mkdir()
    (tmp_path / "zz" / "AGENTS.md").write_text("past the cap\n")

    got = environment.collect_instructions(tmp_path)
    assert len(got) == environment.INSTRUCTION_MAX_FILES + 1  # 8 files + 1 omission note
    assert got[0]["content"].endswith("characters]") and len(got[0]["content"]) <= environment.INSTRUCTION_MAX_FILE_CHARS
    assert got[-1]["path"] == "(omitted by client budget)"
    assert "zz/AGENTS.md" in got[-1]["content"]
    total = sum(len(g["content"]) for g in got)
    assert total <= environment.INSTRUCTION_MAX_TOTAL_CHARS + 512  # the note is small


def test_collect_never_raises_on_weird_trees(tmp_path: Path):
    (tmp_path / "AGENTS.md").write_bytes(b"\xff\x00invalid utf8")
    got = environment.collect_instructions(tmp_path)  # must not raise
    assert len(got) == 1
    assert "invalid" in got[0]["content"]  # replacement decoding kept the readable part


# ---------- hello / context_update over the fake socket -----------------


class FakeServer:
    def __init__(self, spec, on_stdout=None, on_state=None):
        self.spec = spec
        self.name = spec.name
        self.descriptor = {"name": spec.name, "command": "x"}

    async def start(self):
        pass

    async def stop(self, **_kw):
        pass

    async def restart(self):
        pass

    async def send(self, line):
        pass


class FakeWebSocket:
    def __init__(self, inbound):
        self.inbound = [json.dumps(f) for f in inbound]
        self.sent = []

    async def send(self, raw):
        self.sent.append(json.loads(raw))

    async def close(self):
        self.inbound = []

    def __aiter__(self):
        return self

    async def __anext__(self):
        if not self.inbound:
            raise StopAsyncIteration
        return self.inbound.pop(0)


class FakeConnect:
    def __init__(self, sessions):
        self.sessions = list(sessions)

    def __call__(self, url, **kwargs):
        ws = self.sessions.pop(0)

        class _Ctx:
            async def __aenter__(self):
                return ws

            async def __aexit__(self, *exc):
                await ws.close()
                return False

        return _Ctx()


SPECS = [ServerSpec(name="git", argv=["git-mcp"])]


def make_connection(tmp_path, ws, **overrides):
    root = overrides.pop("instruction_root", tmp_path)
    settings = TunnelSettings(
        hub_url="https://hub.example.com",
        token="t",
        label="box",
        reconnect_delay=0.0,
        max_retries=1,
        instruction_root=str(root) if root is not None else None,
        instructions_interval=overrides.pop("instructions_interval", 0.2),
        **overrides,
    )
    return HubConnection(SPECS, settings, version="0", connect=FakeConnect([ws]), server_factory=FakeServer)


async def test_hello_carries_instruction_files(tmp_path):
    (tmp_path / "AGENTS.md").write_text("root rule\n")
    (tmp_path / "src").mkdir()
    (tmp_path / "src" / "AGENTS.md").write_text("deep rule\n")
    ws = FakeWebSocket([protocol.hello_ack("c1", "hub", "0.1")])
    await make_connection(tmp_path, ws).run()

    instructions = ws.sent[0]["client"]["instructions"]
    assert instructions == [
        {"path": "AGENTS.md", "content": "root rule\n"},
        {"path": "src/AGENTS.md", "content": "deep rule\n"},
    ]


async def test_hello_omits_instructions_when_there_are_none(tmp_path):
    ws = FakeWebSocket([protocol.hello_ack("c1", "hub", "0.1")])
    await make_connection(tmp_path, ws).run()
    assert "instructions" not in ws.sent[0]["client"]


async def test_hello_omits_instructions_when_disabled(tmp_path):
    (tmp_path / "AGENTS.md").write_text("quiet\n")
    ws = FakeWebSocket([protocol.hello_ack("c1", "hub", "0.1")])
    await make_connection(tmp_path, ws, instruction_root=None).run()
    assert "instructions" not in ws.sent[0]["client"]


async def test_refresh_loop_pushes_context_update_on_change(tmp_path):
    (tmp_path / "AGENTS.md").write_text("before\n")
    ws = FakeWebSocket([])
    connection = make_connection(tmp_path, ws, instructions_interval=0.2, env_brief=False)
    sent = []

    async def fake_send(frame):
        sent.append(frame)

    connection._send = fake_send
    connection._instructions_last = connection._collect_instructions()

    task = asyncio.create_task(connection._context_refresh_loop())
    try:
        await asyncio.sleep(0.5)
        assert sent == [], "unchanged files must not churn the wire"

        (tmp_path / "AGENTS.md").write_text("after\n")
        for _ in range(50):
            await asyncio.sleep(0.1)
            if sent:
                break
        assert len(sent) == 1
        assert sent[0]["type"] == protocol.CONTEXT_UPDATE
        assert sent[0]["instructions"] == [{"path": "AGENTS.md", "content": "after\n"}]

        sent.clear()
        (tmp_path / "AGENTS.md").unlink()
        for _ in range(50):
            await asyncio.sleep(0.1)
            if sent:
                break
        assert sent == [{"type": protocol.CONTEXT_UPDATE, "instructions": []}]
    finally:
        task.cancel()


async def test_refresh_loop_stays_quiet_when_disabled(tmp_path):
    ws = FakeWebSocket([])
    connection = make_connection(tmp_path, ws, instruction_root=None, env_brief=False)

    async def fake_send(frame):
        raise AssertionError("must not send")

    connection._send = fake_send
    await asyncio.wait_for(connection._context_refresh_loop(), 0.3)  # returns immediately
