from __future__ import annotations

import json
from pathlib import Path

import pytest

from mcp_switchboard_client import cli, environment, protocol


@pytest.fixture(autouse=True)
def _no_host_container(monkeypatch):
    monkeypatch.setattr(environment, "_in_container", lambda root: False)
    monkeypatch.setattr(environment.sys, "base_prefix", environment.sys.prefix)


def detect(tmp_path, env=None, **kw):
    return environment.detect(cwd=tmp_path, env=env or {}, **kw)


def test_plain_environment(tmp_path):
    result = detect(tmp_path)
    assert result["kinds"] == []
    assert result["workspace"] == str(tmp_path)
    assert result["project"] == tmp_path.name
    assert "details" not in result


def test_container(tmp_path, monkeypatch):
    monkeypatch.setattr(environment, "_in_container", lambda root: True)
    assert detect(tmp_path)["kinds"] == ["container"]


def test_devcontainer_env_and_jsonc_name(tmp_path):
    (tmp_path / ".git").mkdir()
    dc = tmp_path / ".devcontainer"
    dc.mkdir()
    (dc / "devcontainer.json").write_text(
        '// c\n{\n /* x */ "name": "My // Proj",\n "image": "a",\n "list": [1, 2,],\n}\n'
    )
    sub = tmp_path / "src" / "deep"
    sub.mkdir(parents=True)
    result = detect(sub, {"REMOTE_CONTAINERS": "true"})
    assert result["kinds"] == ["devcontainer"]
    assert result["project"] == "My // Proj"
    assert result["details"]["devcontainerName"] == "My // Proj"


def test_devcontainer_via_workspaces_path(tmp_path, monkeypatch):
    monkeypatch.setattr(environment, "_in_container", lambda root: True)
    ws = tmp_path / "workspaces" / "p"
    ws.mkdir(parents=True)
    # cwd does not start with /workspaces/ here, so only "container"
    assert detect(ws)["kinds"] == ["container"]
    monkeypatch.setattr(environment, "find_devcontainer_name", lambda cwd: None)
    result = environment.detect(cwd=Path("/workspaces/p"), env={})
    assert result["kinds"] == ["devcontainer", "container"]


def test_bad_devcontainer_json_falls_back_to_git_root(tmp_path):
    (tmp_path / ".git").write_text("gitdir: x")
    (tmp_path / ".devcontainer.json").write_text("{not json")
    sub = tmp_path / "a"
    sub.mkdir()
    result = detect(sub, {"CODESPACES": "true"})
    assert result["kinds"] == ["devcontainer"]
    assert result["project"] == tmp_path.name


def test_direnv_and_nix(tmp_path):
    result = detect(tmp_path, {"DIRENV_DIR": "-/home/x/p", "IN_NIX_SHELL": "pure", "PATH": "/bin"})
    assert result["kinds"] == ["direnv", "nix-shell"]
    assert result["details"] == {"direnvDir": "/home/x/p", "nixShell": "pure"}


def test_nix_from_path_marker(tmp_path):
    result = detect(tmp_path, {"PATH": "/nix/store/abc-x/bin:/bin"})
    assert result["kinds"] == ["nix-shell"]
    assert "nixShell" not in result.get("details", {})


def test_venv_basename_only(tmp_path):
    result = detect(tmp_path, {"VIRTUAL_ENV": "/secret/place/.venv"})
    assert result["kinds"] == ["venv"]
    assert result["details"] == {"venv": ".venv"}


def test_git_root_project(tmp_path):
    root = tmp_path / "repo"
    (root / ".git").mkdir(parents=True)
    sub = root / "x" / "y"
    sub.mkdir(parents=True)
    assert detect(sub)["project"] == "repo"


def test_override_wins(tmp_path):
    assert detect(tmp_path, project_override=" mine ")["project"] == "mine"


def test_never_raises(tmp_path, monkeypatch):
    def boom(*a, **k):
        raise RuntimeError("x")

    monkeypatch.setattr(environment, "find_git_root", boom)
    assert environment.detect(cwd=tmp_path, env={}) == {"kinds": []}


def test_strip_jsonc_keeps_strings():
    text = '{"a": "x,]", "b": [1,], // t\n}'
    assert json.loads(environment.strip_jsonc(text)) == {"a": "x,]", "b": [1]}


def test_hello_carries_environment():
    env = {"kinds": ["direnv"], "workspace": "/w"}
    assert protocol.hello("c", "1", "i", "l", [], env)["client"]["environment"] == env
    assert "environment" not in protocol.hello("c", "1", "i", "l", [])["client"]


def test_project_name_setting(tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    monkeypatch.setenv("MCP_SWITCHBOARD_HUB_URL", "wss://h.example")
    monkeypatch.setenv("MCP_SWITCHBOARD_TUNNEL_TOKEN", "t")
    s = cli.build_settings([])
    assert s.environment["project"] == tmp_path.name
    monkeypatch.setenv("MCP_SWITCHBOARD_PROJECT_NAME", "fromenv")
    assert cli.build_settings([]).environment["project"] == "fromenv"
    assert cli.build_settings(["--project-name", "flag"]).environment["project"] == "flag"
    assert cli.build_settings([]).tunnel_settings().environment["project"] == "fromenv"
