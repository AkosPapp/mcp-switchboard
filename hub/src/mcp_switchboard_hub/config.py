"""Hub settings, from the process environment and an optional .env file.

See envconf.py for the mechanism, including the path indirection that lets any
secret be given either literally or as a path to a file (sops, systemd, etc).
"""

from __future__ import annotations

import logging
from dataclasses import dataclass, field
from pathlib import Path
from typing import Dict, Optional

from . import envconf
from .envconf import ConfigError

LOGGER = logging.getLogger("mcp_switchboard_hub.config")

DEFAULT_TUNNEL_PORT = 8097
DEFAULT_PRIVATE_PORT = 8099


@dataclass
class Settings:
    # The public-facing listener: only the tunnel endpoint lives here, and it
    # always requires a token.
    tunnel_host: str = "127.0.0.1"
    tunnel_port: int = DEFAULT_TUNNEL_PORT
    tunnel_token: str = ""

    # The private listener: console, /api, /mcp and /metrics. Unauthenticated by
    # default because it is meant to bind loopback and never be exposed.
    private_host: str = "127.0.0.1"
    private_port: int = DEFAULT_PRIVATE_PORT
    private_token: Optional[str] = None

    data_dir: Path = Path("/var/lib/mcp-switchboard")
    log_level: str = "INFO"

    call_timeout: float = 120.0
    tools_timeout: float = 30.0

    retention_days: int = 30
    max_rows: int = 100_000

    loki_enabled: bool = False
    loki_url: str = "http://127.0.0.1:3100"
    loki_labels: Dict[str, str] = field(default_factory=lambda: {"service": "mcp-switchboard"})

    # Base URL the console uses when it prints the /mcp endpoint list, so you
    # can copy a URL that actually resolves. Defaults to loopback + the
    # private port; override if the console is reached through something else
    # (an SSH tunnel to a different local port, for example).
    local_base_url: Optional[str] = None

    # The tunnel listener's externally-reachable address (e.g. what a
    # Tailscale Funnel or reverse proxy in front of it answers on). There is
    # no way to derive this from how the hub binds locally, so it is only
    # ever what you configure. Used solely to build the copyable "connect a
    # client" command in the console; unset, that section just explains what
    # to set.
    public_url: Optional[str] = None

    @property
    def db_path(self) -> Path:
        return self.data_dir / "calls.db"

    @property
    def effective_local_base_url(self) -> str:
        return self.local_base_url or f"http://127.0.0.1:{self.private_port}"


def _parse_labels(raw: Optional[str]) -> Dict[str, str]:
    """Parse `k=v,k2=v2` into a dict."""
    if not raw:
        return {}
    labels: Dict[str, str] = {}
    for part in raw.split(","):
        part = part.strip()
        if not part:
            continue
        if "=" not in part:
            raise ConfigError(f"LOKI_LABELS entry {part!r} must be key=value")
        key, _, value = part.partition("=")
        labels[key.strip()] = value.strip()
    return labels


def load_settings(env_file: Optional[Path] = None) -> Settings:
    if env_file is not None:
        envconf.load_env_file(env_file)

    tunnel_token = envconf.get("TUNNEL_TOKEN", secret=True)
    if not tunnel_token:
        raise ConfigError(
            "MCP_SWITCHBOARD_TUNNEL_TOKEN is required - it authenticates the tunnel "
            "listener, which is the one intended to be publicly reachable"
        )

    labels = {"service": "mcp-switchboard"}
    labels.update(_parse_labels(envconf.get("LOKI_LABELS")))

    return Settings(
        tunnel_host=envconf.get("TUNNEL_HOST", "127.0.0.1"),
        tunnel_port=envconf.get_int("TUNNEL_PORT", DEFAULT_TUNNEL_PORT),
        tunnel_token=tunnel_token,
        private_host=envconf.get("PRIVATE_HOST", "127.0.0.1"),
        private_port=envconf.get_int("PRIVATE_PORT", DEFAULT_PRIVATE_PORT),
        private_token=envconf.get("PRIVATE_TOKEN", secret=True),
        data_dir=Path(envconf.get("DATA_DIR", "/var/lib/mcp-switchboard")),
        log_level=envconf.get("LOG_LEVEL", "INFO").upper(),
        call_timeout=envconf.get_float("CALL_TIMEOUT", 120.0),
        tools_timeout=envconf.get_float("TOOLS_TIMEOUT", 30.0),
        retention_days=envconf.get_int("RETENTION_DAYS", 30),
        max_rows=envconf.get_int("MAX_ROWS", 100_000),
        loki_enabled=envconf.get_bool("LOKI_ENABLED", False),
        loki_url=envconf.get("LOKI_URL", "http://127.0.0.1:3100").rstrip("/"),
        loki_labels=labels,
        local_base_url=_strip_or_none(envconf.get("LOCAL_BASE_URL")),
        public_url=_strip_or_none(envconf.get("PUBLIC_URL")),
    )


def _strip_or_none(value: Optional[str]) -> Optional[str]:
    if value is None:
        return None
    value = value.rstrip("/")
    return value or None
