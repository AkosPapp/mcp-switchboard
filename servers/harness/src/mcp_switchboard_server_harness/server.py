"""The harness MCP server.

A plain stdio MCP server, so it is tunnelled like any other: reference it from
an ``mcp.json`` as ``uvx mcp-switchboard-server-harness`` (the client also adds
it by default). The tool functions are ordinary module-level functions so they
can be unit-tested directly; everything that runs a subprocess is ``async`` so
one slow command never stalls the server.

**Confinement.** File tools resolve every path (following symlinks) and refuse
anything outside the *root*, which defaults to the working directory and is set
with ``--root`` / ``MCP_SWITCHBOARD_HARNESS_ROOT``. That is a guard against
mistakes and path tricks, not a sandbox: ``run_command``, ``run_python`` and the
``process_*`` tools execute arbitrary code as the launching user, and can do
anything that user can. Only expose the hub's ``/mcp`` endpoints to consumers
you trust.
"""

from __future__ import annotations

import asyncio
import atexit
import base64
import binascii
import fnmatch
import functools
import inspect
import json
import os
import shutil
import signal
import subprocess
import sys
import tempfile
import uuid
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Dict, List, Optional, Tuple

from mcp.server.mcpserver import MCPServer
from mcp.server.mcpserver.exceptions import ToolError
from mcp_types import ToolAnnotations
from pydantic import BaseModel, Field

from . import __version__

ENV_PREFIX = "MCP_SWITCHBOARD_HARNESS_"
DEFAULT_TIMEOUT = 120.0  # seconds, per command
MAX_TIMEOUT = 3600.0
DEFAULT_MAX_OUTPUT = 100_000  # characters returned per stream / file read
MAX_PROCESSES = 16
PROCESS_BUFFER_LIMIT = 1_000_000  # characters kept per background stream
_SKIP_DIRS = {".git", "node_modules", "__pycache__", ".venv"}


# ---------- Configuration and path confinement -----------------------------

_root: Path = Path.cwd().resolve()
_max_output: int = DEFAULT_MAX_OUTPUT


def configure(root: Optional[str] = None, max_output: Optional[int] = None) -> None:
    """Set the confinement root and/or the output cap; None leaves a setting unchanged
    (the root starts as the current directory)."""
    global _root, _max_output
    if root is not None:
        new_root = Path(root).resolve()
        if not new_root.is_dir():
            raise ValueError(f"root is not a directory: {new_root}")
        _root = new_root
    if max_output is not None:
        if max_output < 1:
            raise ValueError("max output must be positive")
        _max_output = max_output


def _resolve(path: str) -> Path:
    """Resolve ``path`` (relative to the root) and ensure it stays inside the root.

    ``realpath`` semantics: symlinks are followed even for the parts of the path
    that exist, so a link pointing outside the root is refused.
    """
    p = Path(path)
    if not p.is_absolute():
        p = _root / p
    resolved = Path(os.path.realpath(p))
    if resolved != _root and _root not in resolved.parents:
        raise PermissionError(f"{path!r} is outside the allowed root {_root}")
    return resolved


def _rel(p: Path) -> str:
    """A resolved path, shown relative to the root ('.' for the root itself)."""
    try:
        return p.relative_to(_root).as_posix() or "."
    except ValueError:
        return str(p)


def _cap(text: str, limit: Optional[int] = None) -> Tuple[str, bool]:
    limit = _max_output if limit is None else limit
    return (text[:limit], True) if len(text) > limit else (text, False)


# ---------- Subprocess helpers ---------------------------------------------


def _kill_group(proc: "asyncio.subprocess.Process") -> None:
    """Kill the process and everything it spawned (it leads its own session)."""
    try:
        os.killpg(proc.pid, signal.SIGKILL)
    except (ProcessLookupError, PermissionError):
        pass


def _clamp_timeout(timeout: Optional[float]) -> float:
    if timeout is None:
        return DEFAULT_TIMEOUT
    if timeout <= 0:
        raise ValueError("timeout must be positive")
    return min(timeout, MAX_TIMEOUT)


