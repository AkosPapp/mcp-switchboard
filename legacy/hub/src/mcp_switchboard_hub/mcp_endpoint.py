"""The consumer-facing MCP server: one aggregated view of every tunneled tool.

This is what n8n (or any MCP client) connects to. It is served over Streamable
HTTP at several scopes, so a consumer can take everything or narrow to one
machine, one project on that machine, or one server (optionally within a
project):

    /mcp                                                    {label}__[{project}__]{server}__{tool}
    /mcp/host/{label}                                        [{project}__]{server}__{tool}
    /mcp/host/{label}/project/{project}                       {server}__{tool}
    /mcp/host/{label}/server/{server}                         {tool}
    /mcp/host/{label}/project/{project}/server/{server}       {tool}

A server's project is optional (set via `"project"` in its `mcp.json` entry),
so the bracketed component above is simply omitted for a project-less server.

All five are backed by a single `Server` and a single session manager; the scope
is derived per request from the path params on `ctx.request`, which the transport
attaches for us. That avoids having to create (and correctly tear down) a session
manager per machine as tunnels come and go.
"""

from __future__ import annotations

import logging
from typing import TYPE_CHECKING, Any, Dict, Optional, Set, Tuple

import mcp_types as types
from mcp.server.lowlevel import NotificationOptions, Server
from mcp.server.streamable_http_manager import StreamableHTTPASGIApp, StreamableHTTPSessionManager
from starlette.routing import Route

from .registry import Registry, Scope, compose_tool_name

if TYPE_CHECKING:  # pragma: no cover
    from .service import Service

LOGGER = logging.getLogger("mcp_switchboard_hub.mcp")


class HubServer(Server):
    """Force `tools.listChanged` on.

    The session manager builds initialization options by calling this with no
    arguments, which would otherwise advertise listChanged=false - fatal here,
    since our whole tool list changes as tunnels come and go.
    """

    def create_initialization_options(
        self, notification_options=None, experimental_capabilities=None, extensions=None
    ):
        return super().create_initialization_options(
            notification_options or NotificationOptions(tools_changed=True),
            experimental_capabilities,
            extensions,
        )


class ScopedASGI:
    """Wraps the streamable-http app.

    Deliberately a class, not a function: Starlette treats a function endpoint as
    a request/response handler, and only non-function callables as raw ASGI apps.
    Also note we register these with `Route`, never `Mount` - `Mount("/mcp")`
    answers `POST /mcp` with a 307 redirect to `/mcp/`, which many MCP clients
    mishandle.
    """

    def __init__(self, app: StreamableHTTPASGIApp) -> None:
        self._app = app

    async def __call__(self, scope, receive, send) -> None:
        await self._app(scope, receive, send)


