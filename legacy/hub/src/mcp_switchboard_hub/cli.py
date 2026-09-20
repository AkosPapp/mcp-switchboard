"""Console script: ``mcp-switchboard-hub``."""

from __future__ import annotations

import argparse
import asyncio
import logging
import signal
import sys
from pathlib import Path
from typing import List, Optional

import uvicorn

from .app import build_hub
from .config import Settings, load_settings
from .envconf import ConfigError

LOGGER = logging.getLogger("mcp_switchboard_hub")


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="mcp-switchboard-hub",
        description=(
            "Aggregating MCP gateway. Accepts outbound tunnels from machines running local "
            "MCP servers and serves their tools over MCP, with a web console and call log."
        ),
        allow_abbrev=False,
    )
    parser.add_argument("--env-file", default=".env", help="env file to load before reading settings (default: ./.env)")
    parser.add_argument("--tunnel-host", default=None)
    parser.add_argument("--tunnel-port", type=int, default=None)
    parser.add_argument("--private-host", default=None)
    parser.add_argument("--private-port", type=int, default=None)
    parser.add_argument("--data-dir", default=None)
    parser.add_argument(
        "--log-level",
        choices=["DEBUG", "INFO", "WARNING", "ERROR", "CRITICAL"],
        default=None,
    )
    return parser


def settings_from_args(argv: Optional[List[str]] = None) -> Settings:
    args = build_parser().parse_args(argv)

    env_file = Path(args.env_file) if args.env_file else None
    settings = load_settings(env_file)

    # Flags win over the environment.
    if args.tunnel_host:
        settings.tunnel_host = args.tunnel_host
    if args.tunnel_port:
        settings.tunnel_port = args.tunnel_port
    if args.private_host:
        settings.private_host = args.private_host
    if args.private_port:
        settings.private_port = args.private_port
    if args.data_dir:
        settings.data_dir = Path(args.data_dir)
    if args.log_level:
        settings.log_level = args.log_level

    return settings


def check_websocket_support() -> None:
    """Fail loudly if uvicorn has no websocket implementation to use.

    Without one, uvicorn's `ws="auto"` quietly degrades to HTTP-only and serves
    every websocket route as a 404, so tunnels fail with a completely
    misleading error. That is the exact failure mode that made the gateway this
    project replaces unusable, and diagnosing it took reading their source - so
    we refuse to start rather than inflict it on anyone else.
    """
    from importlib.util import find_spec

    if find_spec("websockets") is None and find_spec("wsproto") is None:
        raise ConfigError(
            "no WebSocket implementation is installed, so the tunnel endpoint would "
            "answer every connection with a 404. Install 'websockets' (a declared "
            "dependency of this package) or 'wsproto'."
        )


async def serve(settings: Settings) -> None:
    check_websocket_support()
    hub = build_hub(settings)

    tunnel = uvicorn.Server(
        uvicorn.Config(
            hub.tunnel_app,
            host=settings.tunnel_host,
            port=settings.tunnel_port,
            log_level=settings.log_level.lower(),
        )
    )
    private = uvicorn.Server(
        uvicorn.Config(
            hub.private_app,
            host=settings.private_host,
            port=settings.private_port,
            log_level=settings.log_level.lower(),
        )
    )

    # We own the signals, because two servers each installing their own handlers
    # fight over them.
    for server in (tunnel, private):
        server.install_signal_handlers = lambda: None  # type: ignore[method-assign]

    loop = asyncio.get_running_loop()

    def request_stop(*_args: object) -> None:
        LOGGER.info("shutdown signal received")
        tunnel.should_exit = True
        private.should_exit = True

    for sig in (signal.SIGINT, signal.SIGTERM):
        try:
            loop.add_signal_handler(sig, request_stop)
        except NotImplementedError:  # pragma: no cover - Windows
            signal.signal(sig, lambda *_a: request_stop())

    async with hub.lifespan():
        LOGGER.info(
            "tunnel on %s:%d, console/api/mcp on %s:%d",
            settings.tunnel_host, settings.tunnel_port,
            settings.private_host, settings.private_port,
        )
        await asyncio.gather(tunnel.serve(), private.serve())


def main(argv: Optional[List[str]] = None) -> None:
    try:
        settings = settings_from_args(argv)
    except ConfigError as e:
        print(f"error: {e}", file=sys.stderr)
        sys.exit(2)

    logging.basicConfig(
        level=getattr(logging, settings.log_level, logging.INFO),
        format="%(asctime)s %(name)s %(levelname)s: %(message)s",
        datefmt="%Y-%m-%dT%H:%M:%S",
        stream=sys.stderr,
    )

    try:
        asyncio.run(serve(settings))
    except KeyboardInterrupt:  # pragma: no cover
        pass


if __name__ == "__main__":  # pragma: no cover
    main()