async def _run(
    argv: List[str],
    cwd: Optional[Path] = None,
    input: Optional[str] = None,
    env: Optional[Dict[str, str]] = None,
    timeout: Optional[float] = None,
) -> Dict[str, Any]:
    """Run a command to completion; on timeout the whole process group is killed."""
    seconds = _clamp_timeout(timeout)
    try:
        proc = await asyncio.create_subprocess_exec(
            *argv,
            cwd=str(cwd or _root),
            env=env,
            stdin=asyncio.subprocess.PIPE if input is not None else asyncio.subprocess.DEVNULL,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
            start_new_session=True,
        )
    except FileNotFoundError as e:
        raise RuntimeError(f"command not found: {argv[0]}") from e
    try:
        out, err = await asyncio.wait_for(
            proc.communicate(input.encode() if input is not None else None), seconds
        )
    except asyncio.TimeoutError as e:
        _kill_group(proc)
        await proc.wait()
        raise RuntimeError(f"command timed out after {seconds:g}s: {argv[0]}") from e
    except asyncio.CancelledError:
        _kill_group(proc)
        raise
    return {
        "returncode": proc.returncode,
        "stdout": out.decode(errors="replace"),
        "stderr": err.decode(errors="replace"),
    }


def _atomic_write(path: Path, data: bytes) -> None:
    """Write via a temp file in the same directory and ``os.replace``, so readers
    (and a crash) never see a half-written file. Keeps an existing file's mode."""
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, tmp = tempfile.mkstemp(dir=path.parent, prefix=f".{path.name}.")
    try:
        with os.fdopen(fd, "wb") as f:
            f.write(data)
        if path.exists():
            shutil.copymode(path, tmp)
        os.replace(tmp, path)
    except BaseException:
        try:
            os.unlink(tmp)
        except FileNotFoundError:
            pass
        raise


# ---------- Result models (advertised as output schemas) --------------------


class CommandResult(BaseModel):
    stdout: str
    stderr: str
    exit_code: int
    return_value: Optional[Dict[str, Any]] = Field(
        default=None, description="Reserved for structured results; currently always null."
    )
    truncated: bool = Field(default=False, description="stdout or stderr was cut at the output limit.")
    stdout_total: int = Field(default=0, description="Full length of stdout in characters, before truncation.")
    stderr_total: int = Field(default=0, description="Full length of stderr in characters, before truncation.")


class FileReadResult(BaseModel):
    content: Optional[str] = Field(default=None, description="Base64 of the bytes read; set when binary=true.")
    raw_text: Optional[str] = Field(default=None, description="UTF-8 text read; set when binary=false.")
    size: int = Field(description="Total size of the file in bytes.")
    offset: int = Field(description="Where the read started (bytes for binary, characters for text).")
    truncated: bool = Field(description="More data remains after this chunk; read again with a larger offset.")


class FileWriteResult(BaseModel):
    success: bool
    message: str


class FileDeleteResult(BaseModel):
    deleted: bool
    message: str


class DirEntry(BaseModel):
    name: str
    path: str = Field(description="Relative to the root.")
    size: int
    is_dir: bool
    mtime: Optional[str] = Field(default=None, description="Modification time, ISO 8601 UTC.")


class DirListResult(BaseModel):
    entries: List[DirEntry]


class FileMoveResult(BaseModel):
    moved: bool
    message: str


class GrepMatch(BaseModel):
    file: str
    line_no: int
    text: str
    is_match: bool = Field(description="False for a surrounding context line.")


class GrepResult(BaseModel):
    matches: List[GrepMatch]
    truncated: bool = Field(description="max_results was reached; narrow the search.")


class ProcessStarted(BaseModel):
    id: str
    pid: int


class ProcessOutput(BaseModel):
    id: str
    stdout: str = Field(description="New output since the previous read.")
    stderr: str
    running: bool
    exit_code: Optional[int] = None
    dropped: bool = Field(default=False, description="Older output was discarded because the buffer filled.")


class ProcessKilled(BaseModel):
    id: str
    killed: bool
    message: str


# ---------- Shell, files ---------------------------------------------------


