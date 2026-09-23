"""The outbound tunnel: one WebSocket to the hub, every local server on it.

One process opens exactly one connection (``wss://<hub>/tunnel/v1``) and
multiplexes every configured MCP server over it, tagging each frame with the
server name. See docs/PROTOCOL.md - this is the client half of it.

The client is a dumb pipe: it never interprets an MCP payload, it only moves
lines between a subprocess and the socket.

Liveness is WebSocket ping/pong only (20s interval, 10s timeout). There is
deliberately no application-level heartbeat frame; the protocol this replaces
had both sides answer ``heartbeat`` with ``heartbeat``, which is an infinite
loop. Do not add one.
"""

from __future__ import annotations

import asyncio
import json
import logging
import ssl
import uuid
from contextlib import suppress
from dataclasses import dataclass
from typing import Any, Dict, Iterable, List, Optional
from urllib.parse import urlsplit, urlunsplit

import certifi
import websockets
from websockets.exceptions import ConnectionClosed

from . import protocol
from .config import ServerSpec
from .supervisor import LocalServer

LOGGER = logging.getLogger("mcp_switchboard_client.tunnel")

CLIENT_NAME = "mcp-switchboard-client"

DEFAULT_RECONNECT_DELAY = 1.0
DEFAULT_MAX_RETRIES = 0  # 0 = infinite
MAX_BACKOFF_DELAY = 60.0

PING_INTERVAL = 20
PING_TIMEOUT = 10

_WS_SCHEMES = {"ws": "ws", "wss": "wss", "http": "ws", "https": "wss"}


class TunnelError(Exception):
    """Raised for an unusable hub URL or tunnel setting."""


class FatalTunnelError(TunnelError):
    """The hub refused this client; retrying cannot help."""


def _tls_context() -> ssl.SSLContext:
    """A TLS context trusting the system store *and* certifi's CA bundle.

    Built once and reused. This client typically runs under `uvx`, whose
    portable CPython builds frequently can't find a system cert store (NixOS
    keeps its trust root at a nonstandard path), so certifi is the floor that
    makes public certificates verify on any OS. The system store (and
    SSL_CERT_FILE / SSL_CERT_DIR) stays in the mix on purpose: a private or
    corporate CA installed there must keep working, which `cafile=` alone
    would silently drop.
    """
    context = ssl.create_default_context()
    context.load_verify_locations(cafile=certifi.where())
    return context


_TLS_CONTEXT = _tls_context()


def _header_kwarg() -> str:
    """Name of the extra-headers keyword for the installed websockets version.

    websockets >= 14 makes the asyncio client the top-level ``connect`` and
    calls it ``additional_headers``; 12 and 13 still export the legacy client,
    which calls it ``extra_headers``.
    """
    try:
        major = int(str(websockets.__version__).split(".")[0])
    except (AttributeError, ValueError):
        return "additional_headers"
    return "additional_headers" if major >= 14 else "extra_headers"


def normalize_hub_url(url: str) -> str:
    """Turn a user-supplied hub URL into the WebSocket URL to dial.

    ``hub.example.com`` and ``https://hub.example.com`` both become
    ``wss://hub.example.com/tunnel/v1``; an explicit path is left alone, so a
    hub mounted under a prefix can be reached as ``wss://host/prefix/tunnel/v1``.
    """
    raw = (url or "").strip()
    if not raw:
        raise TunnelError("hub URL must not be empty")

    if "://" not in raw:
        raw = "wss://" + raw

    parts = urlsplit(raw)
    scheme = _WS_SCHEMES.get(parts.scheme.lower())
    if scheme is None:
        raise TunnelError(
            f"unsupported hub URL scheme {parts.scheme!r} (use wss://, ws://, https:// or http://)"
        )
    if not parts.netloc:
        raise TunnelError(f"hub URL {url!r} has no host")

    path = parts.path
    if path in ("", "/"):
        path = protocol.TUNNEL_PATH

    return urlunsplit((scheme, parts.netloc, path, parts.query, ""))


@dataclass
class TunnelSettings:
    hub_url: str
    token: str
    label: str
    reconnect_delay: float = DEFAULT_RECONNECT_DELAY
    max_retries: int = DEFAULT_MAX_RETRIES
    # Detected once at startup (see environment.detect); sent in every hello.
    environment: Optional[Dict[str, Any]] = None


