"""Builds the console's "Endpoints" panel: the /mcp URL reference table and the
copyable client-install command.

Kept as a pure function of (Settings, a registry snapshot) rather than a method
on Service, so it is trivial to test with a plain dict and no live hub.
"""

from __future__ import annotations

from typing import Any, Dict, List, Optional

from .config import Settings

INSTALL_SCRIPT_URL = "https://akospapp.github.io/mcp-switchboard/install.sh"


def _first_tool(server: Dict[str, Any]) -> Optional[Dict[str, Any]]:
    tools = server.get("tools") or []
    return tools[0] if tools else None


def _placeholder_example(scope: str) -> str:
    """The example tool name shown when nothing real is connected yet."""
    examples = {
        "all": "legion5__git__git_status",
        "host": "git__git_status",
        "project": "server__tool",
        "server": "git_status",
        "project_server": "tool",
    }
    return examples[scope]


def build_endpoint_rows(snapshot: Dict[str, Any]) -> List[Dict[str, Any]]:
    """One row per meaningful scope, using real connected tools as examples
    where possible and falling back to a placeholder example otherwise."""
    connections = snapshot.get("connections") or []

    example_all = None
    for connection in connections:
        for server in connection.get("servers") or []:
            tool = _first_tool(server)
            if tool and tool.get("exposedName"):
                example_all = tool["exposedName"]
                break
        if example_all:
            break

    rows: List[Dict[str, Any]] = [
        {
            "path": "/mcp",
            "scope": "all",
            "description": "everything, every machine",
            "example": example_all or _placeholder_example("all"),
        }
    ]

    seen_hosts = set()
    seen_projects = set()

    if not connections:
        rows.append(
            {
                "path": "/mcp/host/<host>",
                "scope": "host",
                "description": "everything on one machine",
                "example": _placeholder_example("host"),
            }
        )
        return rows

    for connection in connections:
        label = connection.get("label")
        servers = connection.get("servers") or []

        if label not in seen_hosts:
            seen_hosts.add(label)
            tool = None
            for server in servers:
                tool = _first_tool(server)
                if tool:
                    break
            example = tool["exposedName"].split("__", 1)[-1] if tool else _placeholder_example("host")
            rows.append(
                {
                    "path": f"/mcp/host/{label}",
                    "scope": "host",
                    "description": f"everything on {label}",
                    "example": example,
                }
            )

        projects = sorted({s.get("project") for s in servers if s.get("project")})
        for project in projects:
            key = (label, project)
            if key in seen_projects:
                continue
            seen_projects.add(key)
            example = _placeholder_example("project")
            for server in servers:
                if server.get("project") != project:
                    continue
                tool = _first_tool(server)
                if tool:
                    example = f"{server['name']}__{tool['name']}"
                    break
            rows.append(
                {
                    "path": f"/mcp/host/{label}/project/{project}",
                    "scope": "project",
                    "description": f"the {project!r} project on {label}",
                    "example": example,
                }
            )

        for server in servers:
            project = server.get("project")
            name = server.get("name")
            tool = _first_tool(server)
            example = tool["name"] if tool else _placeholder_example("server")
            if project:
                rows.append(
                    {
                        "path": f"/mcp/host/{label}/project/{project}/server/{name}",
                        "scope": "project_server",
                        "description": f"just {name} (in {project} on {label})",
                        "example": example,
                    }
                )
            else:
                rows.append(
                    {
                        "path": f"/mcp/host/{label}/server/{name}",
                        "scope": "server",
                        "description": f"just {name} on {label}",
                        "example": example,
                    }
                )

    return rows


def build_install_command(settings: Settings) -> Optional[str]:
    """The client install one-liner, or None if MCP_SWITCHBOARD_PUBLIC_URL is unset.

    Includes the real tunnel token: this only ever renders on the private
    listener, which is not meant to be exposed - equivalent exposure to
    reading the same token out of the hub's own settings file.
    """
    if not settings.public_url:
        return None
    return (
        f"curl -fsSL {INSTALL_SCRIPT_URL} | sh -s -- "
        f"--hub-url {settings.public_url} --token {settings.tunnel_token}"
    )


def build_endpoints_info(settings: Settings, snapshot: Dict[str, Any]) -> Dict[str, Any]:
    return {
        "localBaseUrl": settings.effective_local_base_url,
        "publicUrl": settings.public_url,
        "installCommand": build_install_command(settings),
        "rows": build_endpoint_rows(snapshot),
    }
