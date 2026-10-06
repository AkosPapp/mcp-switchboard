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
from typing import Any, Dict, List, Mapping, Optional, Sequence

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


# ---------- Instruction files (hello instructions / context_update) ----------
#
# The repo's own agent instructions, read from the client host and shipped to
# the hub (docs/PROTOCOL.md: hello.client.instructions, refreshed by the
# context_update frame). These budgets are deliberately SEPARATE from the
# environment caps above (which are 256-rune strings for display); instruction
# bodies are content the model must see in full, so they get a larger,
# explicitly-documented budget. Nothing here may raise: the tunnel treats a
# failed collection as "no instruction files", never as a fault.

INSTRUCTION_FILENAMES = ("AGENTS.md", "CLAUDE.md", ".cursorrules", ".github/copilot-instructions.md")

INSTRUCTION_MAX_FILE_CHARS = 32 * 1024  # per file
INSTRUCTION_MAX_FILES = 8  # per host
INSTRUCTION_MAX_TOTAL_CHARS = 64 * 1024  # per hello/context_update
_INSTRUCTION_MAX_DIR_SCANS = 5000  # walk bound so a huge tree cannot stall a connect
# Cache, dependency and build trees: instruction files under them are never the
# project's own guidance. Dotted directories are not descended into except
# .github, which is on the standard-name list.
_INSTRUCTION_SKIP_DIRS = {
    ".git", "node_modules", "__pycache__", ".venv", "venv", ".harness",
    ".mypy_cache", ".pytest_cache", ".ruff_cache", ".tox", ".cache",
    "dist", "build", "target", ".next", ".terraform",
}
_INSTRUCTION_DESCEND_DOTS = {".github"}


def find_instruction_files(cwd: Path) -> List[Path]:
    """All standard instruction files at or under ``cwd``, root-first.

    Every found file is returned, not just the nearest: the client cannot
    predict which paths the agent will work under, and the hub labels each with
    its path and states the refine/nested rule. Deterministic order: directory
    depth, then path, then the fixed filename order within a directory.
    """
    names = {n.split("/")[-1] for n in INSTRUCTION_FILENAMES}
    order = {n.split("/")[-1]: i for i, n in enumerate(INSTRUCTION_FILENAMES)}
    located: List[tuple] = []  # (depth, posix, Path)
    scans = 0
    try:
        for dirpath, dirnames, filenames in os.walk(cwd, followlinks=False):
            scans += 1
            if scans > _INSTRUCTION_MAX_DIR_SCANS:
                break
            base = Path(dirpath)
            dirnames[:] = sorted(
                d for d in dirnames
                if d not in _INSTRUCTION_SKIP_DIRS and (not d.startswith(".") or d in _INSTRUCTION_DESCEND_DOTS)
            )
            depth = len(base.relative_to(cwd).parts)
            for name in sorted(filenames):
                if name not in names:
                    continue
                # copilot-instructions.md only counts inside a .github
                # directory; the bare names count anywhere kept.
                if name == "copilot-instructions.md" and base.name != ".github":
                    continue
                located.append((depth, base / name, order[name]))
    except OSError:
        return []
    located.sort(key=lambda item: (item[0], item[1].as_posix(), item[2]))
    return [item[1] for item in located]