class HubConnection:
    """One WebSocket to the hub, multiplexing every local MCP server."""

    def __init__(
        self,
        specs: Iterable[ServerSpec],
        settings: TunnelSettings,
        *,
        version: str = "0.0.0",
        connect: Any = None,
        server_factory: Any = None,
    ) -> None:
        self.settings = settings
        self.url = normalize_hub_url(settings.hub_url)
        self.instance = uuid.uuid4().hex
        self.version = version

        self._connect = connect if connect is not None else websockets.connect
        factory = server_factory if server_factory is not None else LocalServer
        self._servers: Dict[str, Any] = {}
        for spec in specs:
            self._servers[spec.name] = factory(
                spec,
                on_stdout=self._on_server_stdout,
                on_state=self._on_server_state,
            )

        self._ws: Any = None
        self._acked = False
        self._session_ok = False
        self._stopping = False
        self._stop_event: Optional[asyncio.Event] = None

    @property
    def servers(self) -> Dict[str, Any]:
        """The local servers, keyed by name, in config order."""
        return self._servers

    # -- lifecycle -----------------------------------------------------

    async def run(self) -> None:
        """Connect, serve, and reconnect until stopped or out of retries."""
        self._stop_event = asyncio.Event()
        if self._stopping:
            self._stop_event.set()

        attempt = 0
        try:
            while not self._stopping:
                try:
                    await self._connect_and_serve()
                except asyncio.CancelledError:
                    raise
                except FatalTunnelError as e:
                    LOGGER.error("%s", e)
                    break
                except Exception as e:  # noqa: BLE001 - every connection error is retryable
                    LOGGER.error("connection error: %s: %s", type(e).__name__, e)

                if self._stopping:
                    break

                if self._session_ok:
                    # A session that got as far as hello_ack starts backoff over.
                    attempt = 0

                attempt += 1
                if 0 < self.settings.max_retries <= attempt:
                    LOGGER.error(
                        "giving up after %d attempt(s)", self.settings.max_retries
                    )
                    break

                delay = min(
                    self.settings.reconnect_delay * (2 ** (attempt - 1)),
                    MAX_BACKOFF_DELAY,
                )
                LOGGER.info("reconnecting in %.1fs (attempt %d)", delay, attempt)
                if await self._sleep_or_stop(delay):
                    break
        finally:
            await self._stop_servers()

    def request_stop(self) -> None:
        """Ask the tunnel to shut down. Safe to call from a signal handler."""
        self._stopping = True
        if self._stop_event is not None:
            self._stop_event.set()

    async def _sleep_or_stop(self, delay: float) -> bool:
        """Sleep, returning True if a stop was requested instead."""
        assert self._stop_event is not None
        try:
            await asyncio.wait_for(self._stop_event.wait(), timeout=max(delay, 0.0))
        except asyncio.TimeoutError:
            return False
        return True

    async def _stop_servers(self) -> None:
        if not self._servers:
            return
        LOGGER.info("stopping %d local server(s)", len(self._servers))
        results = await asyncio.gather(
            *(server.stop() for server in self._servers.values()),
            return_exceptions=True,
        )
        for server, result in zip(self._servers.values(), results):
            if isinstance(result, BaseException):
                LOGGER.warning("error stopping %s: %s", server.name, result)

    # -- one connection ------------------------------------------------

    async def _connect_and_serve(self) -> None:
        self._acked = False
        self._session_ok = False
        kwargs = {
            _header_kwarg(): {"Authorization": f"Bearer {self.settings.token}"},
            "ping_interval": PING_INTERVAL,
            "ping_timeout": PING_TIMEOUT,
        }
        if urlsplit(self.url).scheme == "wss":
            kwargs["ssl"] = _TLS_CONTEXT

        LOGGER.info("connecting to %s", self.url)
        async with self._connect(self.url, **kwargs) as ws:
            self._ws = ws
            closer = asyncio.create_task(self._close_when_stopped(ws))
            try:
                await self._send(
                    protocol.hello(
                        CLIENT_NAME,
                        self.version,
                        self.instance,
                        self.settings.label,
                        self._descriptors(),
                        self.settings.environment,
                    )
                )
                async for raw in ws:
                    await self._handle_frame(raw)
            except ConnectionClosed as e:
                LOGGER.warning("hub connection closed: %s", e)
            finally:
                closer.cancel()
                with suppress(asyncio.CancelledError):
                    await closer
                self._ws = None
                self._acked = False
                LOGGER.info("tunnel down")

    async def _close_when_stopped(self, ws: Any) -> None:
        assert self._stop_event is not None
        await self._stop_event.wait()
        with suppress(Exception):
            await ws.close()

    def _descriptors(self) -> List[Dict[str, str]]:
        return [server.descriptor for server in self._servers.values()]

    # -- inbound -------------------------------------------------------

    async def _handle_frame(self, raw: Any) -> None:
        if isinstance(raw, (bytes, bytearray)):
            raw = bytes(raw).decode("utf-8", errors="replace")
        try:
            data = json.loads(raw)
        except (json.JSONDecodeError, TypeError):
            LOGGER.error("non-JSON frame from hub, dropping")
            return
        if not isinstance(data, dict):
            LOGGER.error("frame from hub is not an object, dropping")
            return

        frame_type = data.get("type")
        if frame_type == protocol.MCP:
            await self._handle_mcp(data)
        elif frame_type == protocol.RESTART:
            await self._handle_restart(data)
        elif frame_type == protocol.HELLO_ACK:
            await self._handle_hello_ack(data)
        elif frame_type == protocol.ERROR:
            self._handle_error(data)
        else:
            LOGGER.warning("ignoring unknown frame type %r from hub", frame_type)

    async def _handle_hello_ack(self, data: Dict[str, Any]) -> None:
        hub = data.get("hub") or {}
        LOGGER.info(
            "tunnel up: connection %s to %s %s",
            data.get("connectionId"),
            hub.get("name", "hub"),
            hub.get("version", "?"),
        )
        self._acked = True
        self._session_ok = True
        # The hub opens a fresh MCP session per connection, and a server that
        # already completed `initialize` would reject a second one, so every
        # (re)connect restarts every local server. See docs/PROTOCOL.md.
        await self._restart_all()

    def _lookup(self, name: Any) -> Any:
        if not isinstance(name, str):
            return None
        return self._servers.get(name)

    async def _handle_mcp(self, data: Dict[str, Any]) -> None:
        name = data.get("server")
        server = self._lookup(name)
        if server is None:
            LOGGER.warning("mcp frame for unknown server %r, dropping", name)
            return
        payload = data.get("payload")
        if payload is None:
            LOGGER.warning("mcp frame for %r has no payload, dropping", name)
            return
        await server.send(json.dumps(payload))

    async def _handle_restart(self, data: Dict[str, Any]) -> None:
        name = data.get("server")
        server = self._lookup(name)
        if server is None:
            LOGGER.warning("restart requested for unknown server %r", name)
            return
        LOGGER.info("restart requested for %s", name)
        await server.restart()

    def _handle_error(self, data: Dict[str, Any]) -> None:
        message = data.get("message") or "unspecified error"
        server = data.get("server")
        if not self._acked:
            # Before hello_ack an error means the hub refused this client -
            # unsupported protocol version, illegal server name. Retrying with
            # exactly the same hello will never succeed.
            raise FatalTunnelError(f"hub rejected the connection: {message}")
        if server:
            LOGGER.error("hub error for server %s: %s", server, message)
        else:
            LOGGER.error("hub error: %s", message)

    async def _restart_all(self) -> None:
        if not self._servers:
            LOGGER.warning("no local servers configured")
            return
        LOGGER.info("(re)starting %d local server(s)", len(self._servers))
        results = await asyncio.gather(
            *(server.restart() for server in self._servers.values()),
            return_exceptions=True,
        )
        for server, result in zip(self._servers.values(), results):
            if isinstance(result, BaseException):
                LOGGER.error("failed to restart %s: %s", server.name, result)

    # -- outbound ------------------------------------------------------

    async def _on_server_stdout(self, name: str, line: str) -> None:
        try:
            payload = json.loads(line)
        except json.JSONDecodeError:
            LOGGER.warning("%s wrote a non-JSON stdout line, dropping: %s", name, line[:200])
            return
        await self._try_send(protocol.mcp(name, payload))

    async def _on_server_state(
        self,
        name: str,
        state: str,
        exit_code: Optional[int],
        error: Optional[str],
    ) -> None:
        await self._try_send(protocol.server_state(name, state, exit_code, error))

    async def _send(self, frame: Dict[str, Any]) -> None:
        ws = self._ws
        if ws is None:
            raise TunnelError("not connected to the hub")
        await ws.send(json.dumps(frame))

    async def _try_send(self, frame: Dict[str, Any]) -> None:
        """Send a frame, dropping it if the tunnel is down."""
        try:
            await self._send(frame)
        except asyncio.CancelledError:
            raise
        except Exception as e:  # noqa: BLE001 - a dropped frame must not kill a server
            LOGGER.debug("dropping %s frame, tunnel unavailable: %s", frame.get("type"), e)
