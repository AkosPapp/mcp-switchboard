"""Tunnel protocol v1 frames. Spec: docs/PROTOCOL.md.

docs/protocol.json is the normative manifest, asserted from both sides:
client/tests/test_protocol_conformance.py is the Python half and
hub/internal/protocol/protocol_test.go the Go half. Drift in either direction
fails CI. The client deliberately carries no MCP dependency.
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
CONTEXT_UPDATE = "context_update"
SKILLS_UPDATE = "skills_update"

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
    environment: Optional[Dict[str, Any]] = None,
    instructions: Optional[List[Dict[str, str]]] = None,
    environment_brief: Optional[str] = None,
    skills: Optional[List[Dict[str, str]]] = None,
) -> Dict[str, Any]:
    client: Dict[str, Any] = {"name": client_name, "version": version, "instance": instance, "label": label}
    if environment is not None:
        client["environment"] = environment
    if instructions is not None:
        client["instructions"] = instructions
    if environment_brief is not None:
        client["environment_brief"] = environment_brief
    if skills is not None:
        client["skills"] = skills
    return {
        "type": HELLO,
        "protocol": PROTOCOL_VERSION,
        "client": client,
        "servers": servers,
    }


def context_update(
    instructions: Optional[List[Dict[str, str]]] = None,
    environment_brief: Optional[str] = None,
) -> Dict[str, Any]:
    """Replace the hub's copy of the client's instruction files and/or host brief.

    Each present field replaces its hub-side copy wholesale (an empty list or
    string clears it); an absent field is left alone. Sent whenever the client
    re-reads them (files changed / brief refreshed); it is additive (protocol
    version stays 1) and hubs that do not know the frame ignore it.
    """
    frame: Dict[str, Any] = {"type": CONTEXT_UPDATE}
    if instructions is not None:
        frame["instructions"] = instructions
    if environment_brief is not None:
        frame["environment_brief"] = environment_brief
    return frame


def skills_update(skills: List[Dict[str, str]]) -> Dict[str, Any]:
    """Replace the hub's copy of the skills scanned on this host.

    The list is complete (an empty list means the host has none), mirrors the
    context_update wholesale-replacement contract, is additive (protocol
    version stays 1), and hubs that do not know the frame ignore it.
    """
    return {"type": SKILLS_UPDATE, "skills": skills}


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