def collect_instructions(cwd: Path) -> List[Dict[str, str]]:
    """Instruction files as ``[{path, content}, ...]`` within the wire budget.

    ``path`` is relative to ``cwd`` (forward slashes). A file over the per-file
    budget is truncated with a visible marker; files that do not fit the
    whole-hello budget are replaced by one ``(omitted)`` entry listing them, so
    the agent knows the set is incomplete. Never raises.
    """
    files = find_instruction_files(cwd)
    out: List[Dict[str, str]] = []
    skipped: List[str] = []
    used = 0
    try:
        for path in files:
            rel = path.relative_to(cwd).as_posix()
            if len(out) >= INSTRUCTION_MAX_FILES:
                skipped.append(rel)
                continue
            try:
                text = path.read_text(encoding="utf-8", errors="replace")
            except OSError:
                continue
            if len(text) > INSTRUCTION_MAX_FILE_CHARS:
                marker = f"\n[... truncated by the client budget: the file is {len(text)} characters]"
                # Keep the marker INSIDE the cap so the hub's identical re-cap
                # cannot shave it off and make the truncation invisible again.
                text = text[: max(INSTRUCTION_MAX_FILE_CHARS - len(marker), 0)] + marker
            remaining = INSTRUCTION_MAX_TOTAL_CHARS - used
            if len(text) > remaining:
                skipped.append(rel)
                continue
            used += len(text)
            out.append({"path": rel, "content": text})
    except Exception:  # noqa: BLE001 - collecting must never break the tunnel
        LOGGER.debug("instruction collection failed", exc_info=True)
    if skipped:
        out.append(
            {
                "path": "(omitted by client budget)",
                "content": "More instruction files exist under this root but were not sent "
                "(per-host budgets: %d files, %d total characters). Known names not included:\n%s"
                % (INSTRUCTION_MAX_FILES, INSTRUCTION_MAX_TOTAL_CHARS, "\n".join(f"- {s}" for s in skipped)),
            }
        )
    return out


# ---------- Environment brief (hello environment_brief / context_update) -----
#
# A short, cheaply-computed description of the client host so the agent knows
# where its tools actually run without spending a turn on probing (docs/
# PROTOCOL.md). Rules: everything is read from local sources with a small
# timeout, nothing may hang a connect (a hung brief is a skipped line), network
# reachability is NEVER probed (interfaces/resolvers only), and anything that
# cannot be verified within the budget is reported "unknown", not guessed.
# Collected again on every refresh tick, so the hub's own staleness flag is
# what covers a client that vanishes mid-session.

BRIEF_MAX_CHARS = 6 * 1024  # wire comfort; the hub caps the field on its side
BRIEF_MAX_LINES = 30

BRIEF_TOOLS = ("git", "rg", "ctags", "python3", "node", "go", "docker", "kubectl", "sbatch", "ruff", "pytest", "opencode")

_CRED_IN_URL = re.compile(r"//[^/@\s]*:[^/@\s]*@")


def _brief_proc(cmd: List[str], timeout: float = 2.0) -> Optional[str]:
    """Run a helper, return stdout stripped; None on any failure or timeout."""
    import subprocess

    try:
        done = subprocess.run(
            cmd, capture_output=True, text=True, timeout=timeout,
            stdin=subprocess.DEVNULL, encoding="utf-8", errors="replace",
        )
    except (OSError, subprocess.TimeoutExpired, ValueError):
        return None
    if done.returncode != 0:
        return None
    return done.stdout.strip()


def _strip_git_credentials(url: str) -> str:
    return _CRED_IN_URL.sub("//", url)


def _sudo_state(timeout: float = 1.5) -> str:
    """yes/no/unknown, decided only by a NON-INTERACTIVE check that cannot prompt."""
    import subprocess

    try:
        done = subprocess.run(
            ["sudo", "-n", "true"], capture_output=True, text=True, timeout=timeout,
            stdin=subprocess.DEVNULL, env={**os.environ, "SUDO_ASKPASS": "/bin/false"},
        )
    except (OSError, subprocess.TimeoutExpired, ValueError):
        return "unknown"
    if done.returncode == 0:
        return "yes"
    # -n makes "password required" an immediate exit, so a real code came back.
    return "no"


