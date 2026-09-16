"""Tunnel protocol v1 frames. Spec: docs/PROTOCOL.md.

This module is duplicated byte-for-byte into mcp_switchboard_client and
mcp_switchboard_hub so that neither package has to depend on the other - the
client deliberately carries no MCP dependency. hub/tests/test_protocol_sync.py
fails if the two copies drift.
"""

from __future__ import annotations

from typing import Any, Dict, List, Optional

PROTOCOL_VERSION = 1

TUNNEL_PATH = "/tunnel/v1"

HELLO = "hello"
HELLO_ACK = "hello_ack"
MCP = "mcp"
SERVER_STATE = "server_state"
RESTART = "restart"
ERROR = "error"

STATE_STARTING = "starting"
STATE_RUNNING = "running"
STATE_EXITED = "exited"
STATE_FAILED = "failed"

# Tools are exposed to consumers as {label}__{server}__{tool}, so neither a
# machine label nor a server name may contain the separator.
NAME_SEPARATOR = "__"


class ProtocolError(Exception):
    """Raised for a frame that cannot be acted on."""


def validate_name(name: str, kind: str) -> str:
    if not name:
        raise ProtocolError(f"{kind} must not be empty")
    if NAME_SEPARATOR in name:
        raise ProtocolError(f"{kind} {name!r} must not contain {NAME_SEPARATOR!r}")
    return name


def hello(
    client_name: str,
    version: str,
    instance: str,
    label: str,
    servers: List[Dict[str, Any]],
) -> Dict[str, Any]:
    return {
        "type": HELLO,
        "protocol": PROTOCOL_VERSION,
        "client": {"name": client_name, "version": version, "instance": instance, "label": label},
        "servers": servers,
    }


def hello_ack(connection_id: str, hub_name: str, hub_version: str) -> Dict[str, Any]:
    return {
        "type": HELLO_ACK,
        "connectionId": connection_id,
        "hub": {"name": hub_name, "version": hub_version},
    }


def mcp(server: str, payload: Any) -> Dict[str, Any]:
    return {"type": MCP, "server": server, "payload": payload}


def server_state(
    server: str,
    state: str,
    exit_code: Optional[int] = None,
    error: Optional[str] = None,
) -> Dict[str, Any]:
    return {
        "type": SERVER_STATE,
        "server": server,
        "state": state,
        "exitCode": exit_code,
        "error": error,
    }


def restart(server: str) -> Dict[str, Any]:
    return {"type": RESTART, "server": server}


def error(message: str, server: Optional[str] = None) -> Dict[str, Any]:
    return {"type": ERROR, "message": message, "server": server}
