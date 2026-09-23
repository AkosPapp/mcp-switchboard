"""protocol.py is checked against docs/protocol.json, not against another copy.

Byte-identity between the hub's and the client's copy (hub/tests/test_protocol_sync.py)
only works while both are Python. The Go hub cannot participate in that check, so
docs/protocol.json is the manifest both languages assert against instead: this file
is the Python half, hub/internal/protocol/protocol_test.go is the Go half. Drift in
either direction fails CI (spec.md P1/P2).
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from mcp_switchboard_client import protocol


def manifest() -> dict:
    here = Path(__file__).resolve()
    for candidate in here.parents:
        path = candidate / "docs" / "protocol.json"
        if path.is_file():
            return json.loads(path.read_text(encoding="utf-8"))
    pytest.skip("not running from a source checkout")


MANIFEST = manifest()


def test_constants_match_the_manifest() -> None:
    assert protocol.PROTOCOL_VERSION == MANIFEST["protocolVersion"]
    assert protocol.TUNNEL_PATH == MANIFEST["tunnelPath"]
    assert protocol.NAME_SEPARATOR == MANIFEST["nameSeparator"]


def test_server_states_match_the_manifest() -> None:
    declared = {
        protocol.STATE_STARTING,
        protocol.STATE_RUNNING,
        protocol.STATE_EXITED,
        protocol.STATE_FAILED,
    }
    assert declared == set(MANIFEST["serverStates"])


def test_frame_type_constants_match_the_manifest() -> None:
    declared = {
        protocol.HELLO,
        protocol.HELLO_ACK,
        protocol.MCP,
        protocol.SERVER_STATE,
        protocol.RESTART,
        protocol.ERROR,
    }
    assert declared == set(MANIFEST["frames"])


# One builder call per frame, with arguments chosen so that every optional field is
# populated - a builder that silently dropped a field would otherwise pass.
BUILDERS = {
    "hello": lambda: protocol.hello(
        "c", "0.0.0", "i", "lab", [{"name": "git"}], {"kinds": ["direnv"], "workspace": "/w"}
    ),
    "hello_ack": lambda: protocol.hello_ack("cid", "hub", "0.0.0"),
    "mcp": lambda: protocol.mcp("git", {"jsonrpc": "2.0"}),
    "server_state": lambda: protocol.server_state("git", protocol.STATE_EXITED, 1, "boom"),
    "restart": lambda: protocol.restart("git"),
    "error": lambda: protocol.error("bad", "git"),
}


@pytest.mark.parametrize("frame_type", sorted(MANIFEST["frames"]))
def test_builder_emits_exactly_the_declared_fields(frame_type: str) -> None:
    declared = MANIFEST["frames"][frame_type]
    frame = BUILDERS[frame_type]()

    assert frame["type"] == frame_type
    allowed = set(declared["required"]) | set(declared["optional"])
    assert set(frame) <= allowed, f"{frame_type} emits undeclared fields"
    for field in declared["required"]:
        assert field in frame, f"{frame_type} is missing required field {field}"
        assert frame[field] is not None, f"required field {field} must not be null"


def test_every_builder_is_covered() -> None:
    """A frame added to the manifest without a builder here would not be checked."""
    assert set(BUILDERS) == set(MANIFEST["frames"])


def test_hello_client_optional_fields_are_declared_and_emitted() -> None:
    declared = MANIFEST["frames"]["hello"]["clientOptional"]
    assert "environment" in declared
    client = protocol.hello("c", "0.0.0", "i", "lab", [], {"kinds": []})["client"]
    assert set(client) <= {"name", "version", "instance", "label"} | set(declared)
    assert "environment" in client
