"""The tunnel endpoint: a client's WebSocket on one side, MCP client sessions on the other.

The client is a dumb pipe carrying raw MCP JSON-RPC for N local servers over one
socket. For each of those servers the hub runs a real `mcp.ClientSession` on top
of an anyio memory stream pair bridged to that server's channel, which is what
gives us initialize/tools-list/call semantics, id correlation and concurrent
in-flight calls without hand-rolling any JSON-RPC.

Lifecycle note: `ClientSession` opens an anyio task group internally, and a task
group must be entered and exited in the same task. So each server gets exactly
one long-lived owner task that opens the session, registers it, parks until the
server goes away, and unregisters on the way out.
"""

from __future__ import annotations

import json
import logging
import uuid
from typing import Any, Awaitable, Callable, Dict, Optional

import anyio
import mcp_types as types
from fastapi import WebSocket, WebSocketDisconnect
from mcp import ClientSession
from mcp.shared.message import SessionMessage

from . import protocol
from .protocol import ProtocolError
from .registry import Connection, Registry, ServerChannel, ToolInfo, utcnow

LOGGER = logging.getLogger("mcp_switchboard_hub.tunnel")

ChangeHook = Callable[[], Awaitable[None]]


class TunnelHandler:
    """Serves /tunnel/v1 for every connecting client."""

    def __init__(
        self,
        registry: Registry,
        *,
        token: str,
        tools_timeout: float = 30.0,
        on_change: Optional[ChangeHook] = None,
        metrics: Any = None,
    ) -> None:
        self.registry = registry
        self.token = token
        self.tools_timeout = tools_timeout
        self.on_change = on_change
        self.metrics = metrics

    def authorized(self, websocket: WebSocket) -> bool:
        header = websocket.headers.get("authorization", "")
        if not header.startswith("Bearer "):
            return False
        # Constant-time compare: this endpoint is the one meant to face the
        # public internet.
        import hmac

        return hmac.compare_digest(header[len("Bearer ") :].strip(), self.token)

    async def handle(self, websocket: WebSocket) -> None:
        if not self.authorized(websocket):
            LOGGER.warning("rejecting tunnel connection: bad or missing token")
            await websocket.close(code=1008, reason="unauthorized")
            return

        await websocket.accept()
        session = _ClientConnection(
            websocket,
            self.registry,
            tools_timeout=self.tools_timeout,
            on_change=self.on_change,
            metrics=self.metrics,
        )
        await session.run()


