"""Glue: everything a tool call needs, in one place.

Both call paths - the console (`execute_tool_call`) and MCP consumers
(`call_tool_for_mcp`) - funnel through `_dispatch`, so every call is logged,
measured and exported identically no matter who made it. That is the whole point
of having a hub rather than a plain proxy.
"""

from __future__ import annotations

import logging
import time
import uuid
from typing import Any, AsyncIterator, Dict, Optional, Tuple

import mcp_types as types

from . import protocol
from .calls import CallRecord, CallStore
from .config import Settings
from .loki import LokiExporter
from .metrics import Metrics
from .registry import Connection, Registry, ServerChannel, utcnow

LOGGER = logging.getLogger("mcp_switchboard_hub.service")


class Service:
    def __init__(
        self,
        settings: Settings,
        registry: Registry,
        calls: CallStore,
        metrics: Metrics,
        loki: LokiExporter,
    ) -> None:
        self.settings = settings
        self.registry = registry
        self.calls = calls
        self.metrics = metrics
        self.loki = loki
        self.mcp: Any = None  # set by app.py once the MCP endpoint exists

    # -- lifecycle -----------------------------------------------------

    async def start(self) -> None:
        self.settings.data_dir.mkdir(parents=True, exist_ok=True)
        await self.calls.start()
        await self.loki.start()
        purged = await self.calls.purge()
        if purged:
            LOGGER.info("purged %d call records past retention", purged)
        self.sync_gauges()

    async def close(self) -> None:
        await self.loki.close()
        await self.calls.close()

    # -- views (the console's contract) --------------------------------

    def snapshot(self) -> Dict[str, Any]:
        return self.registry.snapshot()

    def subscribe(self) -> AsyncIterator[Dict[str, Any]]:
        return self.registry.subscribe()

    def sync_gauges(self) -> None:
        connections, by_state = self.registry.counts()
        self.metrics.set_connections(connections)
        self.metrics.set_servers(by_state)
        for connection, channel in self.registry.iter_servers():
            self.metrics.set_tools(connection.label, channel.name, len(channel.tools))

    async def on_topology_change(self) -> None:
        """A tunnel came up, went down, or changed its tool set."""
        self.sync_gauges()
        if self.mcp is not None:
            await self.mcp.broadcast_tools_changed()

    # -- control -------------------------------------------------------

    async def restart_server(self, connection_id: str, server: str) -> None:
        connection = self.registry.get(connection_id)
        if connection is None or server not in connection.servers:
            raise LookupError(f"no server {server!r} on connection {connection_id!r}")
        await connection.send(protocol.restart(server))

    # -- calling -------------------------------------------------------

    async def execute_tool_call(
        self,
        *,
        connection_id: str,
        server: str,
        tool: str,
        arguments: Dict[str, Any],
        source: str = "console",
    ) -> CallRecord:
        connection = self.registry.get(connection_id)
        if connection is None:
            raise LookupError(f"no connection {connection_id!r}")
        channel = connection.servers.get(server)
        if channel is None:
            raise LookupError(f"no server {server!r} on connection {connection_id!r}")
        if not any(t.name == tool for t in channel.tools):
            raise LookupError(f"no tool {tool!r} on {connection.label}/{server}")

        record, _ = await self._dispatch(
            connection=connection,
            channel=channel,
            tool=tool,
            arguments=arguments,
            source=source,
            exposed_name=f"{connection.label}__{server}__{tool}",
        )
        return record

    async def call_tool_for_mcp(
        self,
        *,
        connection: Connection,
        channel: ServerChannel,
        tool: str,
        arguments: Dict[str, Any],
        exposed_name: str,
    ) -> types.CallToolResult:
        _, result = await self._dispatch(
            connection=connection,
            channel=channel,
            tool=tool,
            arguments=arguments,
            source="mcp",
            exposed_name=exposed_name,
        )
        if result is None:
            return types.CallToolResult(
                content=[types.TextContent(type="text", text="the tool call failed before reaching the server")],
                is_error=True,
            )
        return result

    async def _dispatch(
        self,
        *,
        connection: Connection,
        channel: ServerChannel,
        tool: str,
        arguments: Dict[str, Any],
        source: str,
        exposed_name: str,
    ) -> Tuple[CallRecord, Optional[types.CallToolResult]]:
        started = utcnow()
        began = time.perf_counter()
        result: Optional[types.CallToolResult] = None
        serialized: Optional[Dict[str, Any]] = None
        error: Optional[str] = None
        status = "ok"

        if not channel.ready:
            error = f"{connection.label}/{channel.name} is {channel.state}, not connected"
            status = "error"
        else:
            try:
                raw = await channel.session.call_tool(
                    tool, arguments, read_timeout_seconds=self.settings.call_timeout
                )
                if isinstance(raw, types.CallToolResult):
                    result = raw
                    serialized = raw.model_dump(mode="json", by_alias=True, exclude_unset=True)
                    if raw.is_error:
                        status = "error"
                        error = _first_text(raw)
                else:
                    # InputRequiredResult and friends: we have no interactive
                    # channel to satisfy them, so surface it rather than pretend.
                    status = "error"
                    error = f"server returned an unsupported result type: {type(raw).__name__}"
                    serialized = raw.model_dump(mode="json", by_alias=True, exclude_unset=True)
            except Exception as e:  # noqa: BLE001 - a failing tool must not take down the hub
                status = "error"
                error = str(e) or e.__class__.__name__
                LOGGER.warning("tool %s on %s/%s failed: %s", tool, connection.label, channel.name, error)

        duration = time.perf_counter() - began
        record = CallRecord(
            id=uuid.uuid4().hex,
            connection_id=connection.id,
            label=connection.label,
            server=channel.name,
            tool=tool,
            exposed_name=exposed_name,
            arguments=arguments,
            result=serialized,
            error=error,
            status=status,
            source=source,
            started_at=started,
            duration_ms=duration * 1000.0,
        )

        try:
            await self.calls.record(record)
        except Exception as e:  # noqa: BLE001 - logging a call must never fail the call
            LOGGER.warning("could not persist call record: %s", e)

        self.metrics.observe_call(
            label=connection.label,
            server=channel.name,
            tool=tool,
            status=status,
            source=source,
            duration_seconds=duration,
        )
        self.loki.emit(
            "tool_call",
            {
                "callId": record.id,
                "tool": tool,
                "exposedName": exposed_name,
                "status": status,
                "source": source,
                "durationMs": round(record.duration_ms, 3),
                "error": error,
            },
            labels={"connection": connection.label, "server": channel.name},
        )
        self.registry.publish({"type": "call", "call": record.to_json()})

        return record, result


def _first_text(result: types.CallToolResult) -> Optional[str]:
    for item in result.content or []:
        text = getattr(item, "text", None)
        if text:
            return text
    return None