async def run_command(
    command: str,
    cwd: Optional[str] = None,
    env: Optional[Dict[str, str]] = None,
    timeout: Optional[float] = None,
) -> CommandResult:
    """Execute a shell command (via `bash -c`). cwd defaults to the root; env entries override the inherited environment; timeout is in seconds (default 120, max 3600) and kills the command and its children."""
    merged = {**os.environ, **env} if env else None
    result = await _run(
        ["bash", "-c", command], cwd=_resolve(cwd) if cwd else None, env=merged, timeout=timeout
    )
    out, out_cut = _cap(result["stdout"])
    err, err_cut = _cap(result["stderr"])
    return CommandResult(
        stdout=out,
        stderr=err,
        exit_code=result["returncode"],
        truncated=out_cut or err_cut,
        stdout_total=len(result["stdout"]),
        stderr_total=len(result["stderr"]),
    )


def file_read(
    path: str, binary: bool = False, offset: int = 0, limit: Optional[int] = None
) -> FileReadResult:
    """Read a file. With binary=true the bytes come back base64-encoded in `content`; otherwise UTF-8 text in `raw_text`. Output is capped (default 100000): use offset and limit (bytes for binary, characters for text) to page through large files."""
    p = _resolve(path)
    if offset < 0:
        raise ValueError("offset must not be negative")
    want = _max_output if limit is None else min(limit, _max_output)
    if want < 1:
        raise ValueError("limit must be positive")
    size = p.stat().st_size
    if binary:
        with open(p, "rb") as f:
            f.seek(offset)
            chunk = f.read(want)
        return FileReadResult(
            content=base64.b64encode(chunk).decode("ascii"),
            size=size,
            offset=offset,
            truncated=offset + len(chunk) < size,
        )
    text = p.read_text()
    chunk = text[offset : offset + want]
    return FileReadResult(
        raw_text=chunk, size=size, offset=offset, truncated=offset + len(chunk) < len(text)
    )


def file_write(path: str, content: str, append: bool = False, binary: bool = False) -> FileWriteResult:
    """Write a file, creating parent directories. With binary=true, `content` is base64 and is decoded to bytes; otherwise it is written as UTF-8 text. Overwrites are atomic; append=true appends instead."""
    p = _resolve(path)
    try:
        data = base64.b64decode(content, validate=True) if binary else content.encode()
    except (binascii.Error, ValueError) as e:
        return FileWriteResult(success=False, message=f"content is not valid base64: {e}")
    try:
        if append:
            p.parent.mkdir(parents=True, exist_ok=True)
            with open(p, "ab") as f:
                f.write(data)
        else:
            _atomic_write(p, data)
    except OSError as e:
        return FileWriteResult(success=False, message=str(e))
    return FileWriteResult(success=True, message=f"{'appended' if append else 'wrote'} {len(data)} bytes to {path}")


def file_delete(path: str, recursive: bool = True) -> FileDeleteResult:
    """Delete a file, symlink or directory. Directories need recursive=true unless empty. A missing path is reported, not raised. The root itself cannot be deleted."""
    lexical = Path(os.path.normpath(Path(path) if Path(path).is_absolute() else _root / path))
    if lexical == _root:
        return FileDeleteResult(deleted=False, message="refusing to delete the root")
    p = _resolve(str(lexical.parent)) / lexical.name  # resolve the parent only: deleting a symlink removes the link
    try:
        if p.is_symlink() or p.is_file():
            p.unlink()
        elif p.is_dir():
            if recursive:
                shutil.rmtree(p)
            else:
                p.rmdir()
        else:
            return FileDeleteResult(deleted=False, message=f"{path} does not exist")
    except OSError as e:
        return FileDeleteResult(deleted=False, message=str(e))
    return FileDeleteResult(deleted=True, message=f"deleted {path}")


def _entry(p: Path) -> DirEntry:
    st = p.lstat()
    return DirEntry(
        name=p.name,
        path=_rel(p),
        size=st.st_size,
        is_dir=p.is_dir() and not p.is_symlink(),
        mtime=datetime.fromtimestamp(st.st_mtime, tz=timezone.utc).isoformat(),
    )


