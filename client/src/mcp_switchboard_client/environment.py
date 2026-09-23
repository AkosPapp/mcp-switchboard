"""Detect where the client runs, for the ``hello`` frame's ``client.environment``.

Pure and best-effort: :func:`detect` never raises, reads only a small fixed set of
signals, and reports paths and names, never environment variable values beyond the
documented details (docs/AGENT_MODEL_API.md, "Connections: environment detection").
"""

from __future__ import annotations

import json
import logging
import os
import re
import sys
from pathlib import Path
from typing import Any, Dict, List, Mapping, Optional

LOGGER = logging.getLogger("mcp_switchboard_client.environment")

_CGROUP_MARKERS = ("docker", "kubepods", "containerd", "podman")
_MAX_STR = 256
_MAX_WALK = 64


def _cap(value: str) -> str:
    return value[:_MAX_STR]


def strip_jsonc(text: str) -> str:
    """Remove ``//`` and ``/* */`` comments and trailing commas, leaving strings intact."""
    out: List[str] = []
    i, n = 0, len(text)
    while i < n:
        c = text[i]
        if c == '"':
            j = i + 1
            while j < n and text[j] != '"':
                j += 2 if text[j] == "\\" else 1
            out.append(text[i : j + 1])
            i = j + 1
        elif text.startswith("//", i):
            while i < n and text[i] != "\n":
                i += 1
        elif text.startswith("/*", i):
            end = text.find("*/", i + 2)
            i = n if end < 0 else end + 2
        else:
            out.append(c)
            i += 1
    cleaned = "".join(out)
    # Trailing commas: a comma followed only by whitespace and a closer. Strings
    # were already tokenised above, but the regex could match inside one, so redo
    # it string-aware.
    result: List[str] = []
    i, n = 0, len(cleaned)
    while i < n:
        c = cleaned[i]
        if c == '"':
            j = i + 1
            while j < n and cleaned[j] != '"':
                j += 2 if cleaned[j] == "\\" else 1
            result.append(cleaned[i : j + 1])
            i = j + 1
        elif c == "," and re.match(r"\s*[}\]]", cleaned[i + 1 :]):
            i += 1
        else:
            result.append(c)
            i += 1
    return "".join(result)


def _read(path: Path, limit: int = 1_000_000) -> Optional[str]:
    try:
        with open(path, "r", encoding="utf-8", errors="replace") as f:
            return f.read(limit)
    except OSError:
        return None


def _parents(cwd: Path) -> List[Path]:
    return [cwd, *cwd.parents][:_MAX_WALK]


def find_devcontainer_name(cwd: Path) -> Optional[str]:
    """``name`` from the nearest devcontainer.json at or above ``cwd``."""
    for directory in _parents(cwd):
        for candidate in (
            directory / ".devcontainer" / "devcontainer.json",
            directory / ".devcontainer.json",
        ):
            text = _read(candidate)
            if text is None:
                continue
            try:
                data = json.loads(strip_jsonc(text))
            except ValueError:
                continue
            name = data.get("name") if isinstance(data, dict) else None
            if isinstance(name, str) and name.strip():
                return _cap(name.strip())
    return None


def find_git_root(cwd: Path) -> Optional[Path]:
    """The nearest directory at or above ``cwd`` containing ``.git`` (dir or file)."""
    for directory in _parents(cwd):
        try:
            if (directory / ".git").exists():
                return directory
        except OSError:
            continue
    return None


def _in_container(root: Path) -> bool:
    for marker in ("/.dockerenv", "/run/.containerenv"):
        try:
            if Path(marker).exists():
                return True
        except OSError:
            pass
    cgroup = _read(Path("/proc/1/cgroup"), 65536) if root == Path("/") else None
    return bool(cgroup) and any(m in cgroup.lower() for m in _CGROUP_MARKERS)


def detect(
    cwd: Optional[Path] = None,
    env: Optional[Mapping[str, str]] = None,
    project_override: Optional[str] = None,
) -> Dict[str, Any]:
    """Return the ``environment`` object for ``hello``. Never raises."""
    try:
        return _detect(cwd, env, project_override)
    except Exception:  # noqa: BLE001 - detection must never break the client
        LOGGER.debug("environment detection failed", exc_info=True)
        return {"kinds": []}


def _detect(
    cwd: Optional[Path], env: Optional[Mapping[str, str]], project_override: Optional[str]
) -> Dict[str, Any]:
    env = os.environ if env is None else env
    cwd = Path(os.getcwd() if cwd is None else cwd).absolute()
    kinds: List[str] = []
    details: Dict[str, str] = {}

    container = _in_container(Path("/"))
    devcontainer = any(
        env.get(k) for k in ("REMOTE_CONTAINERS", "DEVCONTAINER", "CODESPACES", "VSCODE_REMOTE_CONTAINERS_SESSION")
    ) or (container and str(cwd).startswith("/workspaces/"))
    dc_name = find_devcontainer_name(cwd) if devcontainer else None

    if devcontainer:
        kinds.append("devcontainer")
        if dc_name:
            details["devcontainerName"] = dc_name
        image = env.get("DEVCONTAINER_IMAGE") or env.get("CONTAINER_IMAGE")
        if image:
            details["image"] = _cap(image)
    if container:
        kinds.append("container")
    if env.get("DIRENV_DIR") or env.get("DIRENV_FILE") or env.get("DIRENV_DIFF"):
        kinds.append("direnv")
        direnv_dir = env.get("DIRENV_DIR", "").lstrip("-")
        if direnv_dir:
            details["direnvDir"] = _cap(direnv_dir)
    in_nix = env.get("IN_NIX_SHELL")
    if in_nix or env.get("NIX_BUILD_TOP") or "/nix/store/" in env.get("PATH", ""):
        kinds.append("nix-shell")
        if in_nix in ("pure", "impure"):
            details["nixShell"] = in_nix
        elif in_nix:
            details["nixShell"] = "impure"
    venv = env.get("VIRTUAL_ENV")
    if venv or sys.prefix != getattr(sys, "base_prefix", sys.prefix):
        kinds.append("venv")
        name = os.path.basename((venv or sys.prefix).rstrip("/\\"))
        if name:
            details["venv"] = _cap(name)

    project = (project_override or "").strip() or None
    if project is None and dc_name:
        project = dc_name
    if project is None:
        root = find_git_root(cwd)
        project = (root or cwd).name or None

    result: Dict[str, Any] = {"kinds": kinds}
    if project:
        result["project"] = _cap(project)
    result["workspace"] = _cap(str(cwd))
    if details:
        result["details"] = details
    return result


def summarize(environment: Mapping[str, Any]) -> str:
    kinds = ",".join(environment.get("kinds", [])) or "none"
    return (
        f"environment: kinds={kinds} project={environment.get('project', '-')} "
        f"workspace={environment.get('workspace', '-')}"
    )
