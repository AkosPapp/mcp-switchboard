"""Load and normalize MCP server configs into a uniform list of ServerSpec.

Two config shapes are supported (see README.md for the full discriminator rule):

1. Plain ``mcpServers`` entries: ``{"command": ..., "args": [...], "env": {...}}``.
   Spawned directly as the local stdio subprocess.
2. FastMCP-style entries: any entry (or the whole config file, if it has no
   ``mcpServers`` wrapper) that contains a top-level ``source`` key is treated
   as a FastMCP config (https://gofastmcp.com/public/schemas/fastmcp.json/v1.json)
   and launched via ``fastmcp run <generated-config>`` instead of being spawned
   directly. The discriminator is exactly: presence of a ``source`` key.

A server entry may also carry a ``"project"``, grouping it under that name in
the hub's tool naming and scoped endpoints (e.g. ``host/legion5/project/nix/
server/lsp``). A top-level ``"project"`` in the config file sets the default
for every entry that does not specify its own.
"""

from __future__ import annotations

import json
import sys
import logging
import tempfile
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Dict, List, Optional

LOGGER = logging.getLogger("mcp_switchboard_client.config")


class ConfigError(Exception):
    """Raised for any problem loading or interpreting the config file."""


@dataclass
class ServerSpec:
    """A single local MCP server to launch and tunnel."""

    name: str
    argv: List[str]
    env: Dict[str, str] = field(default_factory=dict)
    cwd: Optional[str] = None
    project: Optional[str] = None
    # Set when this spec was materialized from a FastMCP-style entry, so the
    # generated temp config file can be cleaned up on shutdown.
    fastmcp_tempfile: Optional[Path] = None


def load_config(path: Path) -> List[ServerSpec]:
    """Load ``path`` and return the list of servers to tunnel.

    Raises:
        ConfigError: if the file is missing, not valid JSON, or doesn't match
            either supported shape.
    """
    if not path.is_file():
        raise ConfigError(
            f"config file not found: {path} "
            "(pass --config, create ./mcp.json in the current directory, or leave the built-in harness enabled)"
        )

    try:
        raw = json.loads(path.read_text(encoding="utf-8"))
    except json.JSONDecodeError as e:
        raise ConfigError(f"config file {path} is not valid JSON: {e}") from e

    if not isinstance(raw, dict):
        raise ConfigError(f"config file {path} must contain a JSON object at the top level")

    default_project = _read_project(raw, path, "top-level 'project'")

    if "mcpServers" in raw:
        servers = raw["mcpServers"]
        if not isinstance(servers, dict) or not servers:
            raise ConfigError(f"'mcpServers' in {path} must be a non-empty object")
        return [_build_spec(name, entry, path, default_project) for name, entry in servers.items()]

    if "source" in raw:
        # Whole file is a single bare FastMCP config.
        name = raw.get("name") or path.stem or "fastmcp-server"
        return [_build_fastmcp_spec(name, raw, default_project)]

    raise ConfigError(
        f"config file {path} matches neither the 'mcpServers' shape nor the "
        "FastMCP single-server shape (missing both 'mcpServers' and 'source' keys)"
    )


def _read_project(entry: Dict[str, Any], config_path: Path, where: str) -> Optional[str]:
    """Read and normalize an optional ``"project"`` key. Blank counts as absent."""
    project = entry.get("project")
    if project is None:
        return None
    if not isinstance(project, str):
        raise ConfigError(f"{where} in {config_path} must be a string")
    return project.strip() or None


def _build_spec(name: str, entry: Any, config_path: Path, default_project: Optional[str] = None) -> ServerSpec:
    if not isinstance(entry, dict):
        raise ConfigError(f"mcpServers.{name} in {config_path} must be an object")

    if "source" in entry:
        return _build_fastmcp_spec(name, entry, default_project)

    project = _read_project(entry, config_path, f"mcpServers.{name}.project") or default_project

    command = entry.get("command")
    if not command or not isinstance(command, str):
        raise ConfigError(
            f"mcpServers.{name} in {config_path} must have a string 'command' "
            "(or a 'source' key to be treated as a FastMCP-style server)"
        )

    args = entry.get("args", [])
    if not isinstance(args, list) or not all(isinstance(a, str) for a in args):
        raise ConfigError(f"mcpServers.{name}.args in {config_path} must be a list of strings")

    env = entry.get("env", {})
    if not isinstance(env, dict):
        raise ConfigError(f"mcpServers.{name}.env in {config_path} must be an object")

    cwd = entry.get("cwd")
    if cwd is not None and not isinstance(cwd, str):
        raise ConfigError(f"mcpServers.{name}.cwd in {config_path} must be a string")

    return ServerSpec(
        name=name, argv=[command, *args], env={k: str(v) for k, v in env.items()}, cwd=cwd, project=project
    )


def _build_fastmcp_spec(name: str, entry: Dict[str, Any], default_project: Optional[str] = None) -> ServerSpec:
    """Materialize a FastMCP-style entry into a runnable ServerSpec.

    We write the entry out verbatim (minus a forced transport override) as its
    own fastmcp.json-shaped file and launch it with ``fastmcp run <file>``,
    letting FastMCP itself handle the uv-based environment/source setup.

    Only stdio transport can be tunneled by this tool (the wire protocol only
    bridges stdin/stdout), so ``deployment.transport`` is forced to "stdio"
    regardless of what the entry declares, with a warning if it was set to
    something else.
    """
    raw_project = entry.get("project")
    if raw_project is not None and not isinstance(raw_project, str):
        raise ConfigError(f"mcpServers.{name}.project must be a string")
    project = (raw_project.strip() if isinstance(raw_project, str) else None) or default_project

    fastmcp_config = dict(entry)
    fastmcp_config.pop("project", None)  # not a FastMCP config key
    deployment = dict(fastmcp_config.get("deployment") or {})
    original_transport = deployment.get("transport")
    if original_transport and original_transport != "stdio":
        LOGGER.warning(
            "server %r declares deployment.transport=%r, but this tool only "
            "tunnels stdio; overriding to stdio",
            name,
            original_transport,
        )
    deployment["transport"] = "stdio"
    fastmcp_config["deployment"] = deployment

    fd, tmp_name = tempfile.mkstemp(prefix=f"fastmcp-{name}-", suffix=".json")
    tmp_path = Path(tmp_name)
    with open(fd, "w", encoding="utf-8") as f:
        json.dump(fastmcp_config, f)

    return ServerSpec(
        name=name,
        argv=["uvx", "fastmcp", "run", str(tmp_path)],
        env={},
        project=project,
        fastmcp_tempfile=tmp_path,
    )


HARNESS_NAME = "harness"


def harness_spec() -> ServerSpec:
    """The built-in coding harness, run with this interpreter.

    It is a dependency of this package, so it is always importable here and
    needs no separate install or network fetch at startup.
    """
    return ServerSpec(name=HARNESS_NAME, argv=[sys.executable, "-m", "mcp_switchboard_server_harness"])
