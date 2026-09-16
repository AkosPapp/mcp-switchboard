"""Console-script entry point: ``mcp-reverse-proxy-client``."""

from __future__ import annotations

import argparse
import asyncio
import logging
import os
import signal
import sys
from pathlib import Path

from .config import ConfigError, load_config
from .proxy import DEFAULT_KEEPALIVE_INTERVAL, DEFAULT_MAX_RETRIES, DEFAULT_RECONNECT_DELAY, ReconnectSettings
from .supervisor import run_all

# Mirrors the env var names used by Context Forge's own mcpgateway.reverse_proxy
# client, so a token/URL exported for that tool works unchanged with this one.
ENV_GATEWAY_URL = "REVERSE_PROXY_GATEWAY"
ENV_TOKEN = "REVERSE_PROXY_TOKEN"  # nosec B105 - env var name, not a secret

DEFAULT_CONFIG_NAME = "mcp.json"


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="mcp-reverse-proxy-client",
        description=(
            "Tunnel local MCP stdio servers to an MCP gateway (Context Forge) over "
            "outbound-only, authenticated connections."
        ),
        allow_abbrev=False,
    )
    parser.add_argument(
        "--gateway-url",
        default=None,
        help=f"Gateway URL to tunnel to (or set {ENV_GATEWAY_URL})",
    )
    parser.add_argument(
        "--token",
        default=None,
        help=f"Bearer token for gateway authentication (or set {ENV_TOKEN})",
    )
    parser.add_argument(
        "--config",
        default=None,
        help=f"Path to the MCP server config file (default: ./{DEFAULT_CONFIG_NAME} in the current directory)",
    )
    parser.add_argument(
        "--reconnect-delay",
        type=float,
        default=DEFAULT_RECONNECT_DELAY,
        help=f"Initial reconnect delay in seconds, doubling each attempt up to 60s (default: {DEFAULT_RECONNECT_DELAY})",
    )
    parser.add_argument(
        "--max-retries",
        type=int,
        default=DEFAULT_MAX_RETRIES,
        help=f"Maximum reconnect attempts per server, 0 = infinite (default: {DEFAULT_MAX_RETRIES})",
    )
    parser.add_argument(
        "--keepalive",
        type=float,
        default=DEFAULT_KEEPALIVE_INTERVAL,
        help=f"Heartbeat interval in seconds (default: {DEFAULT_KEEPALIVE_INTERVAL})",
    )
    parser.add_argument(
        "--log-level",
        choices=["DEBUG", "INFO", "WARNING", "ERROR", "CRITICAL"],
        default="INFO",
        help="Log level (default: INFO)",
    )
    return parser


def parse_args(argv=None) -> argparse.Namespace:
    parser = build_parser()
    # argparse already rejects unrecognized arguments with a clear error,
    # which is what we want since the shell installer forwards args blindly.
    args = parser.parse_args(argv)

    if not args.gateway_url:
        args.gateway_url = os.environ.get(ENV_GATEWAY_URL)
    if not args.gateway_url:
        parser.error(f"--gateway-url is required (or set {ENV_GATEWAY_URL})")

    if not args.token:
        args.token = os.environ.get(ENV_TOKEN)
    if not args.token:
        parser.error(f"--token is required (or set {ENV_TOKEN})")

    # --config resolves strictly relative to the current working directory;
    # no upward directory search.
    config_path = Path(args.config) if args.config else Path.cwd() / DEFAULT_CONFIG_NAME
    args.config_path = config_path

    return args


async def _async_main(args: argparse.Namespace) -> int:
    try:
        specs = load_config(args.config_path)
    except ConfigError as e:
        print(f"error: {e}", file=sys.stderr)
        return 1

    settings = ReconnectSettings(
        reconnect_delay=args.reconnect_delay,
        max_retries=args.max_retries,
        keepalive_interval=args.keepalive,
    )

    stop_event = asyncio.Event()
    loop = asyncio.get_running_loop()

    def _request_stop(*_args: object) -> None:
        logging.getLogger("mcp_reverse_proxy_client").info("shutdown signal received")
        stop_event.set()

    for sig in (signal.SIGINT, signal.SIGTERM):
        try:
            loop.add_signal_handler(sig, _request_stop)
        except NotImplementedError:
            # e.g. Windows event loop without signal handler support
            signal.signal(sig, lambda *_a: _request_stop())

    await run_all(specs, args.gateway_url, args.token, settings, stop_event)
    return 0


def main(argv=None) -> None:
    args = parse_args(argv)
    logging.basicConfig(
        level=getattr(logging, args.log_level),
        format="%(asctime)s %(name)s %(levelname)s: %(message)s",
        datefmt="%Y-%m-%dT%H:%M:%S",
        stream=sys.stderr,
    )
    try:
        exit_code = asyncio.run(_async_main(args))
    except KeyboardInterrupt:
        exit_code = 0
    sys.exit(exit_code)


if __name__ == "__main__":
    main()