class _ClientConnection:
    """One connected client process and all of its server channels."""

    def __init__(
        self,
        websocket: WebSocket,
        registry: Registry,
        *,
        tools_timeout: float,
        on_change: Optional[ChangeHook],
        metrics: Any,
    ) -> None:
        self.ws = websocket
        self.registry = registry
        self.tools_timeout = tools_timeout
        self.on_change = on_change
        self.metrics = metrics

        self.connection: Optional[Connection] = None
        self.log = LOGGER
        self._send_lock = anyio.Lock()
        # server name -> the send half of that channel's inbound stream
        self._inbound: Dict[str, Any] = {}
        # server name -> event that retires that server's owner task
        self._stoppers: Dict[str, anyio.Event] = {}

    # -- frame I/O -----------------------------------------------------

    async def send(self, frame: Dict[str, Any]) -> None:
        async with self._send_lock:
            await self.ws.send_text(json.dumps(frame))
        if self.metrics is not None:
            self.metrics.count_frame("out")

    async def run(self) -> None:
        try:
            hello = json.loads(await self.ws.receive_text())
        except (WebSocketDisconnect, json.JSONDecodeError):
            return

        try:
            self._accept_hello(hello)
        except ProtocolError as e:
            self.log.warning("rejecting client: %s", e)
            await self.send(protocol.error(str(e)))
            await self.ws.close(code=1002, reason=str(e))
            return

        assert self.connection is not None
        await self.send(
            protocol.hello_ack(self.connection.id, "mcp-switchboard-hub", _hub_version())
        )
        self.registry.add_connection(self.connection)
        await self._changed()

        try:
            async with anyio.create_task_group() as tg:
                self._tg = tg
                await self._receive_loop()
                tg.cancel_scope.cancel()
        finally:
            await self._teardown()

    def _accept_hello(self, frame: Dict[str, Any]) -> None:
        if frame.get("type") != protocol.HELLO:
            raise ProtocolError(f"expected a {protocol.HELLO!r} frame first")

        version = frame.get("protocol")
        if version != protocol.PROTOCOL_VERSION:
            raise ProtocolError(
                f"unsupported protocol version {version!r}; this hub speaks {protocol.PROTOCOL_VERSION}"
            )

        client = frame.get("client") or {}
        label = str(client.get("label") or "").strip()
        protocol.validate_name(label, "label")

        if self.registry.label_in_use(label):
            # Two machines claiming one label would make tools ambiguous and
            # silently shadow each other in the aggregated list.
            raise ProtocolError(f"label {label!r} is already connected")

        connection = Connection(
            id=uuid.uuid4().hex,
            label=label,
            client={
                "name": client.get("name"),
                "version": client.get("version"),
                "instance": client.get("instance"),
            },
            connected_at=utcnow(),
            send=self.send,
        )

        for spec in frame.get("servers") or []:
            name = str(spec.get("name") or "").strip()
            protocol.validate_name(name, "server name")
            connection.servers[name] = ServerChannel(
                name=name,
                connection_id=connection.id,
                label=label,
                command=str(spec.get("command") or ""),
            )

        if not connection.servers:
            raise ProtocolError("hello listed no servers")

        self.connection = connection
        self.log = logging.getLogger(f"mcp_switchboard_hub.tunnel.{label}")

    # -- main loop -----------------------------------------------------

    async def _receive_loop(self) -> None:
        while True:
            try:
                raw = await self.ws.receive_text()
            except WebSocketDisconnect:
                return
            except RuntimeError:
                # Starlette raises this if the socket is already closing.
                return

            if self.metrics is not None:
                self.metrics.count_frame("in")

            try:
                frame = json.loads(raw)
            except json.JSONDecodeError:
                self.log.warning("dropping non-JSON frame from client")
                continue

            kind = frame.get("type")
            if kind == protocol.MCP:
                await self._on_mcp(frame)
            elif kind == protocol.SERVER_STATE:
                await self._on_server_state(frame)
            else:
                self.log.warning("ignoring unknown frame type %r", kind)

    async def _on_mcp(self, frame: Dict[str, Any]) -> None:
        server = frame.get("server")
        stream = self._inbound.get(server)
        if stream is None:
            # Normal during startup/shutdown races; the server's session either
            # has not opened yet or has already retired.
            self.log.debug("no open session for %r, dropping inbound message", server)
            return
        try:
            message = types.jsonrpc_message_adapter.validate_json(
                json.dumps(frame.get("payload")), by_name=False
            )
        except Exception as e:  # noqa: BLE001 - a malformed payload must not kill the tunnel
            self.log.warning("unparseable MCP payload from %r: %s", server, e)
            return
        try:
            await stream.send(SessionMessage(message))
        except anyio.BrokenResourceError:
            self.log.debug("session for %r closed while delivering a message", server)

    async def _on_server_state(self, frame: Dict[str, Any]) -> None:
        assert self.connection is not None
        name = frame.get("server")
        channel = self.connection.servers.get(name)
        if channel is None:
            self.log.warning("state for unknown server %r", name)
            return

        channel.state = frame.get("state") or protocol.STATE_FAILED
        channel.error = frame.get("error")
        channel.exit_code = frame.get("exitCode")
        self.log.info("server %s is %s%s", name, channel.state, f" ({channel.error})" if channel.error else "")

        if channel.state == protocol.STATE_RUNNING:
            await self._start_channel(channel)
        else:
            await self._stop_channel(channel)

        self.registry.publish_change()
        await self._changed()

    # -- per-server MCP sessions ---------------------------------------

    async def _start_channel(self, channel: ServerChannel) -> None:
        if channel.name in self._stoppers:
            return  # already owned
        stopper = anyio.Event()
        self._stoppers[channel.name] = stopper
        self._tg.start_soon(self._own_channel, channel, stopper)

    async def _stop_channel(self, channel: ServerChannel) -> None:
        stopper = self._stoppers.pop(channel.name, None)
        if stopper is not None:
            stopper.set()

    async def _own_channel(self, channel: ServerChannel, stopper: anyio.Event) -> None:
        """Own one ClientSession for its whole life. Enter and exit in THIS task."""
        read_w, read_stream = anyio.create_memory_object_stream[SessionMessage | Exception](0)
        write_stream, write_r = anyio.create_memory_object_stream[SessionMessage](0)
        self._inbound[channel.name] = read_w

        async def pump_to_client() -> None:
            async for outgoing in write_r:
                payload = json.loads(outgoing.message.model_dump_json(by_alias=True, exclude_unset=True))
                await self.send(protocol.mcp(channel.name, payload))

        try:
            async with anyio.create_task_group() as tg:
                tg.start_soon(pump_to_client)
                async with ClientSession(read_stream, write_stream) as session:
                    with anyio.fail_after(self.tools_timeout):
                        await session.initialize()
                    channel.session = session
                    await self._refresh_tools(channel)
                    self.log.info("session up for %s (%d tools)", channel.name, len(channel.tools))
                    self.registry.publish_change()
                    await self._changed()
                    await stopper.wait()
                tg.cancel_scope.cancel()
        except TimeoutError:
            channel.error = "timed out waiting for the server to answer initialize"
            channel.state = protocol.STATE_FAILED
            self.log.warning("server %s did not complete initialize in %.0fs", channel.name, self.tools_timeout)
        except Exception as e:  # noqa: BLE001 - one bad server must not drop the connection
            channel.error = str(e)
            channel.state = protocol.STATE_FAILED
            self.log.warning("session for %s ended: %s", channel.name, e)
        finally:
            self._inbound.pop(channel.name, None)
            channel.session = None
            channel.tools = []
            with anyio.CancelScope(shield=True):
                await read_w.aclose()
                self.registry.publish_change()
                await self._changed()

    async def _refresh_tools(self, channel: ServerChannel) -> None:
        try:
            with anyio.fail_after(self.tools_timeout):
                result = await channel.session.list_tools()
        except Exception as e:  # noqa: BLE001
            self.log.warning("tools/list failed for %s: %s", channel.name, e)
            channel.tools = []
            return

        channel.tools = [
            ToolInfo(
                name=tool.name,
                title=tool.title,
                description=tool.description,
                input_schema=tool.input_schema or {},
            )
            for tool in result.tools
        ]

    # -- shutdown ------------------------------------------------------

    async def _teardown(self) -> None:
        for stopper in list(self._stoppers.values()):
            stopper.set()
        self._stoppers.clear()
        if self.connection is not None:
            self.registry.remove_connection(self.connection.id)
            if self.metrics is not None:
                for channel in self.connection.servers.values():
                    self.metrics.clear_tools(self.connection.label, channel.name)
        with anyio.CancelScope(shield=True):
            await self._changed()

    async def _changed(self) -> None:
        if self.on_change is not None:
            try:
                await self.on_change()
            except Exception as e:  # noqa: BLE001
                self.log.debug("change hook failed: %s", e)


def _hub_version() -> str:
    try:
        from importlib.metadata import version

        return version("mcp-switchboard-hub")
    except Exception:  # noqa: BLE001 - running from a source checkout
        return "0.0.0+dev"