class McpEndpoint:
    def __init__(self, registry: Registry, service: "Service") -> None:
        self.registry = registry
        self.service = service
        self.server = HubServer(
            "mcp-switchboard",
            version=_hub_version(),
            on_list_tools=self._on_list_tools,
            on_call_tool=self._on_call_tool,
        )
        self.session_manager = StreamableHTTPSessionManager(
            app=self.server, json_response=False, stateless=False
        )
        self._asgi = ScopedASGI(StreamableHTTPASGIApp(self.session_manager))
        # Consumers we have seen, so we can tell them the tool list changed.
        self._consumers: Set[Any] = set()
        # Scope remembered per consumer session, for the messages that arrive
        # without an HTTP request attached.
        self._scopes: Dict[int, Tuple[Scope, Optional[str], Optional[str], Optional[str]]] = {}

    # -- routing -------------------------------------------------------

    def routes(self) -> list:
        return [
            Route("/mcp", endpoint=self._asgi),
            Route("/mcp/host/{label}", endpoint=self._asgi),
            Route("/mcp/host/{label}/project/{project}", endpoint=self._asgi),
            Route("/mcp/host/{label}/server/{server}", endpoint=self._asgi),
            Route("/mcp/host/{label}/project/{project}/server/{server}", endpoint=self._asgi),
        ]

    def run(self):
        """Async context manager that must be entered exactly once, in the app lifespan."""
        return self.session_manager.run()

    # -- scope ---------------------------------------------------------

    def _scope_for(self, ctx: Any) -> Tuple[Scope, Optional[str], Optional[str], Optional[str]]:
        request = getattr(ctx, "request", None)
        key = id(ctx.session)
        if request is not None:
            params = dict(getattr(request, "path_params", {}) or {})
            label = params.get("label")
            project = params.get("project")
            server = params.get("server")
            if label and project and server:
                resolved = (Scope.PROJECT_SERVER, label, project, server)
            elif label and server:
                resolved = (Scope.SERVER, label, None, server)
            elif label and project:
                resolved = (Scope.PROJECT, label, project, None)
            elif label:
                resolved = (Scope.HOST, label, None, None)
            else:
                resolved = (Scope.ALL, None, None, None)
            self._scopes[key] = resolved
            return resolved
        return self._scopes.get(key, (Scope.ALL, None, None, None))

    # -- handlers ------------------------------------------------------

    async def _on_list_tools(self, ctx: Any, params: Any) -> types.ListToolsResult:
        self._consumers.add(ctx.session)
        scope, want_label, want_project, want_server = self._scope_for(ctx)

        tools: list[types.Tool] = []
        seen: Dict[str, str] = {}

        for connection, channel in self.registry.iter_servers():
            if want_label is not None and connection.label != want_label:
                continue
            if want_project is not None and channel.project != want_project:
                continue
            if want_server is not None and channel.name != want_server:
                continue
            if not channel.ready:
                continue

            for tool in channel.tools:
                exposed = compose_tool_name(scope, connection.label, channel.project, channel.name, tool.name)
                if exposed is None:
                    continue

                origin = f"{connection.label}/{channel.project + '/' if channel.project else ''}{channel.name}"
                if exposed in seen:
                    # Duplicate names in one list are undefined behavior - the
                    # client just picks one. Drop the later one loudly instead.
                    LOGGER.warning(
                        "tool name %r from %s collides with %s; hiding the later one. "
                        "Use a per-host endpoint (/mcp/host/<label>) to disambiguate.",
                        exposed, origin, seen[exposed],
                    )
                    continue
                seen[exposed] = origin

                tools.append(
                    types.Tool(
                        name=exposed,
                        title=_title(tool.name, connection.label, channel.project, channel.name),
                        description=_describe(tool.description, connection.label, channel.project, channel.name),
                        input_schema=tool.input_schema or {"type": "object"},
                        meta={
                            "host": connection.label,
                            "project": channel.project,
                            "server": channel.name,
                            "connectionId": connection.id,
                            "upstreamName": tool.name,
                        },
                    )
                )

        return types.ListToolsResult(tools=sorted(tools, key=lambda t: t.name))

    async def _on_call_tool(self, ctx: Any, params: types.CallToolRequestParams) -> types.CallToolResult:
        scope, want_label, want_project, want_server = self._scope_for(ctx)
        found = self.registry.resolve_tool(scope, params.name, want_label, want_project, want_server)
        if found is None:
            return types.CallToolResult(
                content=[types.TextContent(type="text", text=f"no tool named {params.name!r} is connected")],
                is_error=True,
            )

        connection, channel, tool_name = found
        return await self.service.call_tool_for_mcp(
            connection=connection,
            channel=channel,
            tool=tool_name,
            arguments=dict(params.arguments or {}),
            exposed_name=params.name,
        )

    # -- notifications -------------------------------------------------

    async def broadcast_tools_changed(self) -> None:
        """Tell every live consumer to re-list. Called when a tunnel comes or goes."""
        for session in list(self._consumers):
            try:
                await session.send_tool_list_changed()
            except Exception as e:  # noqa: BLE001 - a dead consumer must not matter
                LOGGER.debug("dropping consumer after failed notify: %s", e)
                self._consumers.discard(session)
                self._scopes.pop(id(session), None)


def _title(tool: str, label: str, project: Optional[str], server: str) -> str:
    where = f"{project}/{server}" if project else server
    return f"{tool} · {where} @ {label}"


def _describe(description: Optional[str], label: str, project: Optional[str], server: str) -> str:
    """Tag the description with its origin.

    This is the field an LLM actually reads when choosing a tool, so the origin
    belongs here and not only in the name.
    """
    where = f"{label} · {project} · {server}" if project else f"{label} · {server}"
    prefix = f"[{where}]"
    return f"{prefix} {description}" if description else prefix


def _hub_version() -> str:
    try:
        from importlib.metadata import version

        return version("mcp-switchboard-hub")
    except Exception:  # noqa: BLE001
        return "0.0.0+dev"
