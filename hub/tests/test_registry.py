"""Tool-name composition and lookup, including the optional project component."""

from __future__ import annotations

import pytest

from mcp_switchboard_hub.registry import (
    Connection,
    Registry,
    Scope,
    ServerChannel,
    ToolInfo,
    compose_tool_name,
    utcnow,
)


_next_id = iter(range(10_000))


def make_connection(label: str, servers, conn_id=None):
    connection = Connection(
        id=conn_id or f"conn-{label}-{next(_next_id)}",
        label=label,
        client={"name": "mcp-switchboard-client", "version": "0.2.0", "instance": "i1"},
        connected_at=utcnow(),
        send=lambda frame: None,
    )
    for spec in servers:
        name = spec["name"]
        channel = ServerChannel(
            name=name,
            connection_id=connection.id,
            label=label,
            project=spec.get("project"),
            state="running",
            tools=[ToolInfo(name=t, title=None, description=None, input_schema={}) for t in spec.get("tools", ["status"])],
        )
        channel.session = object()  # any non-None value marks it ready
        connection.servers[name] = channel
    return connection


# ------------------------------------------------------------ compose_tool_name


def test_all_scope_without_project():
    assert compose_tool_name(Scope.ALL, "legion5", None, "git", "git_status") == "legion5__git__git_status"


def test_all_scope_with_project():
    assert compose_tool_name(Scope.ALL, "legion5", "nix", "lsp", "hover") == "legion5__nix__lsp__hover"


def test_host_scope_omits_label():
    assert compose_tool_name(Scope.HOST, "legion5", None, "git", "git_status") == "git__git_status"
    assert compose_tool_name(Scope.HOST, "legion5", "nix", "lsp", "hover") == "nix__lsp__hover"


def test_project_scope_omits_label_and_project():
    assert compose_tool_name(Scope.PROJECT, "legion5", "nix", "lsp", "hover") == "lsp__hover"


def test_server_scope_is_bare_tool_name():
    assert compose_tool_name(Scope.SERVER, "legion5", None, "git", "git_status") == "git_status"
    # SERVER scope ignores project - it's for the label+server-fixed legacy endpoint.
    assert compose_tool_name(Scope.SERVER, "legion5", "nix", "lsp", "hover") == "hover"


def test_project_server_scope_is_bare_tool_name():
    assert compose_tool_name(Scope.PROJECT_SERVER, "legion5", "nix", "lsp", "hover") == "hover"


def test_invalid_tool_name_is_dropped():
    assert compose_tool_name(Scope.ALL, "legion5", None, "git", "bad name!") is None


def test_overlength_name_is_dropped():
    long_tool = "t" * 130
    assert compose_tool_name(Scope.ALL, "legion5", None, "git", long_tool) is None


# ------------------------------------------------------------------ resolve_tool


def test_resolve_tool_all_scope():
    registry = Registry()
    registry.add_connection(make_connection("legion5", [{"name": "git", "tools": ["git_status"]}]))

    found = registry.resolve_tool(Scope.ALL, "legion5__git__git_status")
    assert found is not None
    connection, channel, tool_name = found
    assert connection.label == "legion5"
    assert channel.name == "git"
    assert tool_name == "git_status"


def test_hub_rejects_duplicate_server_names_in_one_hello():
    # A single connection keys its servers by name regardless of project, so
    # two projects both wanting e.g. "lsp" need distinct server names - this
    # is enforced in tunnel.py's _accept_hello, exercised there directly.
    from mcp_switchboard_hub.protocol import ProtocolError
    from mcp_switchboard_hub.tunnel import _ClientConnection

    conn = _ClientConnection(websocket=None, registry=Registry(), tools_timeout=1.0, on_change=None, metrics=None)
    frame = {
        "type": "hello",
        "protocol": 1,
        "client": {"label": "legion5"},
        "servers": [
            {"name": "lsp", "project": "nix"},
            {"name": "lsp", "project": "web"},
        ],
    }
    with pytest.raises(ProtocolError, match="duplicate server name"):
        conn._accept_hello(frame)


def test_resolve_tool_respects_project_filter_across_connections():
    # Two different connections (e.g. two machines briefly sharing a label
    # during a reconnect race) can each have an "lsp" server under a
    # different project; the project filter is what tells them apart.
    registry = Registry()
    registry.add_connection(make_connection("legion5", [{"name": "lsp", "project": "nix", "tools": ["hover"]}]))
    registry.add_connection(make_connection("legion5", [{"name": "lsp", "project": "web", "tools": ["hover"]}]))

    found = registry.resolve_tool(Scope.PROJECT_SERVER, "hover", label="legion5", project="nix", server="lsp")
    assert found is not None
    _, channel, _ = found
    assert channel.project == "nix"


def test_resolve_tool_unknown_name_returns_none():
    registry = Registry()
    registry.add_connection(make_connection("legion5", [{"name": "git"}]))
    assert registry.resolve_tool(Scope.ALL, "legion5__git__nope") is None


# --------------------------------------------------------------------- to_json


def test_server_channel_to_json_includes_project():
    channel = ServerChannel(
        name="lsp",
        connection_id="c1",
        label="legion5",
        project="nix",
        state="running",
        tools=[ToolInfo(name="hover", input_schema={})],
    )
    payload = channel.to_json()
    assert payload["project"] == "nix"
    assert payload["tools"][0]["exposedName"] == "legion5__nix__lsp__hover"


def test_server_channel_to_json_project_is_none_when_unset():
    channel = ServerChannel(name="git", connection_id="c1", label="legion5", state="running")
    assert channel.to_json()["project"] is None
