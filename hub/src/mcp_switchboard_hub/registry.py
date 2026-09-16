"""Live state: which clients are connected, what servers they carry, what tools those expose.

Also owns tool-name composition, because how a tool is named is the main way a
consumer (n8n) can tell which machine it lives on.
"""

from __future__ import annotations

import asyncio
import logging
import re
from dataclasses import dataclass, field
from datetime import datetime, timezone
from enum import Enum
from typing import Any, AsyncIterator, Awaitable, Callable, Dict, Iterator, List, Optional, Tuple

from .protocol import NAME_SEPARATOR, STATE_STARTING

LOGGER = logging.getLogger("mcp_switchboard_hub.registry")

# SEP-986. The low-level MCP server does NOT enforce this, so we do.
TOOL_NAME_RE = re.compile(r"^[A-Za-z0-9._-]+$")
MAX_TOOL_NAME = 128

EVENT_QUEUE_SIZE = 256


class Scope(str, Enum):
    """How much of the hub a given MCP endpoint exposes, and how it names tools."""

    ALL = "all"          # {label}__{server}__{tool}
    HOST = "host"        # {server}__{tool}
    SERVER = "server"    # {tool}


def compose_tool_name(scope: Scope, label: str, server: str, tool: str) -> Optional[str]:
    """Build the name a consumer sees, or None if it cannot be made valid.

    Returning None rather than raising keeps one malformed upstream tool from
    taking out the whole tools/list response.
    """
    if not TOOL_NAME_RE.match(tool):
        LOGGER.warning("dropping tool %r from %s/%s: name has characters MCP does not allow", tool, label, server)
        return None

    if scope is Scope.SERVER:
        name = tool
    elif scope is Scope.HOST:
        name = f"{server}{NAME_SEPARATOR}{tool}"
    else:
        name = f"{label}{NAME_SEPARATOR}{server}{NAME_SEPARATOR}{tool}"

    if len(name) > MAX_TOOL_NAME:
        LOGGER.warning(
            "dropping tool %r from %s/%s: composed name is %d chars, over the %d limit",
            tool, label, server, len(name), MAX_TOOL_NAME,
        )
        return None
    return name


@dataclass
class ToolInfo:
    name: str
    title: Optional[str] = None
    description: Optional[str] = None
    input_schema: Dict[str, Any] = field(default_factory=dict)


@dataclass
class ServerChannel:
    """One local MCP server, reached through one connection."""

    name: str
    connection_id: str
    label: str
    command: str = ""
    state: str = STATE_STARTING
    error: Optional[str] = None
    exit_code: Optional[int] = None
    tools: List[ToolInfo] = field(default_factory=list)
    session: Any = None  # mcp.ClientSession once initialize() has completed

    @property
    def ready(self) -> bool:
        return self.session is not None

    def to_json(self, scope: Scope = Scope.ALL) -> Dict[str, Any]:
        tools = []
        for tool in self.tools:
            exposed = compose_tool_name(scope, self.label, self.name, tool.name)
            tools.append(
                {
                    "name": tool.name,
                    "exposedName": exposed,
                    "title": tool.title,
                    "description": tool.description,
                    "inputSchema": tool.input_schema,
                }
            )
        return {
            "name": self.name,
            "command": self.command,
            "state": self.state,
            "error": self.error,
            "exitCode": self.exit_code,
            "toolCount": len(self.tools),
            "tools": tools,
        }


@dataclass
class Connection:
    """One connected client process, carrying any number of servers."""

    id: str
    label: str
    client: Dict[str, Any]
    connected_at: datetime
    send: Callable[[Dict[str, Any]], Awaitable[None]]
    servers: Dict[str, ServerChannel] = field(default_factory=dict)

    def to_json(self) -> Dict[str, Any]:
        return {
            "id": self.id,
            "label": self.label,
            "client": self.client,
            "connectedAt": self.connected_at.isoformat(),
            "servers": [s.to_json() for s in self.servers.values()],
        }


class Registry:
    """The live tree, plus a change feed for the console."""

    def __init__(self) -> None:
        self._connections: Dict[str, Connection] = {}
        self._subscribers: set[asyncio.Queue] = set()

    # -- connections ---------------------------------------------------

    def add_connection(self, connection: Connection) -> None:
        self._connections[connection.id] = connection
        LOGGER.info("connection up: %s (%s)", connection.label, connection.id[:8])
        self.publish_change()

    def remove_connection(self, connection_id: str) -> Optional[Connection]:
        connection = self._connections.pop(connection_id, None)
        if connection is not None:
            LOGGER.info("connection down: %s (%s)", connection.label, connection_id[:8])
            self.publish_change()
        return connection

    def get(self, connection_id: str) -> Optional[Connection]:
        return self._connections.get(connection_id)

    @property
    def connections(self) -> List[Connection]:
        return list(self._connections.values())

    def label_in_use(self, label: str) -> bool:
        return any(c.label == label for c in self._connections.values())

    def iter_servers(self) -> Iterator[Tuple[Connection, ServerChannel]]:
        for connection in list(self._connections.values()):
            for channel in list(connection.servers.values()):
                yield connection, channel

    def find_server(self, connection_id: str, server: str) -> Optional[ServerChannel]:
        connection = self._connections.get(connection_id)
        if connection is None:
            return None
        return connection.servers.get(server)

    def find_by_label(self, label: str, server: str) -> Optional[Tuple[Connection, ServerChannel]]:
        for connection, channel in self.iter_servers():
            if connection.label == label and channel.name == server:
                return connection, channel
        return None

    def resolve_tool(
        self, scope: Scope, exposed_name: str, label: Optional[str] = None, server: Optional[str] = None
    ) -> Optional[Tuple[Connection, ServerChannel, str]]:
        """Map a name a consumer used back to a concrete (connection, server, tool).

        Resolved by re-composing candidate names rather than consulting a cached
        map, so a tunnel that reconnected mid-session can never be resolved
        through a stale entry.
        """
        for connection, channel in self.iter_servers():
            if label is not None and connection.label != label:
                continue
            if server is not None and channel.name != server:
                continue
            for tool in channel.tools:
                if compose_tool_name(scope, connection.label, channel.name, tool.name) == exposed_name:
                    return connection, channel, tool.name
        return None

    # -- change feed ---------------------------------------------------

    def publish(self, event: Dict[str, Any]) -> None:
        """Fan out to subscribers. Never blocks; a slow consumer loses events."""
        for queue in list(self._subscribers):
            try:
                queue.put_nowait(event)
            except asyncio.QueueFull:
                LOGGER.debug("event subscriber is behind, dropping an event")

    def publish_change(self) -> None:
        self.publish({"type": "connections"})

    async def subscribe(self) -> AsyncIterator[Dict[str, Any]]:
        queue: asyncio.Queue = asyncio.Queue(maxsize=EVENT_QUEUE_SIZE)
        self._subscribers.add(queue)
        try:
            while True:
                yield await queue.get()
        finally:
            self._subscribers.discard(queue)

    # -- views ---------------------------------------------------------

    def snapshot(self) -> Dict[str, Any]:
        return {"connections": [c.to_json() for c in self._connections.values()]}

    def counts(self) -> Tuple[int, Dict[str, int]]:
        by_state: Dict[str, int] = {}
        for _, channel in self.iter_servers():
            by_state[channel.state] = by_state.get(channel.state, 0) + 1
        return len(self._connections), by_state


def utcnow() -> datetime:
    return datetime.now(tz=timezone.utc)