def dir_list(path: str, recursive: bool = False) -> DirListResult:
    """List a directory's entries with size, type and mtime; recursive=true descends into sub-folders (symlinks are not followed)."""
    base = _resolve(path)
    if not base.is_dir():
        raise NotADirectoryError(path)
    if recursive:
        paths: List[Path] = []
        for dirpath, dirnames, filenames in os.walk(base):
            dirnames.sort()
            paths.extend(Path(dirpath) / n for n in [*dirnames, *sorted(filenames)])
    else:
        paths = sorted(base.iterdir())
    return DirListResult(entries=[_entry(p) for p in paths])


def file_move(src: str, dst: str) -> FileMoveResult:
    """Move or rename a file or directory. Refuses to overwrite an existing destination; creates missing parent directories."""
    s, d = _resolve(src), _resolve(dst)
    if not os.path.lexists(s):
        return FileMoveResult(moved=False, message=f"{src} does not exist")
    if os.path.lexists(d):
        return FileMoveResult(moved=False, message=f"{dst} already exists")
    try:
        d.parent.mkdir(parents=True, exist_ok=True)
        shutil.move(str(s), str(d))
    except OSError as e:
        return FileMoveResult(moved=False, message=str(e))
    return FileMoveResult(moved=True, message=f"moved {src} to {dst}")


# ---------- Reading and searching -------------------------------------------


def tree_of_files(root: str = ".") -> Dict[str, Any]:
    """Nested view of a directory tree: each directory maps to its subdirectories, with its files under "__files__"; skips .git, node_modules, __pycache__ and .venv."""
    base = _resolve(root)
    tree: Dict[str, Any] = {}
    for dirpath, dirnames, filenames in os.walk(base):
        dirnames[:] = sorted(d for d in dirnames if d not in _SKIP_DIRS)
        node = tree
        for part in Path(dirpath).relative_to(base).parts:
            node = node.setdefault(part, {})
        node["__files__"] = sorted(filenames)
    return tree


async def _tracked_files(base: Path) -> Optional[List[Path]]:
    """Files git knows about (tracked plus untracked-not-ignored) under ``base``,
    or None if ``base`` is not inside a git work tree."""
    try:
        result = await _run(
            ["git", "ls-files", "--cached", "--others", "--exclude-standard", "-z"], cwd=base
        )
    except RuntimeError:
        return None
    if result["returncode"] != 0:
        return None
    return [base / name for name in result["stdout"].split("\0") if name]


async def find_files(pattern: str, root: str = ".", limit: int = 1000) -> List[str]:
    """Find files under root whose path or name matches a glob such as "**/*.py". Inside a git repository .gitignore is honoured; otherwise .git, node_modules, __pycache__ and .venv are skipped. Returns at most `limit` root-relative paths, sorted."""
    base = _resolve(root)
    candidates = await _tracked_files(base)
    if candidates is None:
        candidates = []
        for dirpath, dirnames, filenames in os.walk(base):
            dirnames[:] = [d for d in dirnames if d not in _SKIP_DIRS]
            candidates.extend(Path(dirpath) / n for n in filenames)
    found = []
    for p in candidates:
        rel = p.relative_to(base).as_posix()
        if fnmatch.fnmatch(rel, pattern) or fnmatch.fnmatch(p.name, pattern):
            if p.is_file():
                found.append(_rel(p))
    return sorted(found)[: max(limit, 0)]


async def ripgrep(
    query: str,
    path: str = ".",
    glob: Optional[str] = None,
    ignore_case: bool = False,
    context: int = 0,
    max_results: int = 200,
) -> GrepResult:
    """Search file contents with ripgrep (must be installed). glob filters files (e.g. "*.py"), context adds N surrounding lines per match, and at most max_results lines are returned (`truncated` says if more existed)."""
    target = _resolve(path)
    argv = ["rg", "--json"]
    if ignore_case:
        argv.append("--ignore-case")
    if glob:
        argv += ["--glob", glob]
    if context > 0:
        argv += ["--context", str(min(context, 20))]
    argv += ["--", query, str(target)]
    result = await _run(argv)
    if result["returncode"] not in (0, 1):  # 1 means no matches
        raise RuntimeError(result["stderr"].strip() or "ripgrep failed")
    matches: List[GrepMatch] = []
    truncated = False
    for line in result["stdout"].splitlines():
        try:
            obj = json.loads(line)
        except ValueError:
            continue
        if obj.get("type") not in ("match", "context"):
            continue
        if len(matches) >= max_results:
            truncated = True
            break
        data = obj["data"]
        matches.append(
            GrepMatch(
                file=_rel(Path(data["path"].get("text", ""))),
                line_no=data["line_number"],
                text=data["lines"].get("text", "").rstrip("\n"),
                is_match=obj["type"] == "match",
            )
        )
    return GrepResult(matches=matches, truncated=truncated)


