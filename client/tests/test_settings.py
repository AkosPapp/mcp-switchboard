"""Flags vs. environment vs. .env, and hub URL normalization."""

from pathlib import Path

import pytest

from mcp_switchboard_client.cli import build_settings
from mcp_switchboard_client.envconf import ConfigError
from mcp_switchboard_client.tunnel import TunnelError, normalize_hub_url

BASE = ["--hub-url", "wss://hub.example.com", "--token", "t0ken"]


def write_env(tmp_path: Path, body: str) -> Path:
    path = tmp_path / "test.env"
    path.write_text(body)
    return path


def test_settings_from_env_file(tmp_path):
    env_file = write_env(
        tmp_path,
        "MCP_SWITCHBOARD_HUB_URL=wss://hub.example.com\n"
        "MCP_SWITCHBOARD_TUNNEL_TOKEN=from-env-file\n"
        "MCP_SWITCHBOARD_LABEL=laptop\n"
        "MCP_SWITCHBOARD_MAX_RETRIES=3\n"
        "MCP_SWITCHBOARD_RECONNECT_DELAY=0.25\n"
        "MCP_SWITCHBOARD_LOG_LEVEL=debug\n"
        "# a comment\n"
        "\n",
    )
    settings = build_settings(["--env-file", str(env_file)])
    assert settings.hub_url == "wss://hub.example.com/tunnel/v1"
    assert settings.token == "from-env-file"
    assert settings.label == "laptop"
    assert settings.max_retries == 3
    assert settings.reconnect_delay == 0.25
    assert settings.log_level == "DEBUG"


def test_real_env_beats_env_file(tmp_path, monkeypatch):
    env_file = write_env(
        tmp_path,
        "MCP_SWITCHBOARD_HUB_URL=wss://from-file.example.com\n"
        "MCP_SWITCHBOARD_TUNNEL_TOKEN=from-file\n",
    )
    monkeypatch.setenv("MCP_SWITCHBOARD_TUNNEL_TOKEN", "from-environment")

    settings = build_settings(["--env-file", str(env_file)])
    assert settings.hub_url == "wss://from-file.example.com/tunnel/v1"
    assert settings.token == "from-environment"


def test_flags_beat_everything(tmp_path, monkeypatch):
    env_file = write_env(
        tmp_path,
        "MCP_SWITCHBOARD_HUB_URL=wss://from-file.example.com\n"
        "MCP_SWITCHBOARD_TUNNEL_TOKEN=from-file\n",
    )
    monkeypatch.setenv("MCP_SWITCHBOARD_TUNNEL_TOKEN", "from-environment")
    monkeypatch.setenv("MCP_SWITCHBOARD_MAX_RETRIES", "9")

    settings = build_settings(
        [
            "--env-file",
            str(env_file),
            "--hub-url",
            "wss://from-flag.example.com",
            "--token",
            "from-flag",
            "--max-retries",
            "2",
        ]
    )
    assert settings.hub_url == "wss://from-flag.example.com/tunnel/v1"
    assert settings.token == "from-flag"
    assert settings.max_retries == 2


def test_token_path_indirection(tmp_path, monkeypatch):
    secret = tmp_path / "tunnel-token"
    secret.write_text("sops-provisioned\n")
    monkeypatch.setenv("MCP_SWITCHBOARD_TUNNEL_TOKEN", str(secret))
    monkeypatch.setenv("MCP_SWITCHBOARD_HUB_URL", "wss://hub.example.com")

    settings = build_settings([])
    assert settings.token == "sops-provisioned"


def test_token_path_that_does_not_exist_is_fatal(monkeypatch):
    monkeypatch.setenv("MCP_SWITCHBOARD_TUNNEL_TOKEN", "/run/secrets/nope/token")
    monkeypatch.setenv("MCP_SWITCHBOARD_HUB_URL", "wss://hub.example.com")

    with pytest.raises(ConfigError):
        build_settings([])


def test_missing_hub_url_and_token_are_fatal(tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)  # no ./.env here
    with pytest.raises(ConfigError, match="hub URL"):
        build_settings([])
    with pytest.raises(ConfigError, match="token"):
        build_settings(["--hub-url", "wss://hub.example.com"])


def test_missing_explicit_env_file_is_fatal(tmp_path):
    with pytest.raises(ConfigError, match="env file not found"):
        build_settings(BASE + ["--env-file", str(tmp_path / "absent.env")])


def test_label_defaults_to_hostname(tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    monkeypatch.setattr("socket.gethostname", lambda: "legion5")
    assert build_settings(BASE).label == "legion5"


def test_label_with_separator_is_rejected(tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    with pytest.raises(ConfigError, match="__"):
        build_settings(BASE + ["--label", "my__host"])


def test_config_path_is_relative_to_cwd_only(tmp_path, monkeypatch):
    nested = tmp_path / "work"
    nested.mkdir()
    (tmp_path / "mcp.json").write_text("{}")  # would be found by an upward search
    monkeypatch.chdir(nested)

    settings = build_settings(BASE)
    assert settings.config_path == nested / "mcp.json"
    assert not settings.config_path.exists()


def test_config_flag_accepts_absolute_path(tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    target = tmp_path / "elsewhere" / "servers.json"
    settings = build_settings(BASE + ["--config", str(target)])
    assert settings.config_path == target


def test_bad_numeric_env_value_is_fatal(tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    monkeypatch.setenv("MCP_SWITCHBOARD_MAX_RETRIES", "lots")
    with pytest.raises(ConfigError):
        build_settings(BASE)


@pytest.mark.parametrize(
    "given,expected",
    [
        ("https://hub.example.com", "wss://hub.example.com/tunnel/v1"),
        ("http://localhost:8000", "ws://localhost:8000/tunnel/v1"),
        ("wss://hub.example.com", "wss://hub.example.com/tunnel/v1"),
        ("wss://hub.example.com/", "wss://hub.example.com/tunnel/v1"),
        ("hub.example.com", "wss://hub.example.com/tunnel/v1"),
        ("  https://hub.example.com  ", "wss://hub.example.com/tunnel/v1"),
        # An explicit path is preserved: the hub may be mounted under a prefix.
        ("https://hub.example.com/switchboard/tunnel/v1", "wss://hub.example.com/switchboard/tunnel/v1"),
        ("wss://hub.example.com/custom", "wss://hub.example.com/custom"),
        ("https://user:pw@hub.example.com", "wss://user:pw@hub.example.com/tunnel/v1"),
        ("https://hub.example.com?x=1", "wss://hub.example.com/tunnel/v1?x=1"),
        ("https://hub.example.com/p#frag", "wss://hub.example.com/p"),
    ],
)
def test_normalize_hub_url(given, expected):
    assert normalize_hub_url(given) == expected


@pytest.mark.parametrize("given", ["", "   ", "ftp://hub.example.com", "https://"])
def test_normalize_hub_url_rejects_junk(given):
    with pytest.raises(TunnelError):
        normalize_hub_url(given)


def test_settings_normalizes_the_hub_url(tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    settings = build_settings(["--hub-url", "https://hub.example.com", "--token", "t"])
    assert settings.hub_url == "wss://hub.example.com/tunnel/v1"
    assert settings.tunnel_settings().hub_url == settings.hub_url


def test_harness_defaults_on_and_can_be_switched_off(monkeypatch):
    assert build_settings(BASE).harness is True
    assert build_settings(BASE + ["--no-harness"]).harness is False
    monkeypatch.setenv("MCP_SWITCHBOARD_HARNESS", "false")
    assert build_settings(BASE).harness is False
