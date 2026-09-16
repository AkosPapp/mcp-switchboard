"""Settings from the process environment and an optional .env file.

This module is duplicated byte-for-byte into mcp_switchboard_client and
mcp_switchboard_hub so neither package depends on the other.
hub/tests/test_protocol_sync.py fails if the two copies drift.

Path indirection: any value that starts with "/" and resolves to an existing
regular file is replaced by that file's contents. That lets a secret be given
either literally or as a path to a sops/systemd-provisioned file, with no
separate *_FILE variants:

    MCP_SWITCHBOARD_TUNNEL_TOKEN=literal-value
    MCP_SWITCHBOARD_TUNNEL_TOKEN=/run/secrets/mcp-switchboard/tunnel-token

Only regular files are substituted, never directories, so a genuine path value
like MCP_SWITCHBOARD_DATA_DIR=/var/lib/mcp-switchboard survives untouched.
"""

from __future__ import annotations

import os
from pathlib import Path
from typing import Optional

PREFIX = "MCP_SWITCHBOARD_"


class ConfigError(Exception):
    """Raised for a malformed or unusable setting."""


def load_env_file(path: Path) -> None:
    """Populate os.environ from a KEY=VALUE file. Real env vars always win.

    Lines that are blank, start with '#', or contain no '=' are ignored. There
    is no interpolation and no 'export' keyword support - this is intentionally
    minimal, matching the .env files people hand-write for this tool.
    """
    try:
        lines = path.read_text(encoding="utf-8").splitlines()
    except FileNotFoundError:
        return
    except OSError as e:
        raise ConfigError(f"cannot read env file {path}: {e}") from e

    for line in lines:
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, _, value = line.partition("=")
        key = key.strip()
        value = value.strip().strip("'\"")
        if key and key not in os.environ:
            os.environ[key] = value


def resolve(value: str, *, secret: bool = False, name: str = "") -> str:
    """Apply path indirection to a single value."""
    if not value.startswith("/"):
        return value

    path = Path(value)
    if path.is_file():
        try:
            return path.read_text(encoding="utf-8").strip()
        except OSError as e:
            raise ConfigError(f"cannot read {name or 'value'} from {value}: {e}") from e

    if secret:
        # Far more likely to be a secret that failed to materialize than a
        # token that happens to look like an absolute path. Fail loudly rather
        # than authenticating with the literal string "/run/secrets/...".
        raise ConfigError(
            f"{name or 'setting'} looks like a file path ({value}) but no readable file is there"
        )
    return value


def get(name: str, default: Optional[str] = None, *, secret: bool = False) -> Optional[str]:
    """Read PREFIX+name from the environment, with path indirection applied."""
    key = PREFIX + name
    raw = os.environ.get(key)
    if raw is None or raw == "":
        return default
    return resolve(raw, secret=secret, name=key)


def get_int(name: str, default: int) -> int:
    raw = get(name)
    if raw is None:
        return default
    try:
        return int(raw)
    except ValueError as e:
        raise ConfigError(f"{PREFIX}{name} must be an integer, got {raw!r}") from e


def get_float(name: str, default: float) -> float:
    raw = get(name)
    if raw is None:
        return default
    try:
        return float(raw)
    except ValueError as e:
        raise ConfigError(f"{PREFIX}{name} must be a number, got {raw!r}") from e


def get_bool(name: str, default: bool) -> bool:
    raw = get(name)
    if raw is None:
        return default
    lowered = raw.strip().lower()
    if lowered in ("1", "true", "yes", "on"):
        return True
    if lowered in ("0", "false", "no", "off"):
        return False
    raise ConfigError(f"{PREFIX}{name} must be a boolean, got {raw!r}")
