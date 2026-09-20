"""Deterministic regression coverage for the restart race in _ClientConnection.

The bug: `_stop_channel` used to set the stopper event and return immediately,
without waiting for the retiring owner task to actually finish tearing down.
A fast restart (state frames arriving as "starting" then "running" in quick
succession) could then have `_start_channel` install a NEW owner task while
the OLD one was still unwinding in the background - and the two would race
over `self._inbound[name]` / `channel.session`, with the old task's `finally`
sometimes clobbering the new registration. Symptom reported in the field:
"unhandled errors in a TaskGroup (1 sub-exception)" logged, server left
running but with no tools.

Reproducing that race against a real subprocess is inherently timing-dependent
(see tests/test_end_to_end.py::test_rapid_restarts_do_not_lose_tools, which
did not reliably reproduce it either way in this environment). This test
instead drives `_stop_channel`/`_start_channel` directly with a scripted,
artificially slow "owner task", so the assertion is deterministic: the fix's
actual guarantee is that `_stop_channel` does not return until the old task
has fully finished, which makes the overlap structurally impossible rather
than merely unlikely.
"""

from __future__ import annotations

import anyio
import pytest

from mcp_switchboard_hub.registry import Registry, ServerChannel
from mcp_switchboard_hub.tunnel import _ClientConnection


def make_connection() -> _ClientConnection:
    return _ClientConnection(
        websocket=None, registry=Registry(), tools_timeout=1.0, on_change=None, metrics=None
    )


@pytest.mark.anyio
async def test_stop_channel_waits_for_the_old_owner_task_to_finish():
    conn = make_connection()
    channel = ServerChannel(name="demo", connection_id="c1", label="legion5")

    order = []

    async def slow_owner_task(stopper: anyio.Event, done: anyio.Event) -> None:
        order.append("started")
        await stopper.wait()
        # Simulate the real teardown taking a moment (session shutdown,
        # subprocess I/O, etc) - this is the window the bug fell into.
        await anyio.sleep(0.2)
        order.append("torn down")
        done.set()

    async with anyio.create_task_group() as tg:
        conn._tg = tg
        stopper = anyio.Event()
        done = anyio.Event()
        conn._stoppers[channel.name] = stopper
        conn._done[channel.name] = done
        tg.start_soon(slow_owner_task, stopper, done)
        await anyio.wait_all_tasks_blocked()  # let it reach stopper.wait()

        await conn._stop_channel(channel)
        order.append("stop_channel returned")

    # If _stop_channel returned early (the old bug), "stop_channel returned"
    # would appear before "torn down" - exactly the overlap that let a new
    # owner task start while the old one was still cleaning up.
    assert order == ["started", "torn down", "stop_channel returned"]


@pytest.mark.anyio
async def test_stop_channel_does_not_hang_if_the_old_task_never_finishes():
    """The wait is bounded, so a genuinely stuck server can't wedge the
    receive loop forever."""
    conn = make_connection()
    conn.STOP_WAIT_TIMEOUT = 0.05  # don't actually wait out the real 5s default in a test
    channel = ServerChannel(name="demo", connection_id="c1", label="legion5")

    conn._stoppers[channel.name] = anyio.Event()
    conn._done[channel.name] = anyio.Event()  # never set - simulates a stuck owner task

    with anyio.fail_after(2.0):
        await conn._stop_channel(channel)

    assert channel.name not in conn._stoppers
    assert channel.name not in conn._done


@pytest.mark.anyio
async def test_start_channel_cannot_run_concurrently_with_a_stopping_channel():
    """End-to-end through the public methods: start, stop, start again - the
    second start must not begin until the first owner task's stop is done."""
    conn = make_connection()
    channel = ServerChannel(name="demo", connection_id="c1", label="legion5")

    active = {"count": 0}
    max_concurrent = {"value": 0}

    async def owner_task(ch, stopper, done):
        active["count"] += 1
        max_concurrent["value"] = max(max_concurrent["value"], active["count"])
        try:
            await stopper.wait()
            await anyio.sleep(0.1)
        finally:
            active["count"] -= 1
            done.set()

    # Patch _own_channel so we can control its shape without a real MCP session.
    conn._own_channel = owner_task

    async with anyio.create_task_group() as tg:
        conn._tg = tg
        await conn._start_channel(channel)
        await anyio.wait_all_tasks_blocked()

        await conn._stop_channel(channel)
        await conn._start_channel(channel)
        await anyio.wait_all_tasks_blocked()

        await conn._stop_channel(channel)

    assert max_concurrent["value"] == 1