def read_lines(path: str, start: int = 1, end: Optional[int] = None) -> List[str]:
    """Lines start..end (1-based, inclusive) of a file; end defaults to the last line."""
    lines = _resolve(path).read_text().splitlines()
    return lines[max(start, 1) - 1 : end]


def edit_file(
    path: str,
    new_content: Optional[str] = None,
    old_str: Optional[str] = None,
    new_str: Optional[str] = None,
    replace_all: bool = False,
) -> str:
    """Edit an existing file: either overwrite it with new_content, or replace old_str with new_str. old_str must occur exactly once unless replace_all is true (an ambiguous match is an error, so the wrong spot is never edited silently). Writes are atomic."""
    p = _resolve(path)
    if not p.is_file():
        raise FileNotFoundError(str(path))
    if new_content is not None:
        _atomic_write(p, new_content.encode())
        return "wrote full content"
    if old_str is None or new_str is None:
        raise ValueError("provide new_content, or both old_str and new_str")
    if not old_str:
        raise ValueError("old_str must not be empty")
    text = p.read_text()
    count = text.count(old_str)
    if count == 0:
        raise ValueError(f"{old_str!r} not found in {path}")
    if count > 1 and not replace_all:
        raise ValueError(
            f"{old_str!r} occurs {count} times in {path}; add context to make it unique or set replace_all"
        )
    _atomic_write(p, text.replace(old_str, new_str).encode())
    return f"replaced {count} occurrence{'s' if count != 1 else ''}"


async def run_python(code: str, timeout: Optional[float] = None) -> Dict[str, Any]:
    """Run a Python snippet in a fresh interpreter (cwd = the root) and return {returncode, stdout, stderr}. Output is capped like run_command."""
    result = await _run([sys.executable, "-"], input=code, timeout=timeout)
    result["stdout"], _ = _cap(result["stdout"])
    result["stderr"], _ = _cap(result["stderr"])
    return result


# ---------- Git -------------------------------------------------------------


def _ref(value: str, what: str) -> str:
    if not value or value.startswith("-"):
        raise ValueError(f"invalid {what}: {value!r}")
    return value


def _git_paths(paths: Optional[List[str]]) -> List[str]:
    return ["--", *(_rel(_resolve(p)) for p in paths)] if paths else []


async def _git(*args: str) -> str:
    result = await _run(["git", *args])
    if result["returncode"] != 0:
        raise RuntimeError(result["stderr"].strip() or result["stdout"].strip() or "git failed")
    text, cut = _cap((result["stdout"] or result["stderr"]).strip())
    return text + ("\n[output truncated]" if cut else "")


async def git_status() -> str:
    """Short `git status` of the root's repository."""
    return await _git("status", "--short")


async def git_add(files: List[str]) -> str:
    """Stage the given files."""
    return await _git("add", "--", *(_rel(_resolve(f)) for f in files))


async def git_log(limit: int = 10) -> str:
    """The most recent commits, one per line."""
    return await _git("log", f"-n{int(limit)}", "--pretty=format:%h %s")


async def git_diff(
    staged: bool = False, paths: Optional[List[str]] = None, rev: Optional[str] = None
) -> str:
    """Show changes as a unified diff. Default: unstaged changes; staged=true: what would be committed; rev: changes of the working tree relative to that revision. Optionally limited to paths."""
    args = ["diff"]
    if staged:
        args.append("--cached")
    if rev:
        args.append(_ref(rev, "rev"))
    return await _git(*args, *_git_paths(paths))


async def git_show(rev: str = "HEAD", stat_only: bool = False) -> str:
    """Show a commit (message and diff); stat_only=true shows just the changed-files summary."""
    args = ["show", _ref(rev, "rev")]
    if stat_only:
        args.append("--stat")
    return await _git(*args)


