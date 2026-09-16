import json
from pathlib import Path

import pytest

from mcp_reverse_proxy_client.config import ConfigError, load_config


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
