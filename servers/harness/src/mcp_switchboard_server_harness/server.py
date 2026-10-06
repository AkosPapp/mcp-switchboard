"""The harness MCP server.

A plain stdio MCP server, so it is tunnelled like any other: reference it from
an ``mcp.json`` as ``uvx mcp-switchboard-server-harness`` (the client also adds
it by default). The tool functions are ordinary module-level functions so they
can be unit-tested directly; everything that runs a subprocess is ``async`` so
one slow command never stalls the server.

**Confinement — what it is and what it is not.** File tools resolve every path
(following symlinks) and accept only the *root* (default: the working directory,
set with ``--root`` / ``MCP_SWITCHBOARD_HARNESS_ROOT``), the *scratch* directory
(``<root>/.harness/scratch`` by default; set with ``--scratch`` /
``MCP_SWITCHBOARD_HARNESS_SCRATCH``; disposable, not persisted across sessions),
and the system temp directory (``$TMPDIR``, else ``/tmp``). That set is a
convention against mistakes and path surprises, **not** a security boundary:
``run_command``, ``run_python`` and the ``process_*`` tools run arbitrary code as
the launching user with that user's full access — anything they can reach
(``~/.git-credentials``, your ssh keys, any readable file) is reachable, and the
file-tool confines deliberately let through exactly what those can write, so the
two paths never disagree. Treat everything outside the writable set as readable
by shell, not blocked. Only expose the hub's ``/mcp`` endpoints to consumers you
trust; a real sandbox (mount namespace / separate uid) is a hub/OS feature, see
docs/SANDBOX_PROPOSAL.md.
"""

from __future__ import annotations

import asyncio
import atexit
import base64
import binascii
import csv
import difflib
import fnmatch
import functools
import inspect
import json
import os
import re
import shlex
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import uuid
from collections import OrderedDict
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
# Directories a recursive dir_list lists but does not descend into: dependency
# and cache trees that are huge and never what the caller is after. Naming one
# as the path itself still lists it.
_NO_DESCEND = _SKIP_DIRS | {".direnv", ".mypy_cache", ".pytest_cache", ".ruff_cache", ".tox", ".cache", "venv"}
DEFAULT_LIST_LIMIT = 500


# ---------- Configuration and path confinement -----------------------------

_root: Path = Path.cwd().resolve()
_max_output: int = DEFAULT_MAX_OUTPUT
_scratch: Optional[Path] = None
SCRATCH_SUBPATH = ".harness/scratch"


def scratch_dir() -> Path:
    """The writable scratch directory: ``<root>/{SCRATCH_SUBPATH}`` unless set otherwise."""
    return _scratch if _scratch is not None else _root / SCRATCH_SUBPATH


def temp_dir() -> Path:
    """The system temp directory file tools are also allowed to reach (respects $TMPDIR)."""
    return Path(tempfile.gettempdir()).resolve()


def configure(
    root: Optional[str] = None, max_output: Optional[int] = None, scratch: Optional[str] = None
) -> None:
    """Set the confinement root, output cap, and/or scratch directory; None leaves a
    setting unchanged (the root starts as the current directory, the scratch as
    ``<root>/.harness/scratch``). The scratch may live outside the root; it is created
    on first use."""
    global _root, _max_output, _scratch
    if root is not None:
        new_root = Path(root).resolve()
        if not new_root.is_dir():
            raise ValueError(f"root is not a directory: {new_root}")
        _root = new_root
    if max_output is not None:
        if max_output < 1:
            raise ValueError("max output must be positive")
        _max_output = max_output
    if scratch is not None:
        new_scratch = Path(scratch).resolve()
        if new_scratch.exists() and not new_scratch.is_dir():
            raise ValueError(f"scratch is not a directory: {new_scratch}")
        _scratch = new_scratch


def _resolve(path: str) -> Path:
    """Resolve ``path`` (relative to the root) and ensure it stays inside the root, the
    scratch directory, or the system temp directory.

    ``realpath`` semantics: symlinks are followed even for the parts of the path
    that exist, so a link pointing outside is refused. Referencing the scratch
    directory creates it on demand, before or after resolution.
    """
    p = Path(path)
    if not p.is_absolute():
        p = _root / p
    resolved = Path(os.path.realpath(p))
    in_root = resolved == _root or _root in resolved.parents
    special = scratch_dir()
    in_scratch = resolved == special or special in resolved.parents
    if in_scratch and not special.is_dir():
        special.mkdir(parents=True, exist_ok=True)
    temp = temp_dir()
    in_temp = resolved == temp or temp in resolved.parents
    if not (in_root or in_scratch or in_temp):
        raise PermissionError(
            f"{path!r} is outside the allowed locations: root {_root}, scratch {special}, "
            f"temp {temp} (only the run_* / process_* tools escape; they are unconfined)"
        )
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


_inflight: "set[asyncio.subprocess.Process]" = set()


def _kill_inflight() -> None:
    for proc in list(_inflight):
        _kill_group(proc)


def _clamp_timeout(timeout: Optional[float]) -> float:
    if timeout is None:
        return DEFAULT_TIMEOUT
    if timeout <= 0:
        raise ValueError("timeout must be positive")
    return min(timeout, MAX_TIMEOUT)


TIMEOUT_EXIT_CODE = 124  # sentinel for "killed at the time limit" (GNU timeout's code)


