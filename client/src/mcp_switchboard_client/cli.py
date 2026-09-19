"""Console-script entry point: ``mcp-switchboard-client``.

Settings come from the environment (prefix ``MCP_SWITCHBOARD_``), optionally
seeded from a .env file, and every one of them can be overridden by a command
line flag. Real environment variables win over the .env file; flags win over
both.
"""

from __future__ import annotations

import argparse
import asyncio
import logging
import signal
import socket
import sys
from dataclasses import dataclass
from pathlib import Path
from typing import List, Optional, Sequence

from . import envconf, protocol
from .config import ConfigError as McpConfigError
from .config import HARNESS_NAME, ServerSpec, harness_spec, load_config
from .envconf import ConfigError
from .tunnel import (
    DEFAULT_MAX_RETRIES,
    DEFAULT_RECONNECT_DELAY,
    HubConnection,
    TunnelError,
    TunnelSettings,
    normalize_hub_url,
)
from . import __version__

PROG = "mcp-switchboard-client"
DEFAULT_CONFIG_NAME = "mcp.json"
DEFAULT_ENV_FILE_NAME = ".env"
DEFAULT_LOG_LEVEL = "INFO"
LOG_LEVELS = ["DEBUG", "INFO", "WARNING", "ERROR", "CRITICAL"]

LOGGER = logging.getLogger("mcp_switchboard_client")


@dataclass
class Settings:
    hub_url: str
    token: str
    label: str
    config_path: Path
    reconnect_delay: float = DEFAULT_RECONNECT_DELAY
    max_retries: int = DEFAULT_MAX_RETRIES
    log_level: str = DEFAULT_LOG_LEVEL
    harness: bool = True

    def tunnel_settings(self) -> TunnelSettings:
        return TunnelSettings(
            hub_url=self.hub_url,
            token=self.token,
            label=self.label,
            reconnect_delay=self.reconnect_delay,
            max_retries=self.max_retries,
        )


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog=PROG,
        description=(
            "Tunnel local stdio MCP servers to an mcp-switchboard hub over a single "
            "outbound, authenticated WebSocket."
        ),
        allow_abbrev=False,
    )
    parser.add_argument("--version", action="version", version=f"{PROG} {__version__}")
    parser.add_argument(
        "--hub-url",
        default=None,
        help=f"Hub URL, e.g. wss://hub.example.com (or {envconf.PREFIX}HUB_URL)",
    )
    parser.add_argument(
        "--token",
        default=None,
        help=(
            f"Tunnel token, or the path to a file holding it "
            f"(or {envconf.PREFIX}TUNNEL_TOKEN)"
        ),
    )
    parser.add_argument(
        "--label",
        default=None,
        help=(
            f"Name this machine is tagged with in the hub, default the hostname "
            f"(or {envconf.PREFIX}LABEL)"
        ),
    )
    parser.add_argument(
        "--config",
        default=None,
        help=(
            f"MCP server config file, resolved against the current directory "
            f"(default ./{DEFAULT_CONFIG_NAME}, or {envconf.PREFIX}CONFIG)"
        ),
    )
    parser.add_argument(
        "--env-file",
        default=None,
        help=f"Env file to load settings from (default ./{DEFAULT_ENV_FILE_NAME} if present)",
    )
    parser.add_argument(
        "--reconnect-delay",
        type=float,
        default=None,
        help=(
            "Initial reconnect delay in seconds, doubling up to 60s "
            f"(default {DEFAULT_RECONNECT_DELAY}, or {envconf.PREFIX}RECONNECT_DELAY)"
        ),
    )
    parser.add_argument(
        "--max-retries",
        type=int,
        default=None,
        help=(
            "Maximum reconnect attempts, 0 = infinite "
            f"(default {DEFAULT_MAX_RETRIES}, or {envconf.PREFIX}MAX_RETRIES)"
        ),
    )
    parser.add_argument(
        "--no-harness",
        action="store_true",
        help=(
            "Do not add the built-in coding harness server (file, search, git and shell "
            f"tools), which is on by default (or set {envconf.PREFIX}HARNESS=false)"
        ),
    )
    parser.add_argument(
        "--log-level",
        choices=LOG_LEVELS,
        default=None,
        help=f"Log level (default {DEFAULT_LOG_LEVEL}, or {envconf.PREFIX}LOG_LEVEL)",
    )
    return parser


def parse_args(argv: Optional[Sequence[str]] = None) -> argparse.Namespace:
    # argparse rejects unknown arguments with a usage message and exit code 2,
    # which is what we want: the shell installer forwards its args blindly.
    return build_parser().parse_args(argv)


