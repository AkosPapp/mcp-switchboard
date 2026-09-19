import json
from pathlib import Path

import pytest

from mcp_switchboard_client.config import ConfigError, load_config


def write_json(path: Path, data: dict) -> Path:
    path.write_text(json.dumps(data))
    return path


def test_plain_command_entry(tmp_path):
    cfg = write_json(
        tmp_path / "mcp.json",
        {"mcpServers": {"git": {"command": "uvx", "args": ["mcp-server-git"], "env": {"X": "1"}}}},
    )
    specs = load_config(cfg)
    assert len(specs) == 1
    assert specs[0].name == "git"
    assert specs[0].argv == ["uvx", "mcp-server-git"]
    assert specs[0].env == {"X": "1"}
    assert specs[0].fastmcp_tempfile is None


def test_fastmcp_entry_discriminated_by_source_key(tmp_path):
    cfg = write_json(
        tmp_path / "mcp.json",
        {
            "mcpServers": {
                "myserver": {
                    "source": {"path": "server.py", "entrypoint": "mcp"},
                    "environment": {"dependencies": ["httpx"]},
                    "deployment": {"transport": "http"},
                }
            }
        },
    )
    specs = load_config(cfg)
    assert len(specs) == 1
    spec = specs[0]
    assert spec.name == "myserver"
    assert spec.argv[:3] == ["uvx", "fastmcp", "run"]
    assert spec.fastmcp_tempfile is not None
    generated = json.loads(spec.fastmcp_tempfile.read_text())
    # transport is forced to stdio even though the entry declared "http"
    assert generated["deployment"]["transport"] == "stdio"
    spec.fastmcp_tempfile.unlink()


def test_bare_fastmcp_config_without_mcpservers_wrapper(tmp_path):
    cfg = write_json(
        tmp_path / "mcp.json",
        {"name": "solo", "source": {"path": "server.py"}, "environment": {}},
    )
    specs = load_config(cfg)
    assert len(specs) == 1
    assert specs[0].name == "solo"
    specs[0].fastmcp_tempfile.unlink()


def test_missing_command_and_source_is_an_error(tmp_path):
    cfg = write_json(tmp_path / "mcp.json", {"mcpServers": {"bad": {}}})
    with pytest.raises(ConfigError):
        load_config(cfg)


def test_missing_file_is_an_error(tmp_path):
    with pytest.raises(ConfigError):
        load_config(tmp_path / "does-not-exist.json")


def test_neither_shape_is_an_error(tmp_path):
    cfg = write_json(tmp_path / "mcp.json", {"unrelated": True})
    with pytest.raises(ConfigError):
        load_config(cfg)


def test_empty_mcp_servers_is_rejected(tmp_path):
    cfg = write_json(tmp_path / "mcp.json", {"mcpServers": {}})
    with pytest.raises(ConfigError, match="non-empty"):
        load_config(cfg)


def test_project_is_read_per_entry_and_from_top_level_default(tmp_path):
    cfg = write_json(
        tmp_path / "mcp.json",
        {
            "project": "nix",
            "mcpServers": {
                "a": {"command": "x"},
                "b": {"command": "y", "project": "other"},
                "c": {"command": "z", "project": "  "},
            },
        },
    )
    projects = {s.name: s.project for s in load_config(cfg)}
    assert projects == {"a": "nix", "b": "other", "c": "nix"}


def test_harness_is_added_by_default_and_can_be_disabled(tmp_path):
    from mcp_switchboard_client.cli import load_servers

    cfg = write_json(tmp_path / "mcp.json", {"mcpServers": {"git": {"command": "x"}}})
    assert [s.name for s in load_servers(cfg, harness=True)] == ["git", "harness"]
    assert [s.name for s in load_servers(cfg, harness=False)] == ["git"]


def test_user_defined_harness_wins(tmp_path):
    from mcp_switchboard_client.cli import load_servers

    cfg = write_json(tmp_path / "mcp.json", {"mcpServers": {"harness": {"command": "mine"}}})
    specs = load_servers(cfg, harness=True)
    assert [(s.name, s.argv) for s in specs] == [("harness", ["mine"])]


def test_missing_default_config_falls_back_to_the_harness_alone(tmp_path):
    from mcp_switchboard_client.cli import load_servers

    absent = tmp_path / "mcp.json"
    assert [s.name for s in load_servers(absent, harness=True, config_required=False)] == ["harness"]
    with pytest.raises(ConfigError, match="not found"):  # an explicitly named file must exist
        load_servers(absent, harness=True, config_required=True)
    with pytest.raises(ConfigError, match="not found"):  # nothing to tunnel without the harness
        load_servers(absent, harness=False, config_required=False)