async def _run(
    argv: List[str],
    cwd: Optional[Path] = None,
    input: Optional[str] = None,
    env: Optional[Dict[str, str]] = None,
    timeout: Optional[float] = None,
) -> Dict[str, Any]:
    """Run a command to completion; on timeout the whole process group is killed.

    A timeout does NOT discard output: the result then carries whatever the
    command had produced (P0-B — "never discard output for being late") plus
    ``timed_out``, ``elapsed_s``, ``applied_timeout_s`` and the
    ``TIMEOUT_EXIT_CODE`` sentinel as ``returncode``. The output streams are
    pumped into caller-visible buffers rather than a coroutine-local
    ``communicate``, so cancelling at the deadline cannot lose the last chunk.
    """
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
    _inflight.add(proc)
    stdout_parts: List[bytes] = []
    stderr_parts: List[bytes] = []

    async def pump(stream, parts: List[bytes]) -> None:
        while True:
            chunk = await stream.read(65536)
            if not chunk:
                return
            parts.append(chunk)

    async def feed() -> None:
        if input is None:
            return
        try:
            proc.stdin.write(input.encode())
            await proc.stdin.drain()
        except (BrokenPipeError, ConnectionResetError):
            pass
        proc.stdin.close()

    async def done() -> None:
        await feed()
        await proc.wait()

    started = time.monotonic()
    collect = asyncio.gather(
        pump(proc.stdout, stdout_parts), pump(proc.stderr, stderr_parts), done()
    )
    try:
        await asyncio.wait_for(asyncio.shield(collect), seconds)
    except asyncio.TimeoutError:
        _kill_group(proc)
        # The group is dead, so the (still-running) pumps should now hit EOF and
        # collect whatever the killed children wrote, with a hard bound of ours.
        try:
            await asyncio.wait_for(asyncio.shield(collect), 5)
        except asyncio.TimeoutError:
            collect.cancel()  # e.g. a child stuck in an uninterruptible syscall
        except (BrokenPipeError, ConnectionResetError, OSError):
            pass
        await proc.wait()
        return {
            "returncode": TIMEOUT_EXIT_CODE,
            "stdout": b"".join(stdout_parts).decode(errors="replace"),
            "stderr": b"".join(stderr_parts).decode(errors="replace"),
            "timed_out": True,
            "elapsed_s": round(time.monotonic() - started, 3),
            "applied_timeout_s": seconds,
        }
    except asyncio.CancelledError:
        collect.cancel()
        _kill_group(proc)
        raise
    finally:
        _inflight.discard(proc)
    return {
        "returncode": proc.returncode,
        "stdout": b"".join(stdout_parts).decode(errors="replace"),
        "stderr": b"".join(stderr_parts).decode(errors="replace"),
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
    truncated: bool = Field(default=False, description="stdout or stderr was cut at the output limit, or the command was killed at the timeout.")
    stdout_total: int = Field(default=0, description="Full length of stdout in characters, before truncation.")
    stderr_total: int = Field(default=0, description="Full length of stderr in characters, before truncation.")
    output_id: Optional[str] = Field(
        default=None,
        description="Set when the returned output is not the complete output: page through the full text with output_read / output_grep.",
    )
    timed_out: bool = Field(
        default=False,
        description="True when the command was killed at the applied timeout; stdout/stderr are then everything it produced up to the kill and exit_code is the sentinel 124.",
    )
    elapsed_s: Optional[float] = Field(default=None, description="Seconds from start to the kill (or exit).")
    applied_timeout_s: Optional[float] = Field(default=None, description="The timeout actually applied (requests above 3600 are clamped).")


class FileReadResult(BaseModel):
    content: Optional[str] = Field(default=None, description="Base64 of the bytes read; set when binary=true.")
    raw_text: Optional[str] = Field(default=None, description="UTF-8 text read; set when binary=false.")
    size: int = Field(description="Total size of the file in bytes.")
    offset: int = Field(description="Where the read started (bytes for binary, characters for text).")
    truncated: bool = Field(description="More data remains after this chunk; read again with a larger offset.")


class ReadLinesResult(BaseModel):
    text: str = Field(
        description="The selected lines joined with newlines, byte-faithful to the file the way file_read is: no per-line trimming, CRLF normalised by text mode, and the file's final newline kept when the selection reaches the end of the file. Paginating without `end` is stitching-safe: consecutive pages concatenate verbatim; an explicit mid-file range `end` has no trailing newline, so join such pages with \"\\n\"."
    )
    start: int = Field(description="First line delivered (1-based).")
    end: int = Field(description="Last line delivered in full (1-based), so resume with start=end+1; start-1 when nothing was delivered.")
    total_lines: int = Field(description="Total lines in the file.")
    truncated: bool = Field(description="More lines remain after `end`; read again with start=end+1.")


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
    truncated: bool = Field(default=False, description="More entries exist than `limit`; list a narrower path or raise limit.")


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


class WatcherStarted(BaseModel):
    id: str


class WatcherPoll(BaseModel):
    id: str
    done: bool = Field(description="True once the watch ended: satisfied or out of time. Once true it never flips back.")
    satisfied: bool = Field(default=False, description="True when the predicate last exited non-zero (its 'condition cleared' signal) before the timeout ran out.")
    exit_code: Optional[int] = Field(default=None, description="The predicate's last exit code, once done (124 when the final predicate attempt itself timed out).")
    elapsed_s: float = Field(description="Seconds since the watch started.")
    polls: int = Field(description="Completed predicate runs so far.")
    stdout: str = Field(description="Predicate stdout produced since the previous poll.")
    stderr: str
    dropped: bool = Field(default=False, description="Older output was discarded because the buffer filled.")


# ---------- Shell, files ---------------------------------------------------


async def run_command(
    command: str,
    cwd: Optional[str] = None,
    env: Optional[Dict[str, str]] = None,
    timeout: Optional[float] = None,
    mode: str = "full",
    pattern: Optional[str] = None,
) -> CommandResult:
    """Execute a shell command (via `bash -c`). cwd defaults to the root; env entries override
    the inherited environment; timeout is in seconds (default 120, max 3600 — that is a harness
    limit; when the call arrives through the hub, the hub's per-call ceiling CALL_TIMEOUT binds
    FIRST regardless of `timeout`, and when it fires the whole result is lost), and a timeout
    kills the command and its children. A timed-out call returns a PARTIAL result — everything
    produced so far with timed_out=true and exit_code=124 — never an error. For waits longer
    than you can safely block, use wait_for_start / wait_for_poll instead. mode shapes big
    output: "full" (default, capped), "head" / "tail" (first / last 100 lines), "grep" (only
    lines matching the regex `pattern`, plus 2 lines of context). When output is cut,
    `output_id` is returned: read the rest with output_read or output_grep.
    Example: run_command(command="pytest -q", mode="tail")"""
    _check_mode(mode, pattern)
    merged = {**os.environ, **env} if env else None
    result = await _run(
        ["bash", "-c", command], cwd=_resolve(cwd) if cwd else None, env=merged, timeout=timeout
    )
    out, err, cut, output_id = _shape_result(result["stdout"], result["stderr"], mode, pattern)
    timed_out = bool(result.get("timed_out"))
    return CommandResult(
        stdout=out,
        stderr=err,
        exit_code=result["returncode"],
        truncated=cut or timed_out,
        stdout_total=len(result["stdout"]),
        stderr_total=len(result["stderr"]),
        output_id=output_id,
        timed_out=timed_out,
        elapsed_s=result.get("elapsed_s"),
        applied_timeout_s=result.get("applied_timeout_s"),
    )


def file_read(
    path: str, binary: bool = False, offset: int = 0, limit: Optional[int] = None
) -> FileReadResult:
    """Read a file. With binary=true the bytes come back base64-encoded in `content`; otherwise UTF-8 text in `raw_text`. Output is capped (default 100000): use offset and limit (bytes for binary, characters for text) to page through large files. Paths must stay inside the root, the scratch or the system temp directory. Example: file_read(path="src/main.py")"""
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
    # Stream: never hold more than one block plus the requested window.
    with open(p, "r") as f:
        skip = offset
        while skip > 0:
            got = f.read(min(skip, 65536))
            if not got:
                break
            skip -= len(got)
        chunk = f.read(want) if skip == 0 else ""
        truncated = skip == 0 and len(chunk) == want and f.read(1) != ""
    return FileReadResult(raw_text=chunk, size=size, offset=offset, truncated=truncated)


def file_write(path: str, content: str, append: bool = False, binary: bool = False) -> FileWriteResult:
    """Write a file, creating parent directories. With binary=true, `content` is base64 and is decoded to bytes; otherwise it is written as UTF-8 text. Overwrites are atomic; append=true appends instead. Writes land inside the root, the scratch or the system temp directory only (scratch is the right place for throwaway output). Example: file_write(path="notes/a.txt", content="hello\\n")"""
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


def file_delete(path: str, recursive: bool = False) -> FileDeleteResult:
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


def dir_list(path: str, recursive: bool = False, limit: int = DEFAULT_LIST_LIMIT) -> DirListResult:
    """List a directory's entries with size, type and mtime; recursive=true descends into sub-folders (symlinks are not followed, and dependency/cache folders such as .git, node_modules, .venv and .direnv are listed but not entered). At most `limit` entries (default 500) come back; `truncated` says if there were more, so list a narrower path instead of the whole tree. Example: dir_list(path="src", recursive=true, limit=200)"""
    base = _resolve(path)
    if not base.is_dir():
        raise NotADirectoryError(path)
    if limit < 1:
        raise ValueError("limit must be positive")
    paths: List[Path] = []
    truncated = False
    if recursive:
        for dirpath, dirnames, filenames in os.walk(base):
            dirnames.sort()
            for n in [*dirnames, *sorted(filenames)]:
                if len(paths) >= limit:
                    truncated = True
                    break
                paths.append(Path(dirpath) / n)
            if truncated:
                break
            dirnames[:] = [d for d in dirnames if d not in _NO_DESCEND]
    else:
        names = sorted(base.iterdir())
        truncated = len(names) > limit
        paths = names[:limit]
    return DirListResult(entries=[_entry(p) for p in paths], truncated=truncated)


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
    """Find files under root whose path or name matches a glob such as "**/*.py". Inside a git repository .gitignore is honoured; otherwise .git, node_modules, __pycache__ and .venv are skipped. Returns at most `limit` root-relative paths, sorted. Example: find_files(pattern="**/*.py", root="src")"""
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
    """Search file contents with a regex (ripgrep patterns; falls back to a built-in scanner when ripgrep is not installed, honoring .gitignore through `git ls-files`). glob filters files (e.g. "*.py"), context adds N surrounding lines per match, and at most max_results lines are returned (`truncated` says if more existed). Example: ripgrep(query="def main", glob="*.py", context=2)"""
    target = _resolve(path)
    if not shutil.which("rg"):
        return await _py_search(query, target, glob, ignore_case, context, max_results)
    argv = ["rg", "--json"]
    if ignore_case:
        argv.append("--ignore-case")
    if glob:
        argv += ["--glob", glob]
    if context > 0:
        argv += ["--context", str(min(context, 20))]
    argv += ["--max-count", str(max(max_results, 0) + 1)]  # per file; one extra detects truncation
    argv += ["--", query, str(target)]
    seconds = _clamp_timeout(None)
    try:
        proc = await asyncio.create_subprocess_exec(
            *argv,
            cwd=str(_root),
            stdin=asyncio.subprocess.DEVNULL,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
            start_new_session=True,
            limit=1 << 24,
        )
    except FileNotFoundError as e:
        raise RuntimeError("command not found: rg") from e
    _inflight.add(proc)
    matches: List[GrepMatch] = []
    truncated = False

    async def drain_err() -> bytes:
        buf = b""
        while True:
            chunk = await proc.stderr.read(4096)
            if not chunk:
                return buf
            buf = (buf + chunk)[:10000]

    async def consume() -> None:
        nonlocal truncated
        while True:
            raw = await proc.stdout.readline()
            if not raw:
                return
            try:
                obj = json.loads(raw)
            except ValueError:
                continue
            if obj.get("type") not in ("match", "context"):
                continue
            if len(matches) >= max_results:
                truncated = True
                _kill_group(proc)
                return
            data = obj["data"]
            matches.append(
                GrepMatch(
                    file=_rel(Path(data["path"].get("text", ""))),
                    line_no=data["line_number"],
                    text=data["lines"].get("text", "").rstrip("\n"),
                    is_match=obj["type"] == "match",
                )
            )

    err_task = asyncio.ensure_future(drain_err())
    try:
        await asyncio.wait_for(consume(), seconds)
        await proc.wait()
    except asyncio.TimeoutError as e:
        _kill_group(proc)
        await proc.wait()
        raise RuntimeError(f"command timed out after {seconds:g}s: rg") from e
    except BaseException:
        _kill_group(proc)
        raise
    finally:
        _inflight.discard(proc)
        err = (await asyncio.gather(err_task, return_exceptions=True))[0]
    if not truncated and proc.returncode not in (0, 1):  # 1 means no matches
        text = err.decode(errors="replace").strip() if isinstance(err, bytes) else ""
        raise RuntimeError(text or "ripgrep failed")
    return GrepResult(matches=matches, truncated=truncated)


async def _py_search(
    query: str, target: Path, glob: Optional[str], ignore_case: bool, context: int, max_results: int
) -> GrepResult:
    """Built-in content search for hosts without ripgrep, so a missing optional
    binary never leaves an agent unable to grep. Same contract as `ripgrep`
    (regex, glob filter, context lines, max_results); walks git-tracked files
    where possible. Scans at most _PY_SEARCH_BYTES per file (skips binaries)."""
    try:
        rx = re.compile(query, re.IGNORECASE if ignore_case else 0)
    except re.error as e:
        raise RuntimeError(f"invalid regex: {e}") from e
    if target.is_file():
        files = [target]
    else:
        tracked = await _tracked_files(target)
        if tracked is None:
            tracked = []
            for dirpath, dirnames, filenames in os.walk(target):
                dirnames[:] = [d for d in dirnames if d not in _NO_DESCEND]
                tracked.extend(Path(dirpath) / n for n in filenames)
        files = tracked
    matches: List[GrepMatch] = []
    truncated = False
    for f in files:
        if glob and not (fnmatch.fnmatch(f.name, glob) or fnmatch.fnmatch(_rel(f), "**/" + glob.lstrip("*/"))):
            continue
        try:
            with open(f, "r") as fh:
                lines = fh.read(_PY_SEARCH_BYTES + 1).splitlines()
        except (OSError, UnicodeDecodeError):
            continue
        hit_lines = [i for i, ln in enumerate(lines) if rx.search(ln)]
        if not hit_lines:
            continue
        want: Dict[int, bool] = {}
        for i in hit_lines:
            want[i] = True
            for d in range(1, min(context, 20) + 1):
                for j in (i - d, i + d):
                    if 0 <= j < len(lines):
                        want.setdefault(j, False)
        for i in sorted(want):
            if len(matches) >= max_results:
                truncated = True
                return GrepResult(matches=matches, truncated=truncated)
            matches.append(GrepMatch(file=_rel(f), line_no=i + 1, text=lines[i], is_match=want[i]))
    return GrepResult(matches=matches, truncated=truncated)


_PY_SEARCH_BYTES = 2_000_000


def read_lines(path: str, start: int = 1, end: Optional[int] = None) -> ReadLinesResult:
    """Text of lines start..end (1-based, inclusive) of a file, newline-faithful.

    The result mirrors file_read's fidelity: `text` is returned as written, no line is
    trimmed, and reading the whole file reproduces file_read's `raw_text` exactly
    (including the final newline, when the file has one). Output is capped like every
    other tool; `truncated` and `end` tell you where to continue (start=end+1).
    To page a file, omit `end` and repeat with start=end+1: pages concatenate
    verbatim. An explicit mid-file `end` returns a selection without a trailing
    newline (join such pages with "\\n"). A single line longer than the cap
    cannot be paged here — use file_read offsets. end defaults to the last line.
    Example: read_lines(path="src/main.py", start=40, end=80)
    """
    first = max(start, 1)
    if end is not None and end < first:
        raise ValueError("end must not be before start")
    with open(_resolve(path), "r") as f:
        content = f.read()
    if content:
        ends_nl = content.endswith("\n")
        lines = content.split("\n")
        if ends_nl:
            lines.pop()
    else:
        ends_nl, lines = False, []
    total = len(lines)
    last = total if end is None else min(end, total)
    selected = lines[first - 1 : last] if first <= last else []
    kept, used = 0, 0
    for ln in selected:
        extra = len(ln) + (1 if kept else 0)
        if used + extra > _max_output:
            break
        used += extra
        kept += 1
    delivered_end = first + kept - 1 if kept else first - 1
    # A page needs a trailing newline when the selection reached the file's last
    # line and the file has one (fidelity: `text` is then a byte-exact suffix of
    # the file text), or when the cap cut a line off — reserving that byte
    # stitches consecutive pages together verbatim.
    eol = bool(kept) and (kept < len(selected) or (delivered_end == total and ends_nl))
    if eol and used + 1 > _max_output:
        kept -= 1
        delivered_end = first + kept - 1 if kept else first - 1
        eol = bool(kept) and kept < len(selected)
    text = "\n".join(selected[:kept]) + ("\n" if eol else "")
    return ReadLinesResult(
        text=text,
        start=first,
        end=delivered_end,
        total_lines=total,
        truncated=delivered_end < total,
    )


DIFF_CAP = 4000  # characters of diff echoed back by the edit tools


def _diff(label: str, old: str, new: str, cap: int = DIFF_CAP) -> str:
    """A compact unified diff (2 lines of context), cut at ``cap`` characters."""
    def split(t: str) -> List[str]:
        return [l if l.endswith("\n") else l + "\n" for l in t.splitlines(keepends=True)]

    text = "".join(
        difflib.unified_diff(
            split(old), split(new),
            fromfile=f"a/{label}", tofile=f"b/{label}", n=2,
        )
    )
    if text and not text.endswith("\n"):
        text += "\n"
    if len(text) > cap:
        text = text[:cap] + "\n[diff truncated]\n"
    return text


def _replace_once(text: str, old_str: str, new_str: str, replace_all: bool, where: str) -> Tuple[str, int]:
    if not old_str:
        raise ValueError(f"{where}old_str must not be empty")
    count = text.count(old_str)
    if count == 0:
        raise ValueError(f"{where}{old_str!r} not found")
    if count > 1 and not replace_all:
        raise ValueError(
            f"{where}{old_str!r} occurs {count} times; add context to make it unique or set replace_all"
        )
    return text.replace(old_str, new_str), count


def _plural(n: int) -> str:
    return f"{n} occurrence{'s' if n != 1 else ''}"


def edit_file(
    path: str,
    new_content: Optional[str] = None,
    old_str: Optional[str] = None,
    new_str: Optional[str] = None,
    replace_all: bool = False,
    dry_run: bool = False,
) -> str:
    """Edit an existing file: either overwrite it with new_content, or replace old_str with new_str. old_str must occur exactly once unless replace_all is true (an ambiguous match is an error, so the wrong spot is never edited silently). Writes are atomic. Returns a one-line summary followed by a unified diff of the change; dry_run=true checks and shows the diff without writing. Example: edit_file(path="a.py", old_str="x = 1", new_str="x = 2")"""
    p = _resolve(path)
    if not p.is_file():
        raise FileNotFoundError(str(path))
    text = p.read_text()
    if new_content is not None:
        new_text, summary = new_content, "full content"
    else:
        if old_str is None or new_str is None:
            raise ValueError("provide new_content, or both old_str and new_str")
        try:
            new_text, count = _replace_once(text, old_str, new_str, replace_all, "")
        except ValueError as e:
            raise ValueError(f"{e} in {path}") from None
        summary = f"{_plural(count)}"
    if not dry_run:
        _atomic_write(p, new_text.encode())
    diff = _diff(_rel(p), text, new_text)
    if new_content is not None:
        head = "would write" if dry_run else "wrote"
        head += " full content"
    else:
        head = f"{'would replace' if dry_run else 'replaced'} {summary}"
    if dry_run:
        head = "dry run: " + head
    return f"{head}\n{diff}" if diff else f"{head} (no change)"


def multi_edit(path: str, edits: List[Dict[str, Any]], dry_run: bool = False) -> str:
    """Apply several replacements to one file, in order and atomically: if any edit fails nothing is written. `edits` is a list of {"old_str", "new_str", optional "replace_all"}; each old_str must match exactly once (in the text as changed by the earlier edits) unless replace_all is true. Returns a summary and a unified diff; dry_run=true only checks. Example: multi_edit(path="a.py", edits=[{"old_str": "foo", "new_str": "bar"}, {"old_str": "1", "new_str": "2", "replace_all": true}])"""
    p = _resolve(path)
    if not p.is_file():
        raise FileNotFoundError(str(path))
    if not edits:
        raise ValueError("edits must not be empty")
    text = p.read_text()
    new_text, total = text, 0
    for i, e in enumerate(edits, 1):
        if not isinstance(e, dict) or "old_str" not in e or "new_str" not in e:
            raise ValueError(f"edit {i}: needs old_str and new_str")
        new_text, count = _replace_once(
            new_text, str(e["old_str"]), str(e["new_str"]), bool(e.get("replace_all", False)), f"edit {i}: "
        )
        total += count
    if not dry_run:
        _atomic_write(p, new_text.encode())
    head = f"{'dry run: would apply' if dry_run else 'applied'} {len(edits)} edit{'s' if len(edits) != 1 else ''} ({_plural(total)})"
    diff = _diff(_rel(p), text, new_text)
    return f"{head}\n{diff}" if diff else f"{head} (no change)"


async def run_python(
    code: str, timeout: Optional[float] = None, mode: str = "full", pattern: Optional[str] = None
) -> Dict[str, Any]:
    """Run a Python snippet in a fresh interpreter (cwd = the root) and return {returncode, stdout, stderr}. Output is capped like run_command; mode ("full", "head", "tail", "grep" with `pattern`) shapes it the same way, and an `output_id` (with truncated=true) is added when output was cut. On timeout you get everything produced up to the kill with timed_out=true and returncode=124 — nothing is discarded. Example: run_python(code="print(2+2)")"""
    _check_mode(mode, pattern)
    result = await _run([sys.executable, "-"], input=code, timeout=timeout)
    result["stdout"], result["stderr"], cut, output_id = _shape_result(
        result["stdout"], result["stderr"], mode, pattern
    )
    if cut:
        result["truncated"] = True
        result["output_id"] = output_id
    if result.get("timed_out"):
        result["truncated"] = True
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
    full = (result["stdout"] or result["stderr"]).strip()
    text, cut = _cap(full)
    if cut:
        text += f"\n[output truncated; full text: output_read(output_id=\"{_store_output(full)}\")]"
    return text


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
    """Show changes as a unified diff. Default: unstaged changes; staged=true: what would be committed; rev: changes of the working tree relative to that revision. Optionally limited to paths. Example: git_diff(staged=true, paths=["src/a.py"])"""
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


PROTECTED_BRANCHES = ("main", "master")

# Extra tool metadata on the wire (_meta), the MCP-sanctioned place for hints
# outside the four standard annotations. "switchboard.irreversible" marks a
# tool whose effect cannot be undone by this machine, so the hub gates it even
# under approval=never (W10); irreversibleReason explains the gate to the
# approver.
TOOL_META = {
    "git_push": {
        "switchboard": {
            "irreversible": True,
            "irreversibleReason": (
                "a push moves a branch on someone else's machine; it cannot be undone from here. "
                "The target remote and current branch are in this client's environment brief"
            ),
        }
    },
}


async def git_push() -> str:
    """Push the current branch to its remote. Irreversible (history moves on the remote), so the hub gates it even under approval=never; pushing straight to main/master is additionally flagged in the output. No force flags exist: this tool never forces."""
    branch = await _git("branch", "--show-current")
    out = await _git("push")
    if branch.strip() in PROTECTED_BRANCHES:
        out = (
            f"note: you pushed directly to the protected branch '{branch.strip()}'; "
            f"prefer a merge request there\n" + out
        )
    return out


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
    """Kill every managed background process group and any in-flight command."""
    for m in list(_processes.values()):
        _kill_group(m.proc)
    _kill_inflight()


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
    """Start a long-running shell command in the background (dev server, watcher, slow build) and return its id. Read its output with process_read, stop it with process_kill. At most 16 at once; all are killed when the harness exits. Wake contract: nothing is ever pushed and you cannot wake yourself — observe progress by calling process_read (its `wait` blocks up to 30 s inside one short call), or, for waits worth hours, start a predicate with wait_for_start and poll wait_for_poll; to notify the user when something finishes, keep polling in-turn and use switchboard_user_ask."""
    _forget_finished()
    if len(_processes) + len(_watchers) >= MAX_PROCESSES:
        raise RuntimeError(
            f"too many background processes and watches ({MAX_PROCESSES}); kill one first"
        )
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
    """Return the output a background process produced since the last read, and whether it is still running (with its exit code once done). wait (seconds, max 30) pauses first so a command can produce output. Pull only: this is how you observe a background process — nothing is pushed, and the process dies with the harness."""
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
    """Kill a background process and everything it spawned. Unread output stays available to process_read. Background processes are already gone when the harness exits; this is for stopping one mid-session."""
    m = _managed(id)
    if m.proc.returncode is not None:
        return ProcessKilled(id=id, killed=False, message=f"already exited with code {m.proc.returncode}")
    _kill_group(m.proc)
    await m.proc.wait()
    await asyncio.gather(*m.tasks, return_exceptions=True)
    return ProcessKilled(id=id, killed=True, message="killed")


# ---------- Watches (long waits without a blocking call) --------------------

# A wait must never be a blocking tool call: the tunnel's hub enforces a
# per-call deadline (CALL_TIMEOUT) well below any wait worth making (P0-B).
# So a watch is start/poll: wait_for_start kicks off a background loop that
# re-runs the predicate every `poll` seconds and is satisfied when a run exits
# non-zero; wait_for_poll reads its state. Each individual call is short, so no
# hub deadline can kill a wait, and a 24 h watch costs one subprocess per poll.

MAX_WATCH = 7 * 86400  # a watch runs at most one week (longer needs a scheduler)
PREDICATE_RUN_CAP = 60.0  # cap on one predicate run, seconds


@dataclass
class _Watcher:
    predicate: str
    poll_s: float
    timeout_s: float
    started: float
    done: bool = False
    satisfied: bool = False
    exit_code: Optional[int] = None
    polls: int = 0
    out: str = ""
    err: str = ""
    dropped: bool = False
    task: Optional["asyncio.Task[None]"] = None


_watchers: Dict[str, _Watcher] = {}


def _forget_finished() -> None:
    """Forget finished background processes / watches whose output was fully read."""
    for pid, m in list(_processes.items()):
        if m.proc.returncode is not None and not m.out and not m.err:
            del _processes[pid]
    for wid, w in list(_watchers.items()):
        if w.done and not w.out and not w.err:
            del _watchers[wid]


def _watcher(id: str) -> _Watcher:
    try:
        return _watchers[id]
    except KeyError:
        raise ValueError(f"unknown watcher id {id!r}") from None


def _watch_append(w: _Watcher, attr: str, text: str) -> None:
    merged = getattr(w, attr) + text
    if len(merged) > PROCESS_BUFFER_LIMIT:
        merged = merged[-PROCESS_BUFFER_LIMIT:]
        w.dropped = True
    setattr(w, attr, merged)


async def _watch_loop(w: _Watcher) -> None:
    """Re-run the predicate until a run exits non-zero or the timeout elapses."""
    while True:
        left = w.timeout_s - (time.monotonic() - w.started)
        if left <= 0:
            w.done = True  # satisfied stays False: the condition never cleared
            return
        try:
            r = await _run(
                ["bash", "-c", w.predicate], timeout=min(PREDICATE_RUN_CAP, max(left, 1.0))
            )
        except RuntimeError:  # e.g. bash missing; treat like an inconclusive check
            await asyncio.sleep(w.poll_s)
            continue
        w.polls += 1
        _watch_append(w, "out", r["stdout"])
        _watch_append(w, "err", r["stderr"])
        if not r.get("timed_out") and r["returncode"] != 0:
            # A hung predicate proves nothing; only a real non-zero exit
            # satisfies the watch ("wait until this stops being true").
            w.done, w.satisfied, w.exit_code = True, True, r["returncode"]
            return
        await asyncio.sleep(min(w.poll_s, max(w.timeout_s - (time.monotonic() - w.started), 0.0)))


async def wait_for_start(
    until: str, timeout: float = 3600, poll: float = 20
) -> WatcherStarted:
    """Start a background watch and return its id at once — nothing blocks, so no hub per-call deadline can kill a long wait. The shell predicate `until` (via bash -c) is re-run every `poll` seconds (min 0.1, default 20); the watch is satisfied when one run exits NON-ZERO, e.g. until="squeue -j 3046 -h | grep -q ." clears when the queue line disappears. timeout bounds the watch in seconds (default 3600, max 604800). Read progress with wait_for_poll: completion is NOT pushed to you. Counted against the same limit of 16 as background processes; killed when the harness exits. Example: wait_for_start(until="squeue -j 3046 -h | grep -q .", timeout=86400, poll=60), then wait_for_poll(id) now and then."""
    if not until.strip():
        raise ValueError("until must be a shell command")
    if not 0 < timeout <= MAX_WATCH:
        raise ValueError(f"timeout must be in (0, {MAX_WATCH:g}] seconds")
    if not 0.1 <= poll <= 300:
        raise ValueError("poll must be between 0.1 and 300 seconds")
    _forget_finished()  # finished, fully-read watches
    if len(_processes) + len(_watchers) >= MAX_PROCESSES:
        raise RuntimeError(
            f"too many background processes and watches ({MAX_PROCESSES}); kill or wait for one first"
        )
    w = _Watcher(predicate=until, poll_s=poll, timeout_s=timeout, started=time.monotonic())
    wid = uuid.uuid4().hex[:8]
    w.task = asyncio.ensure_future(_watch_loop(w))
    _watchers[wid] = w
    return WatcherStarted(id=wid)


async def wait_for_poll(id: str) -> WatcherPoll:
    """Return a wait_for_start watch's state: done (finished, satisfied or timed out), whether the predicate's NON-ZERO exit satisfied it, its exit code, elapsed seconds, poll count, and the predicate output produced since the previous poll. Pull only: completion is NOT pushed to you, and you cannot wake yourself — call this to observe progress. Watchers die when the harness exits; to notify the user when a watch ends, keep polling in-turn (a poll is a short call) and use switchboard_user_ask."""
    w = _watcher(id)
    out, w.out = _cap(w.out)[0], w.out[_max_output:]
    err, w.err = _cap(w.err)[0], w.err[_max_output:]
    dropped, w.dropped = w.dropped, False
    return WatcherPoll(
        id=id,
        done=w.done,
        satisfied=w.satisfied,
        exit_code=w.exit_code,
        elapsed_s=round(time.monotonic() - w.started, 3),
        polls=w.polls,
        stdout=out,
        stderr=err,
        dropped=dropped,
    )


# ---------- Big-output handling ---------------------------------------------

# Output that does not fit the cap is kept here so the model can page through it
# (output_read) or search it (output_grep) instead of re-running the command.
_OUTPUT_MAX_ENTRIES = 20
_OUTPUT_MAX_CHARS = 50_000_000  # total characters kept across all entries
_outputs: "OrderedDict[str, Dict[str, str]]" = OrderedDict()
_MODES = ("full", "head", "tail", "grep")
_SHAPE_LINES = 100  # lines returned by mode="head" / "tail"
_GREP_CONTEXT = 2


def _store_output(stdout: str = "", stderr: str = "") -> str:
    """Remember a full output in the LRU and return its id."""
    each = _OUTPUT_MAX_CHARS // 2
    output_id = uuid.uuid4().hex[:8]
    _outputs[output_id] = {"stdout": stdout[:each], "stderr": stderr[:each]}
    while len(_outputs) > 1 and (
        len(_outputs) > _OUTPUT_MAX_ENTRIES
        or sum(len(v["stdout"]) + len(v["stderr"]) for v in _outputs.values()) > _OUTPUT_MAX_CHARS
    ):
        _outputs.popitem(last=False)
    return output_id


def _stored(output_id: str, stream: str) -> str:
    if stream not in ("stdout", "stderr"):
        raise ValueError('stream must be "stdout" or "stderr"')
    try:
        entry = _outputs[output_id]
    except KeyError:
        raise ValueError(
            f"unknown output_id {output_id!r} (only the last {_OUTPUT_MAX_ENTRIES} large outputs are kept)"
        ) from None
    _outputs.move_to_end(output_id)
    return entry[stream]


def _compile(pattern: str) -> "re.Pattern[str]":
    try:
        return re.compile(pattern)
    except re.error as e:
        raise ValueError(f"invalid regex {pattern!r}: {e}") from None


def _grep_lines(
    lines: List[str], rx: "re.Pattern[str]", context: int, max_results: int
) -> Tuple[List[Tuple[int, str, bool]], bool]:
    """(line_no, text, is_match) for the first ``max_results`` matching lines plus
    ``context`` lines around each; the flag says more matches existed."""
    hits: List[int] = []
    truncated = False
    for i, line in enumerate(lines):
        if rx.search(line):
            if len(hits) >= max_results:
                truncated = True
                break
            hits.append(i)
    context = max(0, min(context, 20))
    wanted = set()
    for i in hits:
        wanted.update(range(max(0, i - context), min(len(lines), i + context + 1)))
    matched = set(hits)
    return [(i + 1, lines[i], i in matched) for i in sorted(wanted)], truncated


def _check_mode(mode: str, pattern: Optional[str]) -> None:
    if mode not in _MODES:
        raise ValueError(f"mode must be one of {', '.join(_MODES)}")
    if mode == "grep":
        if not pattern:
            raise ValueError('mode "grep" needs a pattern')
        _compile(pattern)


def _shape(text: str, mode: str, pattern: Optional[str]) -> str:
    if mode == "full":
        return _cap(text)[0]
    lines = text.splitlines()
    if mode == "head":
        return "\n".join(lines[:_SHAPE_LINES])[:_max_output]
    if mode == "tail":
        out = "\n".join(lines[-_SHAPE_LINES:])
        return out[-_max_output:] if len(out) > _max_output else out
    rows, _ = _grep_lines(lines, _compile(pattern or ""), _GREP_CONTEXT, 200)
    out: List[str] = []
    prev = 0
    for n, line, is_match in rows:
        if prev and n != prev + 1:
            out.append("--")
        out.append(f"{n}{':' if is_match else '-'}{line}")
        prev = n
    return "\n".join(out)[:_max_output]


def _shape_result(
    stdout: str, stderr: str, mode: str, pattern: Optional[str]
) -> Tuple[str, str, bool, Optional[str]]:
    """Shape both streams; if anything was left out, keep the full text and return its id."""
    out, err = _shape(stdout, mode, pattern), _shape(stderr, mode, pattern)
    cut = out != stdout or err != stderr
    return out, err, cut, _store_output(stdout, stderr) if cut else None


class OutputChunk(BaseModel):
    output_id: str
    stream: str
    text: str
    offset: int = Field(description="Where this chunk starts (characters or lines, per mode).")
    next_offset: int = Field(description="Pass as offset to continue reading.")
    total: int = Field(description="Total characters or lines in the stream.")
    more: bool = Field(description="More data remains after this chunk.")


class OutputMatch(BaseModel):
    line_no: int
    text: str
    is_match: bool = Field(description="False for a surrounding context line.")


class OutputGrepResult(BaseModel):
    output_id: str
    stream: str
    matches: List[OutputMatch]
    total_lines: int
    truncated: bool = Field(description="max_results was reached; narrow the pattern.")


def output_read(
    output_id: str, stream: str = "stdout", offset: int = 0, limit: Optional[int] = None, mode: str = "chars"
) -> OutputChunk:
    """Page through the full output of an earlier command whose result carried an `output_id`. stream is "stdout" or "stderr"; mode "chars" (default) or "lines" sets what offset and limit count (a chunk is always capped to the output limit). Continue with next_offset. Example: output_read(output_id="ab12cd34", stream="stderr", offset=0, limit=50, mode="lines")"""
    text = _stored(output_id, stream)
    if offset < 0:
        raise ValueError("offset must not be negative")
    if limit is not None and limit < 1:
        raise ValueError("limit must be positive")
    if mode == "chars":
        want = _max_output if limit is None else min(limit, _max_output)
        chunk = text[offset : offset + want]
        nxt, total = offset + len(chunk), len(text)
    elif mode == "lines":
        lines = text.splitlines(keepends=True)
        want = 200 if limit is None else limit
        picked: List[str] = []
        size = 0
        for line in lines[offset : offset + want]:
            if picked and size + len(line) > _max_output:
                break
            picked.append(line)
            size += len(line)
        chunk = "".join(picked)[:_max_output]
        nxt, total = offset + len(picked), len(lines)
    else:
        raise ValueError('mode must be "chars" or "lines"')
    return OutputChunk(
        output_id=output_id, stream=stream, text=chunk, offset=offset, next_offset=nxt, total=total, more=nxt < total
    )


def output_grep(
    output_id: str, pattern: str, stream: str = "stdout", context: int = 2, max_results: int = 100
) -> OutputGrepResult:
    """Search the full output of an earlier command (by its `output_id`) with a regex; returns matching lines with line numbers and `context` surrounding lines, at most max_results matches. Example: output_grep(output_id="ab12cd34", pattern="ERROR|FAIL", context=3)"""
    text = _stored(output_id, stream)
    lines = text.splitlines()
    rows, truncated = _grep_lines(lines, _compile(pattern), context, max(max_results, 0))
    matches: List[OutputMatch] = []
    size = 0
    for n, line, is_match in rows:
        line = line[:2000]
        size += len(line) + 8
        if size > _max_output:
            truncated = True
            break
        matches.append(OutputMatch(line_no=n, text=line, is_match=is_match))
    return OutputGrepResult(
        output_id=output_id, stream=stream, matches=matches, total_lines=len(lines), truncated=truncated
    )


# ---------- Patches ----------------------------------------------------------

# apply_patch is a self-contained unified-diff applier rather than a wrapper
# around `git apply`/`patch`: it works identically inside and outside a git
# repository, validates every path against the root before anything is touched,
# tolerates line offsets, reports the exact hunk that failed, and is all-or-
# nothing across files. Binary patches are not supported.

_HUNK_RE = re.compile(r"^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@")


@dataclass
class _Hunk:
    old_start: int
    old_len: int
    lines: List[Tuple[str, str, bool]]  # (op ' '/'-'/'+', text, no trailing newline)


@dataclass
class _FilePatch:
    old: Optional[str] = None  # None: /dev/null (creation)
    new: Optional[str] = None  # None: /dev/null (deletion)
    hunks: List[_Hunk] = field(default_factory=list)
    is_new: bool = False
    is_delete: bool = False
    rename: bool = False
    mode: Optional[str] = None  # mode of a created file


def _patch_path(raw: str) -> Optional[str]:
    """A path from a ---/+++ header ('a/x', 'b/x', 'x\\ttimestamp') without its git prefix."""
    raw = raw.split("\t")[0].strip()
    if raw.startswith('"') and raw.endswith('"') and len(raw) > 1:
        raw = raw[1:-1]
    if raw == "/dev/null":
        return None
    if raw.startswith(("a/", "b/")):
        raw = raw[2:]
    return raw


def _parse_patch(patch: str) -> List[_FilePatch]:
    lines = patch.replace("\r\n", "\n").split("\n")
    if lines and lines[-1] == "":
        lines.pop()
    files: List[_FilePatch] = []
    cur: Optional[_FilePatch] = None
    i = 0
    while i < len(lines):
        line = lines[i]
        if line.startswith("diff --git "):
            cur = _FilePatch()
            files.append(cur)
            m = re.match(r"diff --git (?:a/)?(\S+) (?:b/)?(\S+)$", line)
            if m:
                cur.old, cur.new = m.group(1), m.group(2)
            i += 1
        elif cur is not None and line.startswith("new file mode"):
            cur.is_new, cur.mode = True, line.split()[-1]
            i += 1
        elif cur is not None and line.startswith("deleted file mode"):
            cur.is_delete = True
            i += 1
        elif cur is not None and line.startswith("rename from "):
            cur.rename, cur.old = True, line[len("rename from "):].strip()
            i += 1
        elif cur is not None and line.startswith("rename to "):
            cur.rename, cur.new = True, line[len("rename to "):].strip()
            i += 1
        elif line.startswith(("GIT binary patch", "Binary files ")):
            raise ValueError("binary patches are not supported")
        elif line.startswith("--- ") and i + 1 < len(lines) and lines[i + 1].startswith("+++ "):
            old, new = _patch_path(line[4:]), _patch_path(lines[i + 1][4:])
            if cur is None or cur.hunks:
                cur = _FilePatch()
                files.append(cur)
            cur.old, cur.new = old, new
            cur.is_new = cur.is_new or old is None
            cur.is_delete = cur.is_delete or new is None
            i += 2
        elif line.startswith("@@"):
            m = _HUNK_RE.match(line)
            if not m or cur is None:
                raise ValueError(f"malformed hunk header: {line!r}")
            old_len = 1 if m.group(2) is None else int(m.group(2))
            new_len = 1 if m.group(4) is None else int(m.group(4))
            hunk = _Hunk(int(m.group(1)), old_len, [])
            i += 1
            seen_old = seen_new = 0
            while (seen_old < old_len or seen_new < new_len) and i < len(lines):
                body = lines[i]
                op, text = (body[0], body[1:]) if body else (" ", "")  # blank line = empty context
                if op == "\\":
                    if hunk.lines:
                        o, t, _ = hunk.lines[-1]
                        hunk.lines[-1] = (o, t, True)
                    i += 1
                    continue
                if op not in " -+":
                    break
                hunk.lines.append((op, text, False))
                seen_old += op != "+"
                seen_new += op != "-"
                i += 1
            if seen_old != old_len or seen_new != new_len:
                raise ValueError(
                    f"hunk {line!r} is shorter than its header says (has {seen_old} old / {seen_new} new lines); "
                    "regenerate the diff with correct line counts"
                )
            while i < len(lines) and lines[i].startswith("\\"):  # trailing "No newline" marker
                if hunk.lines:
                    o, t, _ = hunk.lines[-1]
                    hunk.lines[-1] = (o, t, True)
                i += 1
            cur.hunks.append(hunk)
        else:
            i += 1  # commentary, index lines, mode lines...
    files = [f for f in files if f.hunks or f.is_new or f.is_delete or f.rename]
    if not files:
        raise ValueError("no file changes found: expected a unified diff (---/+++ headers and @@ hunks)")
    return files


def _patch_target(name: Optional[str]) -> Path:
    """Validate a path named in a patch: relative, no '..', and inside the root
    after symlinks (patches do not write to scratch or temp)."""
    if not name:
        raise ValueError("patch names an empty path")
    parts = Path(name).parts
    if Path(name).is_absolute() or ".." in parts:
        raise PermissionError(f"{name!r} is outside the allowed root {_root} (absolute and '..' paths are refused)")
    resolved = _resolve(name)
    if resolved != _root and _root not in resolved.parents:
        raise PermissionError(f"{name!r} resolves to {resolved}, outside the root {_root}")
    return resolved


def _find_hunk(orig: List[str], seq: List[str], expected: int, floor: int) -> Optional[int]:
    """Index in ``orig`` where ``seq`` matches, nearest to ``expected`` and not before ``floor``."""
    n = len(seq)

    def matches(pos: int, strict: bool) -> bool:
        if pos < floor or pos + n > len(orig):
            return False
        window = orig[pos : pos + n]
        if strict:
            return window == seq
        return [w.strip() for w in window] == [q.strip() for q in seq]

    for strict in (True, False):
        for delta in range(0, len(orig) + 1):
            for pos in ((expected,) if delta == 0 else (expected - delta, expected + delta)):
                if matches(pos, strict):
                    return pos
    return None


def _apply_hunks(name: str, text: str, hunks: List[_Hunk]) -> Tuple[str, int, int]:
    """Apply hunks to ``text``; returns (new text, lines added, lines removed)."""
    eol = "\r\n" if "\r\n" in text and text.count("\r\n") == text.count("\n") else "\n"
    orig_keep = text.splitlines(keepends=True)
    orig = [l.rstrip("\r\n") for l in orig_keep]
    out: List[str] = []
    cursor = added = removed = 0
    for n, h in enumerate(hunks, 1):
        seq = [t for op, t, _ in h.lines if op != "+"]
        expected = h.old_start if h.old_len == 0 else h.old_start - 1
        pos = _find_hunk(orig, seq, max(expected, 0), cursor)
        if pos is None:
            near = orig[max(expected, 0) : max(expected, 0) + len(seq)]
            raise ValueError(
                f"{name}: hunk {n} (@@ -{h.old_start},{h.old_len} @@) does not apply.\n"
                "Expected these lines (context and removed lines):\n"
                + "\n".join(f"  | {t}" for t in seq[:12])
                + ("\n  | ..." if len(seq) > 12 else "")
                + f"\nFile has near line {h.old_start}:\n"
                + ("\n".join(f"  | {t}" for t in near[:12]) if near else "  (past the end of the file)")
            )
        out.extend(orig_keep[cursor:pos])
        k = pos
        for op, t, no_eol in h.lines:
            if op == " ":
                out.append(orig_keep[k])
                k += 1
            elif op == "-":
                k += 1
                removed += 1
            else:
                out.append(t + ("" if no_eol else eol))
                added += 1
        cursor = k
    out.extend(orig_keep[cursor:])
    return "".join(out), added, removed


class PatchFileResult(BaseModel):
    path: str
    action: str = Field(description="modify, create, delete or rename.")
    hunks: int
    added: int
    removed: int
    renamed_from: Optional[str] = None


class PatchResult(BaseModel):
    applied: bool = Field(description="False for a dry run: nothing was written.")
    dry_run: bool
    files: List[PatchFileResult]


def apply_patch(patch: str, dry_run: bool = False) -> PatchResult:
    """Apply a unified diff (git-style or plain ---/+++/@@, one or many files, including new files and deletions) inside the root. All-or-nothing: if any hunk fails to apply, nothing is written and the error shows the failing hunk. Paths must stay inside the root. Line numbers may be a little off; context lines must match. dry_run=true only validates and returns the per-file summary. Example: apply_patch(patch="--- a/f.txt\\n+++ b/f.txt\\n@@ -1,2 +1,2 @@\\n one\\n-two\\n+2\\n")"""
    if not patch.strip():
        raise ValueError("patch is empty")
    if not patch.endswith("\n"):
        patch += "\n"
    file_patches = _parse_patch(patch)
    state: Dict[Path, Optional[str]] = {}  # path -> pending content (None = deleted)
    modes: Dict[Path, str] = {}
    results: List[PatchFileResult] = []

    def load(p: Path, name: str) -> str:
        if p in state:
            if state[p] is None:
                raise ValueError(f"{name}: file was deleted earlier in this patch")
            return state[p] or ""
        if not p.is_file():
            raise FileNotFoundError(f"{name}: file does not exist")
        return p.read_bytes().decode()

    for fp in file_patches:
        added = removed = 0
        renamed_from = None
        if fp.is_new and not fp.rename:
            name = fp.new or fp.old or ""
            p = _patch_target(name)
            if p in state and state[p] is not None or (p not in state and os.path.lexists(p)):
                raise ValueError(f"{name}: file already exists, cannot create it")
            text, added, removed = _apply_hunks(name, "", fp.hunks)
            state[p] = text
            if fp.mode and fp.mode.endswith("755"):
                modes[p] = fp.mode
            action = "create"
        elif fp.is_delete and not fp.rename:
            name = fp.old or fp.new or ""
            p = _patch_target(name)
            text, added, removed = _apply_hunks(name, load(p, name), fp.hunks)
            if text.strip():
                raise ValueError(f"{name}: deletion patch does not remove all of the file's content")
            state[p] = None
            action = "delete"
        else:
            old_name = fp.old or fp.new or ""
            new_name = fp.new or fp.old or ""
            src, dst = _patch_target(old_name), _patch_target(new_name)
            text = load(src, old_name)
            if fp.hunks:
                text, added, removed = _apply_hunks(new_name, text, fp.hunks)
            if src != dst:
                if os.path.lexists(dst) and state.get(dst, "") is not None:
                    raise ValueError(f"{new_name}: rename target already exists")
                state[src] = None
                renamed_from = old_name
                if src.exists() and os.access(src, os.X_OK):
                    modes[dst] = "100755"
            state[dst] = text
            action = "rename" if src != dst else "modify"
            name = new_name
        results.append(
            PatchFileResult(
                path=name, action=action, hunks=len(fp.hunks), added=added, removed=removed, renamed_from=renamed_from
            )
        )
    if not dry_run:
        for p, content in state.items():
            if content is None:
                if p.is_symlink() or p.is_file():
                    p.unlink()
        for p, content in state.items():
            if content is not None:
                _atomic_write(p, content.encode())
                if p in modes:
                    p.chmod(p.stat().st_mode | 0o111)
    return PatchResult(applied=not dry_run, dry_run=dry_run, files=results)


# ---------- Symbol navigation ------------------------------------------------

# Definition patterns per language for the ripgrep fallback: (kind, regex with
# {n} for the escaped name). Tried in order; the first that matches a line names
# its kind. Written in the syntax both Rust's regex (ripgrep) and Python's `re` accept.
_QUAL = r"(?:(?:public|private|protected|static|async|readonly|override|final|abstract|synchronized|native|default)\s+)"
_SYMBOL_RULES: List[Tuple[List[str], List[Tuple[str, str]]]] = [
    (["*.py"], [
        ("class", r"^\s*class\s+{n}\b"),
        ("function", r"^\s*(?:async\s+)?def\s+{n}\b"),
        ("variable", r"^{n}\s*(?::[^=]+)?=(?:[^=]|$)"),
    ]),
    (["*.go"], [
        ("function", r"^\s*func\s+{n}\b"),
        ("method", r"^\s*func\s+\([^)]*\)\s*{n}\b"),
        ("type", r"^\s*type\s+{n}\b"),
        ("constant", r"^\s*const\s+{n}\b"),
        ("variable", r"^\s*var\s+{n}\b"),
    ]),
    (["*.ts", "*.tsx", "*.js", "*.jsx", "*.mjs", "*.cjs"], [
        ("function", r"\bfunction\s*\*?\s*{n}\b"),
        ("class", r"\bclass\s+{n}\b"),
        ("interface", r"\binterface\s+{n}\b"),
        ("type", r"\btype\s+{n}\b\s*(?:<[^>]*>)?\s*="),
        ("enum", r"\benum\s+{n}\b"),
        ("variable", r"\b(?:const|let|var)\s+{n}\b"),
        ("method", r"^\s*" + _QUAL + r"*{n}\s*(?:<[^>]*>)?\([^)]*\)\s*(?::\s*[^;{=]+)?\s*\{"),
    ]),
    (["*.rs"], [
        ("function", r"\bfn\s+{n}\b"),
        ("struct", r"\bstruct\s+{n}\b"),
        ("enum", r"\benum\s+{n}\b"),
        ("trait", r"\btrait\s+{n}\b"),
        ("type", r"\btype\s+{n}\b"),
        ("constant", r"\b(?:const|static)\s+{n}\b"),
        ("module", r"\bmod\s+{n}\b"),
        ("macro", r"\bmacro_rules!\s*{n}\b"),
    ]),
    (["*.java"], [
        ("class", r"\b(?:class|record)\s+{n}\b"),
        ("interface", r"\binterface\s+{n}\b"),
        ("enum", r"\benum\s+{n}\b"),
        ("method", r"^\s*" + _QUAL + r"+[\w<>\[\],.? ]+\s+{n}\s*\("),
    ]),
    (["*.c", "*.h", "*.cc", "*.cpp", "*.cxx", "*.hpp", "*.hh"], [
        ("macro", r"^\s*#\s*define\s+{n}\b"),
        ("struct", r"\b(?:struct|union)\s+{n}\b\s*(?:\{|$|:)"),
        ("class", r"\bclass\s+{n}\b\s*(?:\{|$|:|final)"),
        ("enum", r"\benum(?:\s+class)?\s+{n}\b"),
        ("type", r"\btypedef\b.*\b{n}\s*;"),
        ("function", r"^[A-Za-z_][\w\s\*&:<>,~]*?[\s\*&:]{n}\s*\([^;]*$"),
    ]),
]

_KIND_ALIASES = {
    "func": "function", "fn": "function", "def": "function", "member": "method", "var": "variable",
    "const": "constant", "typedef": "type", "cls": "class",
}


def _norm_kind(kind: Optional[str]) -> str:
    k = (kind or "").lower()
    return _KIND_ALIASES.get(k, k)


class SymbolHit(BaseModel):
    file: str
    line: int
    text: str = Field(description="The source line.")
    kind: Optional[str] = Field(default=None, description="function, class, method, type, variable, ... (definitions only).")


class SymbolResult(BaseModel):
    method: str = Field(description='How it was found: "ctags" or "heuristic" (ripgrep patterns; may miss or over-match).')
    results: List[SymbolHit]
    truncated: bool = Field(description="More results existed than max_results.")


def _check_name(name: str) -> str:
    if not re.fullmatch(r"[\w$]+", name or ""):
        raise ValueError(f"name must be a plain identifier, got {name!r}")
    return name


async def _ctags_ok() -> bool:
    if not shutil.which("ctags"):
        return False
    try:
        r = await _run(["ctags", "--version"], timeout=10)
    except RuntimeError:
        return False
    return "Universal Ctags" in r["stdout"]


def _source_line(cache: Dict[str, List[str]], rel: str, line: int) -> str:
    if rel not in cache:
        try:
            cache[rel] = (_root / rel).read_text(errors="replace").splitlines()
        except OSError:
            cache[rel] = []
    lines = cache[rel]
    return lines[line - 1].rstrip() if 0 < line <= len(lines) else ""


async def _definitions(name: str, path: str, kind: Optional[str], limit: int) -> Tuple[str, List[SymbolHit], bool]:
    """Definitions of ``name`` under ``path``: (method, hits, truncated)."""
    target = _resolve(path)
    want = _norm_kind(kind) if kind else None
    hits: List[SymbolHit] = []
    if await _ctags_ok():
        argv = ["ctags", "--output-format=json", "--fields=+n", "-f", "-", "-R"]
        argv += [f"--exclude={d}" for d in sorted(_SKIP_DIRS)]
        argv.append(str(target))
        r = await _run(argv, timeout=120)
        if r["returncode"] != 0:
            raise RuntimeError(r["stderr"].strip() or "ctags failed")
        cache: Dict[str, List[str]] = {}
        for raw in r["stdout"].splitlines():
            try:
                tag = json.loads(raw)
            except ValueError:
                continue
            if tag.get("_type") != "tag" or tag.get("name") != name:
                continue
            if want and _norm_kind(tag.get("kind")) != want:
                continue
            file = _rel(_resolve(str(tag.get("path", ""))) if tag.get("path") else target)
            line = int(tag.get("line") or 0)
            hits.append(SymbolHit(file=file, line=line, text=_source_line(cache, file, line), kind=tag.get("kind")))
        method = "ctags"
    else:
        esc = re.escape(name)

        async def one(globs: List[str], rules: List[Tuple[str, str]]) -> List[SymbolHit]:
            templates = [t.replace("{n}", esc) for _, t in rules]
            pattern = "|".join(f"(?:{t})" for t in templates)
            found: List[SymbolHit] = []
            for glob in globs:
                res = await ripgrep(pattern, path=str(target), glob=glob, max_results=1000)
                for m in res.matches:
                    for (k, _), t in zip(rules, templates):
                        if re.search(t, m.text):
                            found.append(SymbolHit(file=m.file, line=m.line_no, text=m.text.rstrip(), kind=k))
                            break
            return found

        groups = await asyncio.gather(*(one(globs, rules) for globs, rules in _SYMBOL_RULES))
        hits = [h for g in groups for h in g if not want or _norm_kind(h.kind) == want]
        method = "heuristic"
    seen = set()
    unique = []
    for h in sorted(hits, key=lambda h: (h.file, h.line)):
        if (h.file, h.line) not in seen:
            seen.add((h.file, h.line))
            unique.append(h)
    return method, unique[: max(limit, 0)], len(unique) > limit


async def find_symbol(
    name: str, path: str = ".", kind: Optional[str] = None, max_results: int = 100
) -> SymbolResult:
    """Find where a function, class, method, type or variable named `name` is DEFINED under path (a directory or file). Uses universal-ctags when installed, otherwise ripgrep patterns for Python, Go, TypeScript/JavaScript, Rust, Java and C/C++ (`method` in the result says which). kind optionally narrows to function, class, method, type, variable, constant, interface, enum, struct, module or macro. Example: find_symbol(name="handle_request", path="src", kind="function")"""
    _check_name(name)
    method, hits, cut = await _definitions(name, path, kind, max_results)
    return SymbolResult(method=method, results=hits, truncated=cut)


async def find_references(name: str, path: str = ".", max_results: int = 100) -> SymbolResult:
    """Find every whole-word occurrence of `name` under path (a directory or file), with file, line and the line's text, leaving out the lines where it is defined. `method` says how the definitions were found ("ctags" or "heuristic"). Example: find_references(name="handle_request", path="src")"""
    _check_name(name)
    method, defs, _ = await _definitions(name, path, None, 100_000)
    defined = {(d.file, d.line) for d in defs}
    res = await ripgrep(rf"\b{re.escape(name)}\b", path=path, max_results=max_results + len(defined) + 1)
    hits = [
        SymbolHit(file=m.file, line=m.line_no, text=m.text.rstrip())
        for m in res.matches
        if (m.file, m.line_no) not in defined
    ]
    return SymbolResult(method=method, results=hits[:max_results], truncated=res.truncated or len(hits) > max_results)


# ---------- Tests -------------------------------------------------------------


async def _run_scoped(groups: List[Tuple[str, Path, List[str]]], timeout: float) -> TestRunSummary:
    """Executes the groups _changed_targets produced and merges their parsed
    results into one summary. framework is "changed" so the caller can see the
    run was scoped; command lists what actually ran."""
    passed = failed = skipped = 0
    exit_code, ok = 0, True
    failures: List[FailedTest] = []
    tails: List[str] = []
    cmds: List[str] = []
    for framework, cwd, argv in groups:
        cmds.append(f"(cd {cwd.relative_to(_root).as_posix() or '.'} && {shlex.join(argv)})")
        r = await _run(argv, cwd=cwd, timeout=timeout)
        text = r["stdout"] + (("\n" + r["stderr"]) if r["stderr"] else "")
        if framework == "pytest":
            p, f, sk, fails = _parse_pytest(text)
            tail = text
        else:
            p, f, sk, fails, readable = _parse_go_json(r["stdout"])
            tail = readable.strip() or text
        passed, failed, skipped = passed + p, failed + f, skipped + sk
        failures.extend(fails)
        exit_code = exit_code or r["returncode"]
        ok = ok and r["returncode"] == 0 and f == 0
        tails.append(f"=== {cwd.relative_to(_root).as_posix() or '.'} ({framework}) ===\n{tail}")
    combined = "\n\n".join(tails)
    tail_src = _tail(combined) if len(combined.splitlines()) > _TAIL_LINES else combined
    output_id = _store_output(combined) if len(combined) > _max_output else None
    return TestRunSummary(
        framework="changed", command=" && ".join(cmds), exit_code=exit_code, ok=ok,
        passed=passed, failed=failed, skipped=skipped, failures=failures,
        output_tail=tail_src, output_id=output_id,
    )


class FailedTest(BaseModel):
    name: str
    file: Optional[str] = None
    line: Optional[int] = None
    message: str = ""


class TestRunSummary(BaseModel):
    __test__ = False  # not a pytest class
    framework: str = Field(description="pytest, go, npm, cargo or custom.")
    command: str
    exit_code: int
    ok: bool = Field(description="Exit code 0 and no failures.")
    passed: int
    failed: int
    skipped: int
    failures: List[FailedTest]
    output_tail: str = Field(description="The end of the output, for anything the parsing missed.")
    output_id: Optional[str] = Field(default=None, description="Full output, for output_read / output_grep.")
    timed_out: bool = Field(
        default=False,
        description="True when the run was killed at the timeout; exit_code is then the sentinel 124 and counts reflect the partial output.",
    )
    elapsed_s: Optional[float] = Field(default=None, description="Seconds from start to the kill (or exit).")
    applied_timeout_s: Optional[float] = Field(default=None, description="The timeout actually applied.")


_TAIL_LINES = 40


def _tail(text: str, lines: int = _TAIL_LINES) -> str:
    out = "\n".join(text.splitlines()[-lines:])
    return out[-min(_max_output, 8000):]


def _pytest_counts(text: str) -> Tuple[int, int, int]:
    """(passed, failed+errors, skipped) from pytest's final summary line."""
    for line in reversed(text.splitlines()):
        if re.search(r"\bin [\d.]+s\b", line) and re.search(r"\d+ (?:passed|failed|skipped|errors?|xfailed|xpassed|deselected)", line):
            counts: Dict[str, int] = {}
            for n, word in re.findall(r"(\d+) ([a-z]+)", line.split(" in ")[0]):
                counts[word] = counts.get(word, 0) + int(n)
            return (
                counts.get("passed", 0),
                counts.get("failed", 0) + counts.get("error", 0) + counts.get("errors", 0),
                counts.get("skipped", 0),
            )
    return 0, 0, 0


def _parse_pytest(text: str) -> Tuple[int, int, int, List[FailedTest]]:
    passed, failed, skipped = _pytest_counts(text)
    lines = text.splitlines()
    # file:line: message entries (--tb=line) and per-test blocks, from the FAILURES / ERRORS sections
    entries: List[Tuple[str, int, str]] = []
    blocks: List[Tuple[str, List[str]]] = []
    in_section = False
    for line in lines:
        if re.match(r"^=+ (FAILURES|ERRORS) =+$", line):
            in_section = True
            continue
        if in_section and re.match(r"^=+ .* =+$", line):
            in_section = False
        if not in_section:
            continue
        m = re.match(r"^_{3,} (.+?) _{3,}$", line)
        if m:
            blocks.append((m.group(1), []))
        elif blocks:
            blocks[-1][1].append(line)
        m = re.match(r"^(\S+\.py):(\d+): (.*)$", line)
        if m:
            entries.append((m.group(1), int(m.group(2)), m.group(3)))
    failures: List[FailedTest] = []
    used = set()

    def take(file: Optional[str]) -> Optional[Tuple[str, int, str]]:
        for k, (path, ln, msg) in enumerate(entries):
            if k not in used and (file is None or path.endswith(file) or file.endswith(path)):
                used.add(k)
                return path, ln, msg
        return None

    for line in lines:
        m = re.match(r"^(FAILED|ERROR) (.+?)(?: - (.*))?$", line)
        if not m:
            continue
        node = m.group(2)
        file = node.split("::")[0] if "::" in node else None
        e = take(file)
        failures.append(
            FailedTest(
                name=node,
                file=file or (e[0] if e else None),
                line=e[1] if e else None,
                message=(m.group(3) or (e[2] if e else "")).strip(),
            )
        )
    if not failures:
        for name, body in blocks:
            loc = [re.match(r"^(\S+\.py):(\d+): ", b) for b in body]
            loc = [x for x in loc if x]
            err = next((b[1:].strip() for b in body if re.match(r"^E\s", b)), "")
            failures.append(
                FailedTest(
                    name=name,
                    file=loc[-1].group(1) if loc else None,
                    line=int(loc[-1].group(2)) if loc else None,
                    message=err,
                )
            )
    if not failures:
        failures = [FailedTest(name=f"{p}:{ln}", file=p, line=ln, message=msg) for p, ln, msg in entries]
    return passed, max(failed, len(failures)), skipped, failures


def _parse_go_json(text: str) -> Tuple[int, int, int, List[FailedTest], str]:
    tests: Dict[Tuple[str, str], Dict[str, Any]] = {}
    pkgs: Dict[str, Dict[str, Any]] = {}
    plain: List[str] = []
    for raw in text.splitlines():
        try:
            ev = json.loads(raw)
        except ValueError:
            plain.append(raw)
            continue
        if not isinstance(ev, dict):
            continue
        pkg, test, action = ev.get("Package", ""), ev.get("Test"), ev.get("Action")
        rec = tests.setdefault((pkg, test), {"out": [], "res": None}) if test else pkgs.setdefault(pkg, {"out": [], "res": None})
        if action == "output":
            rec["out"].append(ev.get("Output", ""))
        elif action in ("pass", "fail", "skip"):
            rec["res"] = action
    passed = sum(1 for t in tests.values() if t["res"] == "pass")
    skipped = sum(1 for t in tests.values() if t["res"] == "skip")
    failed_names = [k for k, t in tests.items() if t["res"] == "fail"]
    failures: List[FailedTest] = []
    for pkg, test in failed_names:
        if any(p == pkg and t.startswith(test + "/") for p, t in failed_names):
            continue  # a parent test only fails because a subtest did
        body = "".join(tests[(pkg, test)]["out"])
        m = re.search(r"^\s*(\S+\.go):(\d+):(?:\d+:)? (.*)$", body, re.M)
        if m:
            file, line, msg = m.group(1), int(m.group(2)), m.group(3).strip()
        else:
            useful = [l.strip() for l in body.splitlines() if l.strip() and not l.lstrip().startswith(("=== ", "--- "))]
            file, line, msg = None, None, " | ".join(useful[-3:])
        failures.append(FailedTest(name=f"{pkg}.{test}", file=file, line=line, message=msg))
    failed_pkgs = {p for p, _ in failed_names}
    for pkg, rec in pkgs.items():
        if rec["res"] == "fail" and pkg not in failed_pkgs:  # build failure, panic, timeout
            body = [l.rstrip() for l in "".join(rec["out"]).splitlines() if l.strip() and not l.startswith("FAIL")]
            m = re.search(r"(\S+\.go):(\d+):(?:\d+:)? (.*)", "\n".join(body))
            failures.append(
                FailedTest(
                    name=pkg,
                    file=m.group(1) if m else None,
                    line=int(m.group(2)) if m else None,
                    message=m.group(3).strip() if m else " | ".join(body[-3:]),
                )
            )
    out_lines: List[str] = []  # human-readable output in event order, for output_tail
    for raw in text.splitlines():
        try:
            ev = json.loads(raw)
        except ValueError:
            out_lines.append(raw)
            continue
        if isinstance(ev, dict) and ev.get("Action") == "output":
            out_lines.append(ev.get("Output", "").rstrip("\n"))
    return passed, len(failures), skipped, failures, "\n".join(out_lines)


def _parse_cargo(text: str) -> Tuple[int, int, int, List[FailedTest]]:
    passed = failed = skipped = 0
    for m in re.finditer(r"test result: \w+\. (\d+) passed; (\d+) failed; (\d+) ignored", text):
        passed += int(m.group(1))
        failed += int(m.group(2))
        skipped += int(m.group(3))
    failures: List[FailedTest] = []
    for name in re.findall(r"^test (\S+) \.\.\. FAILED$", text, re.M):
        sec = re.search(rf"^---- {re.escape(name)} stdout ----\n(.*?)(?=^---- |\n\nfailures:|\Z)", text, re.M | re.S)
        body = sec.group(1) if sec else ""
        m = re.search(r"panicked at (?:(\S+?):(\d+):\d+:?)?\s*\n?(.*)", body)
        failures.append(
            FailedTest(
                name=name,
                file=m.group(1) if m else None,
                line=int(m.group(2)) if m and m.group(2) else None,
                message=(m.group(3).strip() if m else body.strip().splitlines()[-1] if body.strip() else ""),
            )
        )
    return passed, max(failed, len(failures)), skipped, failures


def _parse_generic(text: str) -> Tuple[int, int, int]:
    """Counts from jest / mocha / node --test style summaries; zeros if none recognised."""
    m = re.search(r"^Tests:\s+(.*?)\d+ total", text, re.M)
    if m:
        def n(word: str) -> int:
            f = re.search(rf"(\d+) {word}", m.group(0))
            return int(f.group(1)) if f else 0
        return n("passed"), n("failed"), n("skipped")
    def grab(pat: str) -> int:
        f = re.search(pat, text, re.M)
        return int(f.group(1)) if f else 0
    return (
        grab(r"^\s*(\d+) passing") or grab(r"^# pass (\d+)"),
        grab(r"^\s*(\d+) failing") or grab(r"^# fail (\d+)"),
        grab(r"^\s*(\d+) pending") or grab(r"^# skipped (\d+)"),
    )


def _detect_tests(base: Path) -> Tuple[str, str]:
    """(framework, command) for the project in ``base``."""
    def has(name: str) -> bool:
        return (base / name).exists()

    if has("pytest.ini") or has("pyproject.toml") or has("tox.ini") or (base / "tests").is_dir():
        py = "pytest" if shutil.which("pytest") else f"{shlex.quote(sys.executable)} -m pytest"
        return "pytest", f"{py} -q -rfE --tb=line"
    if has("go.mod"):
        return "go", "go test -json ./..."
    if has("package.json"):
        try:
            scripts = json.loads((base / "package.json").read_text()).get("scripts", {})
        except (OSError, ValueError):
            scripts = {}
        if isinstance(scripts, dict) and "test" in scripts:
            return "npm", "npm test"
    if has("Cargo.toml"):
        return "cargo", "cargo test"
    raise ValueError(
        "no test framework detected (looked for pytest config or tests/, go.mod, package.json with a test script, "
        'Cargo.toml); pass command, e.g. run_tests(command="make test")'
    )


async def _dirty_files(base: Path) -> List[str]:
    """Root-relative paths git reports as modified, staged or untracked under
    base ("" status lines skipped). [] when base is not a git work tree."""
    r = await _run(["git", "status", "--porcelain", "-z", "--", "."], cwd=base)
    if r["returncode"] != 0:
        return []
    out: List[str] = []
    entries = [e for e in r["stdout"].split("\0") if e]
    i = 0
    while i < len(entries):
        entry = entries[i]
        status, raw = entry[:2], entry[3:]
        if status[0] == "r" or (len(status) > 1 and status[1] == "r"):
            i += 1  # rename/copy: the next entry is the old path, skip it
        i += 1
        if "D" in status:
            continue
        prefix = base.relative_to(_root).as_posix() if base != _root else ""
        rel = f"{prefix}/{raw}" if prefix and not raw.startswith(prefix) else raw
        out.append(rel.removeprefix("./"))
    return out


def _changed_targets(base: Path, dirty: List[str]) -> List[Tuple[str, Path, List[str]]]:
    """Map dirty paths to scoped test runs: (framework, cwd, argv). Python:
    changed test files run directly, changed sources run their project's
    tests dir; Go: the packages of changed .go files. ts/js/other languages
    are deliberately not guessed (fall back to full auto-detect)."""
    py_projects: Dict[Path, set] = {}
    go_pkgs: Dict[Path, set] = {}
    markers = ("pyproject.toml", "setup.cfg", "pytest.ini", "tox.ini", "setup.py")
    for rel in dirty:
        f = (_root / rel)
        if not f.exists():
            continue
        if f.suffix == ".py":
            proj = f.parent
            while proj != _root.parent and not any((proj / m).exists() for m in markers):
                proj = proj.parent
            if proj == _root.parent:
                continue
            tests = proj / "tests"
            target = f if f.name.startswith("test_") and tests.exists() and f.is_relative_to(tests) else (tests if tests.is_dir() else proj)
            py_projects.setdefault(proj, set()).add(target)
        elif f.suffix == ".go":
            mod = f.parent
            while mod != _root.parent and not (mod / "go.mod").exists():
                mod = mod.parent
            if mod == _root.parent or f.name == "go.mod":
                continue
            pkg = "." if f.parent == mod else "./" + f.parent.relative_to(mod).as_posix()
            go_pkgs.setdefault(mod, set()).add(pkg)
    out: List[Tuple[str, Path, List[str]]] = []
    for proj in sorted(py_projects, key=lambda p: str(p)):
        targets = sorted(py_projects[proj], key=str)
        out.append(("pytest", proj, ["python", "-m", "pytest", "-q", "-rfE", "--tb=line", *(str(t) for t in targets)]))
    for mod in sorted(go_pkgs, key=lambda p: str(p)):
        pkgs = sorted(go_pkgs[mod])
        out.append(("go", mod, ["go", "test", "-json", *pkgs]))
    return out


async def run_tests(
    command: Optional[str] = None, path: str = ".", timeout: Optional[float] = None,
    changed_only: bool = False,
) -> TestRunSummary:
    """Run the project's tests and return structured results: framework, passed/failed/skipped counts, ok, failures [{name, file, line, message}] and output_tail. With no command it auto-detects pytest (pytest config or tests/), `go test -json ./...` (go.mod), `npm test` (package.json test script) or `cargo test`. Pass command to run something else (pytest and go test output is still parsed). changed_only=true runs ONLY the tests git says could be affected (git-changed files → their package/test dir; python+go; falls back to full auto-detect when nothing maps) — far faster on a big monorepo. path is the project directory; timeout defaults to 600 seconds; a timed-out run
    keeps its partial output with timed_out=true and exit_code=124. Full output is
    available via `output_id`. Example: run_tests(path="hub") or run_tests(command="pytest -q tests/test_a.py")"""
    base = _resolve(path)
    if not base.is_dir():
        raise NotADirectoryError(path)
    timeout = 600 if timeout is None else timeout
    if command is None and changed_only:
        groups = _changed_targets(base, await _dirty_files(base))
        if groups:
            return await _run_scoped(groups, timeout)
    if command is None:
        framework, command = _detect_tests(base)
    else:
        framework = "pytest" if "pytest" in command else "go" if "go test" in command else "custom"
        if framework == "go" and "-json" not in command:
            command = command.replace("go test", "go test -json", 1)
    r = await _run(["bash", "-c", command], cwd=base, timeout=timeout)
    text = r["stdout"] + (("\n" + r["stderr"]) if r["stderr"] else "")
    failures: List[FailedTest] = []
    tail_src = text
    if framework == "pytest":
        passed, failed, skipped, failures = _parse_pytest(text)
    elif framework == "go":
        passed, failed, skipped, failures, readable = _parse_go_json(r["stdout"])
        tail_src = (readable + ("\n" + r["stderr"] if r["stderr"] else "")).strip() or text
    elif framework == "cargo":
        passed, failed, skipped, failures = _parse_cargo(text)
    else:
        passed, failed, skipped = _parse_generic(text)
    ok = r["returncode"] == 0 and failed == 0
    output_id = _store_output(tail_src) if len(tail_src) > _max_output or (not ok and len(tail_src.splitlines()) > _TAIL_LINES) else None
    return TestRunSummary(
        framework=framework,
        command=command,
        exit_code=r["returncode"],
        ok=ok,
        passed=passed,
        failed=failed,
        skipped=skipped,
        failures=failures[:100],
        output_tail=_tail(tail_src),
        output_id=output_id,
        timed_out=bool(r.get("timed_out")),
        elapsed_s=r.get("elapsed_s"),
        applied_timeout_s=r.get("applied_timeout_s"),
    )


# ---------- Structured data files ------------------------------------------------

_DATA_MAX_BYTES = 100_000_000
_STEP_RE = re.compile(r"\[(-?\d+|\*)\]|\.?([^.\[\]]+)")


def _path_steps(query: str) -> List[Any]:
    """'items[0].name' -> ['items', 0, 'name']; '*' or '[*]' -> the wildcard."""
    pos, steps = 0, []
    query = query.strip()
    while pos < len(query):
        m = _STEP_RE.match(query, pos)
        if not m:
            raise ValueError(f"bad query near {query[pos:]!r}; use e.g. items[0].name or items[*].id")
        if m.group(1) is not None:
            steps.append("*" if m.group(1) == "*" else int(m.group(1)))
        else:
            steps.append("*" if m.group(2) == "*" else m.group(2))
        pos = m.end()
    return steps


def _json_path(doc: Any, query: str) -> Any:
    nodes, multi = [doc], False
    for step in _path_steps(query):
        nxt: List[Any] = []
        for node in nodes:
            if step == "*":
                if isinstance(node, list):
                    nxt.extend(node)
                elif isinstance(node, dict):
                    nxt.extend(node.values())
                elif not multi:
                    raise ValueError(f"cannot expand [*] on a {type(node).__name__}")
            elif isinstance(step, int):
                if isinstance(node, list) and -len(node) <= step < len(node):
                    nxt.append(node[step])
                elif not multi:
                    raise ValueError(f"index {step} not available (value is {_describe(node)})")
            elif isinstance(node, dict) and step in node:
                nxt.append(node[step])
            elif not multi:
                keys = ", ".join(list(node)[:20]) if isinstance(node, dict) else _describe(node)
                raise ValueError(f"key {step!r} not found; available: {keys}")
        multi = multi or step == "*"
        nodes = nxt
    return nodes if multi else nodes[0]


def _describe(v: Any) -> str:
    if isinstance(v, dict):
        return f"object with keys {', '.join(list(v)[:20])}"
    if isinstance(v, list):
        return f"list of {len(v)}"
    return type(v).__name__


def _kind(v: Any) -> str:
    return {dict: "object", list: "array", str: "string", bool: "boolean", int: "number", float: "number", type(None): "null"}.get(type(v), type(v).__name__)


def _load_data(p: Path, fmt: str) -> Any:
    if p.stat().st_size > _DATA_MAX_BYTES:
        raise ValueError(f"file is larger than {_DATA_MAX_BYTES // 1_000_000} MB")
    text = p.read_text()
    if fmt == "json":
        try:
            return json.loads(text)
        except ValueError as e:
            raise ValueError(f"invalid JSON: {e}") from None
    rows = []
    for n, line in enumerate(text.splitlines(), 1):
        if line.strip():
            try:
                rows.append(json.loads(line))
            except ValueError as e:
                raise ValueError(f"invalid JSON on line {n}: {e}") from None
    return rows


def _fit(value: Any) -> Tuple[Any, bool]:
    """Shrink a list result until its JSON fits the output cap."""
    cut = False
    while isinstance(value, list) and len(value) > 1 and len(json.dumps(value, default=str)) > _max_output:
        value, cut = value[: len(value) // 2], True
    text = json.dumps(value, default=str)
    if len(text) > _max_output:
        return text[:_max_output], True
    return value, cut


def _scalar(raw: str) -> Any:
    try:
        return json.loads(raw)
    except ValueError:
        return raw


def _query_json(doc: Any, fmt: str, query: Optional[str], flt: Optional[str], limit: int, describe: bool) -> Dict[str, Any]:
    if describe:
        info: Dict[str, Any] = {"type": _kind(doc)}
        if isinstance(doc, dict):
            info["keys"] = {k: _kind(v) if not isinstance(v, (list, dict)) else _describe(v) for k, v in list(doc.items())[:100]}
        elif isinstance(doc, list):
            info["length"] = len(doc)
            keys: Dict[str, str] = {}
            for el in doc[:1000]:
                if isinstance(el, dict):
                    for k, v in el.items():
                        keys.setdefault(k, _kind(v))
            if keys:
                info["element_keys"] = keys
        return {"format": fmt, "describe": info}
    value = _json_path(doc, query) if query else doc
    if flt:
        m = re.match(r"^\s*([^=!]+?)\s*==\s*(.*)$", flt)
        if not m:
            raise ValueError('filter must look like key==value, e.g. status=="ok" or age==3')
        key, want = m.group(1), _scalar(m.group(2).strip())
        if not isinstance(value, list):
            raise ValueError("filter needs the query to produce a list (add [*] or point at an array)")

        def keep(el: Any) -> bool:
            try:
                got = _json_path(el, key)
            except (ValueError, TypeError):
                return False
            return got == want or (isinstance(got, (str, int, float)) and str(got) == str(want))

        value = [el for el in value if keep(el)]
    count = len(value) if isinstance(value, list) else 1
    truncated = False
    if isinstance(value, list) and len(value) > limit:
        value, truncated = value[: max(limit, 0)], True
    value, cut = _fit(value)
    return {"format": fmt, "count": count, "truncated": truncated or cut, "result": value}


def _number(s: str) -> Optional[float]:
    try:
        return float(s)
    except ValueError:
        return None


def _cell_type(values: List[str]) -> str:
    seen = set()
    for v in values:
        if v == "":
            continue
        if re.fullmatch(r"-?\d+", v):
            seen.add("integer")
        elif _number(v) is not None:
            seen.add("number")
        elif v.lower() in ("true", "false"):
            seen.add("boolean")
        else:
            seen.add("string")
    if not seen:
        return "empty"
    if seen == {"integer", "number"}:
        return "number"
    return seen.pop() if len(seen) == 1 else "mixed"


def _where_test(where: Dict[str, Any], header: List[str]) -> Any:
    col, op, val = where.get("column"), where.get("op", "=="), where.get("value")
    if col not in header:
        raise ValueError(f"unknown column {col!r}; columns: {', '.join(header)}")
    if op not in ("==", "!=", "<", ">", "<=", ">=", "contains"):
        raise ValueError("where.op must be one of ==, !=, <, >, <=, >=, contains")
    target = "" if val is None else str(val)
    tnum = _number(target)

    def test(row: Dict[str, str]) -> bool:
        cell = row.get(col) or ""
        if op == "contains":
            return target.lower() in cell.lower()
        cnum = _number(cell)
        a, b = (cnum, tnum) if cnum is not None and tnum is not None else (cell, target)
        return {"==": a == b, "!=": a != b, "<": a < b, ">": a > b, "<=": a <= b, ">=": a >= b}[op]

    return test


def _query_csv(
    p: Path, fmt: str, columns: Optional[List[str]], where: Optional[Dict[str, Any]], limit: int, describe: bool
) -> Dict[str, Any]:
    csv.field_size_limit(1 << 24)
    with open(p, newline="") as f:
        reader = csv.DictReader(f, delimiter="\t" if fmt == "tsv" else ",")
        header = list(reader.fieldnames or [])
        if columns:
            bad = [c for c in columns if c not in header]
            if bad:
                raise ValueError(f"unknown column(s) {bad}; columns: {', '.join(header)}")
        test = _where_test(where, header) if where else None
        rows: List[Dict[str, Any]] = []
        total = matched = 0
        samples: Dict[str, List[str]] = {c: [] for c in header}
        for row in reader:
            total += 1
            if describe:
                if total <= 1000:
                    for c in header:
                        samples[c].append(row.get(c) or "")
                continue
            if test and not test(row):
                continue
            matched += 1
            if len(rows) < limit:
                rows.append({c: row.get(c) for c in (columns or header)})
    if describe:
        return {
            "format": fmt,
            "describe": {
                "rows": total,
                "columns": [{"name": c, "type": _cell_type(samples[c])} for c in header],
                "types_from_first_rows": min(total, 1000),
            },
        }
    fitted, cut = _fit(rows)
    return {"format": fmt, "count": matched, "truncated": matched > len(rows) or cut, "columns": columns or header, "result": fitted}


def data_query(
    path: str,
    query: Optional[str] = None,
    format: Optional[str] = None,
    limit: int = 100,
    filter: Optional[str] = None,
    columns: Optional[List[str]] = None,
    where: Optional[Dict[str, Any]] = None,
    describe: bool = False,
) -> Dict[str, Any]:
    """Read-only query of a JSON, JSONL, CSV or TSV file (format is guessed from the extension). JSON: `query` is a path like `items[0].name` or `items[*].id` (JSONL is treated as a list of records: `[0].name`, `[*].id`), `filter` keeps list items where key==value (e.g. `status==ok`). CSV/TSV: `columns` picks columns, `where` is {"column", "op" (==, !=, <, >, <=, >=, contains), "value"}. `limit` caps returned items/rows; `describe=true` returns the schema (CSV: columns, types, row count). Example: data_query(path="users.csv", where={"column": "age", "op": ">", "value": 30}, columns=["name"], limit=10)"""
    p = _resolve(path)
    if not p.is_file():
        raise FileNotFoundError(str(path))
    if limit < 0:
        raise ValueError("limit must not be negative")
    ext = p.suffix.lower().lstrip(".")
    fmt = (format or {"ndjson": "jsonl"}.get(ext, ext)).lower()
    if fmt not in ("json", "jsonl", "csv", "tsv"):
        raise ValueError('format must be json, jsonl, csv or tsv (could not tell from the file extension)')
    if fmt in ("csv", "tsv"):
        return _query_csv(p, fmt, columns, where, limit, describe)
    return _query_json(_load_data(p, fmt), fmt, query, filter, limit, describe)


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
        (multi_edit, False, True, False, False),
        (apply_patch, False, True, False, False),
        (data_query, True, False, True, False),
        # code navigation
        (find_symbol, True, False, True, False),
        (find_references, True, False, True, False),
        (file_move, False, False, False, False),  # never overwrites
        (file_delete, False, True, True, False),
        # code execution: anything can happen
        (run_command, False, True, False, True),
        (run_python, False, True, False, True),
        # runs whatever the project's tests do (same reach as run_command), hence the same hints
        (run_tests, False, True, False, True),
        (output_read, True, False, True, False),  # pages an in-memory copy of an earlier output
        (output_grep, True, False, True, False),
        (process_start, False, True, False, True),
        (process_read, True, False, False, False),  # consumes buffered output
        (process_kill, False, False, True, False),  # can only address ids this harness started (_managed) — see W9
        # watches run the predicate like process_start does; polling just reads
        (wait_for_start, False, False, False, True),
        (wait_for_poll, True, False, False, False),  # consumes buffered output
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
    ("outside the allowed locations", "occurs 2 times"). ``ToolError`` is delivered
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
        server.tool(annotations=annotations, meta=TOOL_META.get(fn.__name__))(_report_errors(fn))
    return server


def main() -> None:
    import argparse

    parser = argparse.ArgumentParser(description="MCP harness server (stdio)")
    parser.add_argument("--name", default="harness")
    parser.add_argument(
        "--root",
        default=os.environ.get(f"{ENV_PREFIX}ROOT"),
        help=(
            "Directory that file tools are confined to, alongside the scratch and the "
            f"system temp directory (default: the current directory, or {ENV_PREFIX}ROOT). "
            "Use / to lift the restriction."
        ),
    )
    parser.add_argument(
        "--scratch",
        default=os.environ.get(f"{ENV_PREFIX}SCRATCH"),
        help=(
            f"Writable scratch directory outside the usual confinement, e.g. for temp "
            f"build output (default: <root>/{SCRATCH_SUBPATH}, or {ENV_PREFIX}SCRATCH). "
            "Created on first use."
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
        configure(args.root, args.max_output, args.scratch)
    except ValueError as e:
        parser.error(str(e))


    def on_signal(signum: int, _frame: Any) -> None:
        _kill_all_processes()
        os._exit(128 + signum)

    for sig in (signal.SIGTERM, signal.SIGHUP):
        signal.signal(sig, on_signal)
    try:
        build_server(args.name).run("stdio")
    finally:  # stdin EOF / normal shutdown / KeyboardInterrupt
        _kill_all_processes()


if __name__ == "__main__":
    main()
