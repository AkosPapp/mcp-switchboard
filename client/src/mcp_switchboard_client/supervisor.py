"""One local stdio MCP server subprocess: spawn, pipe, supervise.

This module knows nothing about MCP or about the tunnel. It owns a subprocess,
hands every stdout line to a callback, writes lines to stdin on request, and
reports lifecycle transitions (``starting`` / ``running`` / ``exited`` /
``failed``) through a second callback so the tunnel can turn them into
``server_state`` frames.

A server that exits on its own is reported and left down: there is deliberately
no automatic respawn, because a server that fails on startup would otherwise be
restarted in a hot loop. It comes back on an explicit ``restart`` frame or on
the next reconnect.
"""

from __future__ import annotations

import asyncio
import logging
import os
from contextlib import suppress
from typing import Awaitable, Callable, Optional

from . import protocol
from .config import ServerSpec

LOGGER = logging.getLogger("mcp_switchboard_client.supervisor")

# How long a server gets to exit after SIGTERM before it is killed.
TERMINATE_TIMEOUT = 5.0

# asyncio's default StreamReader limit is 64 KiB, and readline() raises once a
# line exceeds it. MCP tool results routinely do, so give stdout real headroom.
STDOUT_LIMIT = 4 * 1024 * 1024

# (server name, line)
StdoutCallback = Callable[[str, str], Awaitable[None]]
# (server name, state, exit code, error)
StateCallback = Callable[[str, str, Optional[int], Optional[str]], Awaitable[None]]


class LocalServer:
    """A single local MCP server process."""

    def __init__(
        self,
        spec: ServerSpec,
        on_stdout: Optional[StdoutCallback] = None,
        on_state: Optional[StateCallback] = None,
    ) -> None:
        self.spec = spec
        self._on_stdout = on_stdout
        self._on_state = on_state
        self.log = logging.getLogger(f"mcp_switchboard_client.server.{spec.name}")

        self._process: Optional[asyncio.subprocess.Process] = None
        self._stdout_task: Optional[asyncio.Task] = None
        self._state: Optional[str] = None

    @property
    def name(self) -> str:
        return self.spec.name

    @property
    def state(self) -> Optional[str]:
        return self._state

    @property
    def running(self) -> bool:
        return self._process is not None and self._process.returncode is None

    @property
    def descriptor(self) -> dict:
        """The entry this server contributes to the ``hello`` frame."""
        entry = {"name": self.name, "command": " ".join(self.spec.argv)}
        if self.spec.project:
            entry["project"] = self.spec.project
        return entry

    async def start(self) -> None:
        """Spawn the process. A no-op if one is already claimed."""
        if self._process is not None:
            return

        await self._emit_state(protocol.STATE_STARTING)

        env = os.environ.copy()
        env.update(self.spec.env)

        self.log.info("starting: %s", " ".join(self.spec.argv))
        try:
            process = await asyncio.create_subprocess_exec(
                *self.spec.argv,
                stdin=asyncio.subprocess.PIPE,
                stdout=asyncio.subprocess.PIPE,
                stderr=None,  # inherit, so the server's own logs reach our stderr
                env=env,
                cwd=self.spec.cwd,
                limit=STDOUT_LIMIT,
            )
        except (OSError, ValueError) as e:
            self.log.error("failed to start: %s", e)
            await self._emit_state(protocol.STATE_FAILED, error=str(e))
            return

        self._process = process
        self._stdout_task = asyncio.create_task(
            self._read_stdout(process), name=f"stdout-{self.name}"
        )
        self.log.info("started (pid %d)", process.pid)
        await self._emit_state(protocol.STATE_RUNNING)

    async def stop(self, *, remove_tempfile: bool = True) -> None:
        """Terminate the process, escalating to SIGKILL after TERMINATE_TIMEOUT.

        ``remove_tempfile`` is false for a restart: a FastMCP-derived spec is
        launched from a generated config file that the respawn still needs.
        """
        # Claim self._process/_stdout_task synchronously - no await before the
        # null-out - so two concurrent stop() calls can't both tear the same
        # process down.
        process = self._process
        self._process = None
        stdout_task = self._stdout_task
        self._stdout_task = None

        if process is None:
            if remove_tempfile:
                self._remove_tempfile()
            return

        if stdout_task is not None:
            stdout_task.cancel()
            with suppress(asyncio.CancelledError):
                await stdout_task

        if process.returncode is None:
            self.log.info("stopping (pid %d)", process.pid)
            with suppress(ProcessLookupError):
                process.terminate()
            try:
                await asyncio.wait_for(process.wait(), timeout=TERMINATE_TIMEOUT)
            except asyncio.TimeoutError:
                self.log.warning("did not exit after %.0fs, killing", TERMINATE_TIMEOUT)
                with suppress(ProcessLookupError):
                    process.kill()
                await process.wait()

        if remove_tempfile:
            self._remove_tempfile()

    async def restart(self) -> None:
        await self.stop(remove_tempfile=False)
        await self.start()

    async def send(self, line: str) -> None:
        """Write one line to the server's stdin."""
        process = self._process
        if process is None or process.stdin is None or process.returncode is not None:
            self.log.warning("not running, dropping message")
            return
        try:
            process.stdin.write(line.rstrip("\n").encode("utf-8") + b"\n")
            await process.stdin.drain()
        except (BrokenPipeError, ConnectionResetError) as e:
            self.log.warning("stdin closed, dropping message: %s", e)

    # -- internals -----------------------------------------------------

    async def _read_stdout(self, process: asyncio.subprocess.Process) -> None:
        assert process.stdout is not None
        try:
            while True:
                line = await process.stdout.readline()
                if not line:
                    break
                text = line.decode("utf-8", errors="replace").strip()
                if not text:
                    continue
                if self._on_stdout is not None:
                    try:
                        await self._on_stdout(self.name, text)
                    except asyncio.CancelledError:
                        raise
                    except Exception as e:  # noqa: BLE001 - one bad line must not kill the reader
                        self.log.error("failed to forward stdout line: %s", e)
        except asyncio.CancelledError:
            raise
        except Exception as e:  # noqa: BLE001
            self.log.error("error reading stdout: %s", e)
            return

        # EOF: the server closed stdout, so it is on its way out. Reap it and
        # report; do not respawn here (see module docstring).
        code = await process.wait()
        self.log.warning("exited with code %s", code)
        await self._emit_state(protocol.STATE_EXITED, exit_code=code)

    async def _emit_state(
        self,
        state: str,
        exit_code: Optional[int] = None,
        error: Optional[str] = None,
    ) -> None:
        self._state = state
        if self._on_state is None:
            return
        try:
            await self._on_state(self.name, state, exit_code, error)
        except asyncio.CancelledError:
            raise
        except Exception as e:  # noqa: BLE001 - reporting must never break supervision
            self.log.warning("could not report state %r: %s", state, e)

    def _remove_tempfile(self) -> None:
        if self.spec.fastmcp_tempfile is None:
            return
        try:
            self.spec.fastmcp_tempfile.unlink()
        except FileNotFoundError:
            pass
        except OSError as e:
            self.log.warning("could not remove %s: %s", self.spec.fastmcp_tempfile, e)