def load_settings(args: argparse.Namespace) -> Settings:
    """Resolve flags + env + .env into Settings, or raise ConfigError."""
    env_file = Path(args.env_file) if args.env_file else Path.cwd() / DEFAULT_ENV_FILE_NAME
    if args.env_file and not env_file.is_file():
        raise ConfigError(f"env file not found: {env_file}")
    envconf.load_env_file(env_file)

    hub_url = args.hub_url or envconf.get("HUB_URL")
    if not hub_url:
        raise ConfigError(
            f"no hub URL: pass --hub-url or set {envconf.PREFIX}HUB_URL"
        )
    try:
        hub_url = normalize_hub_url(hub_url)
    except TunnelError as e:
        raise ConfigError(str(e)) from e

    token = args.token or envconf.get("TUNNEL_TOKEN", secret=True)
    if not token:
        raise ConfigError(
            f"no tunnel token: pass --token or set {envconf.PREFIX}TUNNEL_TOKEN "
            "(either the token itself or the path to a file holding it)"
        )

    label = args.label or envconf.get("LABEL") or socket.gethostname()
    try:
        protocol.validate_name(label, "label")
    except protocol.ProtocolError as e:
        raise ConfigError(str(e)) from e

    # The config path is resolved strictly against the current directory;
    # there is no upward search for an mcp.json.
    raw_config = args.config or envconf.get("CONFIG") or DEFAULT_CONFIG_NAME
    config_path = Path(raw_config)
    if not config_path.is_absolute():
        config_path = Path.cwd() / config_path

    reconnect_delay = (
        args.reconnect_delay
        if args.reconnect_delay is not None
        else envconf.get_float("RECONNECT_DELAY", DEFAULT_RECONNECT_DELAY)
    )
    if reconnect_delay < 0:
        raise ConfigError("reconnect delay must not be negative")

    max_retries = (
        args.max_retries
        if args.max_retries is not None
        else envconf.get_int("MAX_RETRIES", DEFAULT_MAX_RETRIES)
    )
    if max_retries < 0:
        raise ConfigError("max retries must not be negative (0 means retry forever)")

    log_level = (args.log_level or envconf.get("LOG_LEVEL") or DEFAULT_LOG_LEVEL).upper()
    if log_level not in LOG_LEVELS:
        raise ConfigError(f"unknown log level {log_level!r} (one of {', '.join(LOG_LEVELS)})")

    return Settings(
        hub_url=hub_url,
        token=token,
        label=label,
        config_path=config_path,
        reconnect_delay=reconnect_delay,
        max_retries=max_retries,
        log_level=log_level,
        harness=not args.no_harness and envconf.get_bool("HARNESS", True),
    )


def build_settings(argv: Optional[Sequence[str]] = None) -> Settings:
    return load_settings(parse_args(argv))


def load_servers(path: Path, harness: bool = False) -> List[ServerSpec]:
    """Load the MCP config, rejecting names the hub could never accept.

    With ``harness``, the built-in coding harness is appended, unless the config
    already defines a server of that name (then the user's entry wins).
    """
    specs = load_config(path)
    if harness and all(spec.name != HARNESS_NAME for spec in specs):
        specs.append(harness_spec())
    for spec in specs:
        try:
            protocol.validate_name(spec.name, "server name")
            if spec.project is not None:
                protocol.validate_name(spec.project, "project name")
        except protocol.ProtocolError as e:
            raise McpConfigError(f"{e} (in {path})") from e
    return specs


def setup_logging(level: str) -> None:
    logging.basicConfig(
        level=getattr(logging, level, logging.INFO),
        format="%(asctime)s %(name)s %(levelname)s: %(message)s",
        datefmt="%Y-%m-%dT%H:%M:%S",
        stream=sys.stderr,
    )


async def _run(settings: Settings, specs: List[ServerSpec]) -> None:
    connection = HubConnection(specs, settings.tunnel_settings(), version=__version__)

    def request_stop() -> None:
        LOGGER.info("shutdown signal received")
        connection.request_stop()

    loop = asyncio.get_running_loop()
    for sig in (signal.SIGINT, signal.SIGTERM):
        try:
            loop.add_signal_handler(sig, request_stop)
        except (NotImplementedError, RuntimeError, ValueError):
            # Windows event loops, and loops not running on the main thread.
            try:
                signal.signal(sig, lambda *_a: request_stop())
            except (ValueError, OSError):
                LOGGER.debug("cannot install a handler for %s", sig)

    LOGGER.info(
        "tunneling %d server(s) as %s: %s",
        len(specs),
        settings.label,
        ", ".join(spec.name for spec in specs),
    )
    await connection.run()
    LOGGER.info("shutdown complete")


def main(argv: Optional[Sequence[str]] = None) -> None:
    args = parse_args(argv)
    try:
        settings = load_settings(args)
    except ConfigError as e:
        print(f"{PROG}: error: {e}", file=sys.stderr)
        sys.exit(1)

    setup_logging(settings.log_level)

    try:
        specs = load_servers(settings.config_path, settings.harness)
    except McpConfigError as e:
        print(f"{PROG}: error: {e}", file=sys.stderr)
        sys.exit(1)

    try:
        asyncio.run(_run(settings, specs))
    except KeyboardInterrupt:
        pass
    except TunnelError as e:
        print(f"{PROG}: error: {e}", file=sys.stderr)
        sys.exit(1)
    sys.exit(0)


if __name__ == "__main__":
    main()