def collect_environment_brief(
    root: Optional[Path] = None, instruction_paths: Optional[Sequence[str]] = None
) -> str:
    """A short text brief of the client host for the environment-brief frame.

    Identity, host, git state, what the file tools can write, DNS (unprobed
    reachability), and presence/absence of a curated tool list plus sudo.
    Never raises; a section whose helper fails is simply left out.
    """
    import getpass
    import grp
    import platform
    import shutil
    import socket
    import tempfile

    lines: List[str] = []
    try:
        root = Path(root) if root is not None else Path(os.getcwd())

        user = f"uid={os.getuid()} user={getpass.getuser()}"
        try:
            gids = {os.getgid(), *os.getgroups()}
            groups = ",".join(sorted({grp.getgrgid(g).gr_name for g in gids})[:8])
            if groups:
                user += f" groups={groups}"
        except (KeyError, OSError):
            pass
        lines.append(f"user: {user}")
        lines.append(
            "host: "
            f"hostname={socket.gethostname()} os={platform.system().lower()}-{platform.machine()} "
            f"python={platform.python_version()}"
        )
        try:
            sudo = _sudo_state()
        except Exception:  # noqa: BLE001
            sudo = "unknown"
        lines.append(f"sudo (non-interactive check): {sudo} (unknown = could not verify)")

        lines.append(f"cwd: {root}")
        git_root = find_git_root(root)
        if git_root is not None:
            branch = _brief_proc(["git", "-C", str(git_root), "branch", "--show-current"]) or "(detached)"
            porcelain = _brief_proc(["git", "-C", str(git_root), "status", "--porcelain"]) or ""
            dirty = sum(1 for l in porcelain.splitlines() if l and not l.startswith("??"))
            untracked = sum(1 for l in porcelain.splitlines() if l.startswith("??"))
            lines.append(
                f"git: root={git_root} branch={branch} modified={dirty} untracked={untracked}"
            )
            remote = _brief_proc(["git", "-C", str(git_root), "remote", "get-url", "origin"])
            if remote:
                lines.append(f"git remote origin: {_strip_git_credentials(remote)} (credentials redacted)")

        scratch_root = os.environ.get("MCP_SWITCHBOARD_HARNESS_SCRATCH") or ""
        scratch = scratch_root or (root / ".harness" / "scratch")
        lines.append(
            "file tools can write: the root, scratch "
            f"{scratch} (unless overridden), and temp {tempfile.gettempdir()}; "
            "run_command/run_python are UNCONFINED and can write anything this user can"
        )
        lines.append(
            "harness output cap: "
            f"{os.environ.get('MCP_SWITCHBOARD_HARNESS_MAX_OUTPUT', '100000')} characters per stream"
        )

        resolvers: List[str] = []
        text = _read(Path("/etc/resolv.conf")) or ""
        for l in text.splitlines():
            if l.strip().startswith("nameserver"):
                parts = l.split()
                if len(parts) > 1:
                    resolvers.append(parts[1])
                if len(resolvers) >= 3:
                    break
        addrs = _brief_proc(["hostname", "-I"], timeout=1.0) or "?"
        lines.append(
            f"network: addresses={addrs} resolvers={','.join(resolvers) or '?'}"
            " (reachability NOT probed)"
        )

        present = [t for t in BRIEF_TOOLS if shutil.which(t)]
        absent = [t for t in BRIEF_TOOLS if t not in present]
        if absent:
            lines.append(f"tools present: {', '.join(present) or '-'}; absent: {', '.join(absent)}")
        else:
            lines.append(f"tools present: {', '.join(present)}")

        if instruction_paths is not None:
            joined = ", ".join(instruction_paths)
            lines.append(f"instruction files loaded: {joined or '(none found under the root)'}")

        if len(lines) > BRIEF_MAX_LINES:
            lines = lines[: BRIEF_MAX_LINES - 1] + ["... (brief truncated)"]
    except Exception:  # noqa: BLE001 - a broken helper must not break the tunnel
        LOGGER.debug("environment brief collection failed", exc_info=True)
    brief = "\n".join(lines)
    return brief[:BRIEF_MAX_CHARS] or ""