async def git_branch() -> str:
    """List local branches (the current one is starred)."""
    return await _git("branch", "--list", "--verbose")


async def git_checkout(ref: str, create: bool = False) -> str:
    """Switch to a branch or revision; create=true makes a new branch first. Fails rather than discarding uncommitted changes that would be overwritten."""
    args = ["checkout"]
    if create:
        args.append("-b")
    return await _git(*args, _ref(ref, "ref"))


async def git_commit(message: str) -> str:
    """Commit the staged changes with the given message."""
    return await _git("commit", "-m", message)


async def git_push() -> str:
    """Push the current branch."""
    return await _git("push")


# ---------- Long-running processes ------------------------------------------


@dataclass
class _Managed:
    proc: "asyncio.subprocess.Process"
    out: str = ""
    err: str = ""
    dropped: bool = False
    tasks: List["asyncio.Task[None]"] = field(default_factory=list)


_processes: Dict[str, _Managed] = {}


def _kill_all_processes() -> None:
    for m in _processes.values():
        _kill_group(m.proc)


atexit.register(_kill_all_processes)


async def _pump(m: _Managed, stream: "asyncio.StreamReader", attr: str) -> None:
    while True:
        chunk = await stream.read(4096)
        if not chunk:
            return
        text = getattr(m, attr) + chunk.decode(errors="replace")
        if len(text) > PROCESS_BUFFER_LIMIT:
            text = text[-PROCESS_BUFFER_LIMIT:]
            m.dropped = True
        setattr(m, attr, text)


def _managed(id: str) -> _Managed:
    try:
        return _processes[id]
    except KeyError:
        raise ValueError(f"unknown process id {id!r}") from None


async def process_start(
    command: str, cwd: Optional[str] = None, env: Optional[Dict[str, str]] = None
) -> ProcessStarted:
    """Start a long-running shell command in the background (dev server, watcher, slow build) and return its id. Read its output with process_read, stop it with process_kill. At most 16 at once; all are killed when the harness exits."""
    for pid, m in list(_processes.items()):  # forget finished, fully-read processes
        if m.proc.returncode is not None and not m.out and not m.err:
            del _processes[pid]
    if len(_processes) >= MAX_PROCESSES:
        raise RuntimeError(f"too many background processes ({MAX_PROCESSES}); kill one first")
    merged = {**os.environ, **env} if env else None
    proc = await asyncio.create_subprocess_exec(
        "bash",
        "-c",
        command,
        cwd=str(_resolve(cwd) if cwd else _root),
        env=merged,
        stdin=asyncio.subprocess.DEVNULL,
        stdout=asyncio.subprocess.PIPE,
        stderr=asyncio.subprocess.PIPE,
        start_new_session=True,
    )
    m = _Managed(proc)
    m.tasks = [
        asyncio.ensure_future(_pump(m, proc.stdout, "out")),
        asyncio.ensure_future(_pump(m, proc.stderr, "err")),
    ]
    pid = uuid.uuid4().hex[:8]
    _processes[pid] = m
    return ProcessStarted(id=pid, pid=proc.pid)


async def process_read(id: str, wait: float = 0.0) -> ProcessOutput:
    """Return the output a background process produced since the last read, and whether it is still running (with its exit code once done). wait (seconds, max 30) pauses first so a command can produce output."""
    m = _managed(id)
    if wait > 0:
        try:
            await asyncio.wait_for(asyncio.shield(m.proc.wait()), min(wait, 30.0))
        except asyncio.TimeoutError:
            pass
    if m.proc.returncode is not None:
        await asyncio.gather(*m.tasks, return_exceptions=True)  # drain what is left
    out, m.out = _cap(m.out)[0], m.out[_max_output:]
    err, m.err = _cap(m.err)[0], m.err[_max_output:]
    dropped, m.dropped = m.dropped, False
    return ProcessOutput(
        id=id, stdout=out, stderr=err, running=m.proc.returncode is None,
        exit_code=m.proc.returncode, dropped=dropped,
    )


async def process_kill(id: str) -> ProcessKilled:
    """Kill a background process and everything it spawned. Unread output stays available to process_read."""
    m = _managed(id)
    if m.proc.returncode is not None:
        return ProcessKilled(id=id, killed=False, message=f"already exited with code {m.proc.returncode}")
    _kill_group(m.proc)
    await m.proc.wait()
    await asyncio.gather(*m.tasks, return_exceptions=True)
    return ProcessKilled(id=id, killed=True, message="killed")


