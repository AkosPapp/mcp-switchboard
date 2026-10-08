"""Client-side skill scanning: discovery locations, frontmatter rules,
budgets, hello shipping and skills_update refresh — mirroring the instruction
file pipeline (docs/PROTOCOL.md)."""

import asyncio
import json

import pytest

from mcp_switchboard_client import environment, protocol
from mcp_switchboard_client.config import ServerSpec
from mcp_switchboard_client.tunnel import HubConnection, TunnelSettings

from test_tunnel import FakeConnect, FakeServer, FakeWebSocket

pytestmark = pytest.mark.asyncio

SKILL_MD = "---\nname: {name}\ndescription: does {name} things\n---\nDo {name} well.\n"


def write_skill(root, name, text=None):
    d = root / name
    d.mkdir(parents=True)
    (d / "SKILL.md").write_text(text if text is not None else SKILL_MD.format(name=name))
    return d


# ---------- collection (pure) ------------------------------------------


def test_global_locations_are_scanned(tmp_path):
    home = tmp_path / "home"
    write_skill(home / ".claude" / "skills", "pdf")
    write_skill(home / ".config" / "opencode" / "skills", "docx")
    write_skill(home / ".agents" / "skills", "pptx")
    found = {s["name"]: s for s in environment.scan_skills(tmp_path / "proj", home)}
    assert set(found) == {"pdf", "docx", "pptx"}
    assert found["pdf"]["source"] == "global:.claude/skills"
    assert found["pdf"]["description"] == "does pdf things"
    assert "Do pdf well." in found["pdf"]["content"]


def test_project_locations_up_to_git_root(tmp_path):
    home = tmp_path / "home"
    home.mkdir()
    repo = tmp_path / "repo"
    (repo / ".git").mkdir(parents=True)
    deep = repo / "sub" / "deeper"
    deep.mkdir(parents=True)
    write_skill(repo / ".claude" / "skills", "repo-skill")
    write_skill(deep / ".opencode" / "skills", "deep-skill")
    outside = tmp_path / "other"
    write_skill(outside / ".claude" / "skills", "outside")
    found = {s["name"] for s in environment.scan_skills(deep, home)}
    assert found == {"repo-skill", "deep-skill"}


def test_invalid_names_and_missing_frontmatter_are_skipped(tmp_path):
    home = tmp_path / "home"
    skills = home / ".claude" / "skills"
    write_skill(skills, "Upper-Case")
    write_skill(skills, "bad--name")
    write_skill(skills, "empty-dir")
    (skills / "empty-dir" / "SKILL.md").unlink()
    (skills / "loose.txt").write_text("x")
    # name in frontmatter must match the directory
    write_skill(skills, "mismatch", "---\nname: other\ndescription: d\n---\nbody\n")
    write_skill(skills, "no-frontmatter", "just text, no frontmatter\n")
    found = {s["name"] for s in environment.scan_skills(tmp_path, home)}
    # no-frontmatter files default the name to the (valid, matching) directory
    assert found == {"no-frontmatter"}


def test_frontmatter_values_are_unquoted(tmp_path):
    home = tmp_path / "home"
    write_skill(
        home / ".claude" / "skills", "quoted",
        '---\nname: quoted\ndescription: "a quoted: description"\nmetadata:\n  audience: maintainers\n---\nbody\nmore body\n',
    )
    (skill,) = environment.scan_skills(tmp_path, home)
    assert skill["description"] == "a quoted: description"
    assert skill["content"].endswith("more body\n")


def test_global_wins_a_name_clash(tmp_path):
    home = tmp_path / "home"
    write_skill(home / ".claude" / "skills", "dup")
    write_skill(tmp_path / "proj" / ".claude" / "skills", "dup")
    found = environment.scan_skills(tmp_path / "proj", home)
    assert [s["source"] for s in found if s["name"] == "dup"] == ["global:.claude/skills"]


def test_oversized_files_are_cut_to_their_budget(tmp_path):
    home = tmp_path / "home"
    skills = home / ".claude" / "skills"
    write_skill(skills, "big", SKILL_MD.format(name="big") + "x" * (environment.SKILL_MAX_FILE_CHARS + 10))
    (found,) = environment.scan_skills(tmp_path, home)
    assert len(found["content"]) <= environment.SKILL_MAX_FILE_CHARS


def test_scan_never_raises(tmp_path):
    assert environment.scan_skills(tmp_path / "does-not-exist", tmp_path / "no-home") == []


# ---------- hello + skills_update wiring --------------------------------


def _skill(name="pdf"):
    return {"name": name, "path": f"{name}/SKILL.md", "source": "global:.claude/skills",
            "description": "d", "content": "body"}


def _make(tmp_path, sessions, **overrides):
    settings = TunnelSettings(
        hub_url="https://hub.example.com", token="s3cret", label="legion5",
        reconnect_delay=0.0, max_retries=1,
        instruction_root=None, env_brief=False,
        **overrides,
    )
    specs = [ServerSpec(name="noop", argv=["true"])]
    connect = FakeConnect(sessions)
    return HubConnection(specs, settings, version="0.4.0", connect=connect, server_factory=FakeServer)


async def test_hello_carries_scanned_skills(tmp_path, monkeypatch):
    monkeypatch.setattr(environment, "scan_skills", lambda cwd=None, home=None: [_skill()])
    ws = FakeWebSocket([protocol.hello_ack("c1", "hub", "1.0")])
    connection = _make(tmp_path, [ws], skills=True)
    await connection.run()
    hello = ws.frames("hello")[0]
    assert hello["client"]["skills"][0]["name"] == "pdf"


async def test_skills_off_omits_the_field(tmp_path, monkeypatch):
    monkeypatch.setattr(environment, "scan_skills", lambda cwd=None, home=None: [])
    ws = FakeWebSocket([protocol.hello_ack("c1", "hub", "1.0")])
    connection = _make(tmp_path, [ws], skills=False)
    await connection.run()
    hello = ws.frames("hello")[0]
    assert "skills" not in hello["client"]


async def test_refresh_loop_sends_skills_update_on_change(tmp_path, monkeypatch):
    scans = [[_skill("a")], [_skill("a"), _skill("b")]]
    monkeypatch.setattr(
        environment, "scan_skills",
        lambda cwd=None, home=None: scans.pop(0) if scans else [_skill("a"), _skill("b")],
    )
    ws = FakeWebSocket([])
    connection = _make(tmp_path, [ws], skills=True, instructions_interval=0.05)
    sent = []

    async def fake_send(frame):
        sent.append(frame)

    connection._send = fake_send
    connection._skills_last = connection._collect_skills()

    task = asyncio.create_task(connection._context_refresh_loop())
    try:
        await asyncio.sleep(0.1)
        assert sent == [], "an unchanged set must not churn the wire"
        for _ in range(100):
            await asyncio.sleep(0.02)
            if sent:
                break
        assert len(sent) == 1
        assert sent[0]["type"] == protocol.SKILLS_UPDATE
        assert [s["name"] for s in sent[0]["skills"]] == ["a", "b"]
    finally:
        task.cancel()