# ---------- Registration ------------------------------------------------------

# (function, readOnly, destructive, idempotent, openWorld). Clients use these
# hints to auto-approve reads and to confirm before anything destructive.
TOOLS: List[Tuple[Any, ToolAnnotations]] = [
    (fn, ToolAnnotations(readOnlyHint=ro, destructiveHint=None if ro else destructive, idempotentHint=idem, openWorldHint=open_world))
    for fn, ro, destructive, idem, open_world in [
        # files
        (file_read, True, False, True, False),
        (dir_list, True, False, True, False),
        (tree_of_files, True, False, True, False),
        (find_files, True, False, True, False),
        (ripgrep, True, False, True, False),
        (read_lines, True, False, True, False),
        (file_write, False, True, False, False),  # overwrites
        (edit_file, False, True, False, False),
        (file_move, False, False, False, False),  # never overwrites
        (file_delete, False, True, True, False),
        # code execution: anything can happen
        (run_command, False, True, False, True),
        (run_python, False, True, False, True),
        (process_start, False, True, False, True),
        (process_read, True, False, False, False),  # consumes buffered output
        (process_kill, False, True, True, False),
        # git
        (git_status, True, False, True, False),
        (git_log, True, False, True, False),
        (git_diff, True, False, True, False),
        (git_show, True, False, True, False),
        (git_branch, True, False, True, False),
        (git_add, False, False, True, False),
        (git_checkout, False, True, False, False),
        (git_commit, False, False, False, False),
        (git_push, False, True, False, True),  # publishes to a remote
    ]
]


_EXPECTED_ERRORS = (OSError, ValueError, RuntimeError, UnicodeError)


def _report_errors(fn: Any) -> Any:
    """Turn the errors a tool raises on purpose into ``ToolError``.

    The SDK hides the message of any other exception from the model ("Error
    executing tool X"), which would throw away exactly the reason it needs
    ("outside the allowed root", "occurs 2 times"). ``ToolError`` is delivered
    as the error text of the result. The wrapper keeps the signature (and
    async-ness), so the advertised schemas are unchanged.
    """

    def describe(e: Exception) -> ToolError:
        return ToolError(f"{type(e).__name__}: {e}" if not isinstance(e, RuntimeError) else str(e))

    if inspect.iscoroutinefunction(fn):

        @functools.wraps(fn)
        async def wrapper(*args: Any, **kwargs: Any) -> Any:
            try:
                return await fn(*args, **kwargs)
            except _EXPECTED_ERRORS as e:
                raise describe(e) from e

    else:

        @functools.wraps(fn)
        def wrapper(*args: Any, **kwargs: Any) -> Any:
            try:
                return fn(*args, **kwargs)
            except _EXPECTED_ERRORS as e:
                raise describe(e) from e

    return wrapper


def build_server(name: str = "harness") -> MCPServer:
    server = MCPServer(name, version=__version__)
    for fn, annotations in TOOLS:
        server.tool(annotations=annotations)(_report_errors(fn))
    return server


def main() -> None:
    import argparse

    parser = argparse.ArgumentParser(description="MCP harness server (stdio)")
    parser.add_argument("--name", default="harness")
    parser.add_argument(
        "--root",
        default=os.environ.get(f"{ENV_PREFIX}ROOT"),
        help=(
            "Directory that file tools are confined to (default: the current directory, "
            f"or {ENV_PREFIX}ROOT). Use / to lift the restriction."
        ),
    )
    parser.add_argument(
        "--max-output",
        type=int,
        default=int(os.environ.get(f"{ENV_PREFIX}MAX_OUTPUT", DEFAULT_MAX_OUTPUT)),
        help=f"Cap, in characters, on each output stream or file read (default {DEFAULT_MAX_OUTPUT})",
    )
    args = parser.parse_args()
    try:
        configure(args.root, args.max_output)
    except ValueError as e:
        parser.error(str(e))
    build_server(args.name).run("stdio")


if __name__ == "__main__":
    main()
