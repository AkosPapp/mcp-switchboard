"""W4 (P0-B): long waits via wait_for_start / wait_for_poll — no blocking call,
so no hub per-call deadline can kill a wait.

Predicate semantics recap: the watch is satisfied when a predicate run exits
NON-ZERO ("wait until this stops being true"), e.g. `test -e marker` clears
when the marker disappears.
"""

import asyncio
import os

import pytest

from mcp_switchboard_server_harness import server as h


def run(coro):
    return asyncio.run(coro)


async def _poll_until(id: str, pred, timeout=20):
    loop = asyncio.get_event_loop()
    end = loop.time() + timeout
    p = None
    while loop.time() < end:
        p = await h.wait_for_poll(id)
        if pred(p):
            return p
        await asyncio.sleep(0.1)
    raise AssertionError(f"watch {id} never reached the expected state: {p}")


def test_watch_satisfied_after_a_few_polls():
    async def scenario(marker):
        w = await h.wait_for_start(until=f"test -e {marker}", poll=0.15, timeout=30)
        first = await h.wait_for_poll(w.id)
        assert not first.done and first.stdout == ""
        await asyncio.sleep(0.4)  # at least two polls run while the condition holds
        os.unlink(marker)
        return await _poll_until(w.id, lambda p: p.done)

    import pathlib

    marker = pathlib.Path(h._root) / "queued.txt"
    marker.write_text("")
    p = run(scenario(marker))
    assert p.satisfied and p.polls >= 2 and p.exit_code != 0
    assert p.elapsed_s > 0


def test_watch_timeout_reports_partial_output():
    async def scenario():
        w = await h.wait_for_start(until="printf 'still-true ' && exit 0", poll=0.15, timeout=0.7)
        return await _poll_until(w.id, lambda p: p.done)

    p = run(scenario())
    assert p.done and not p.satisfied
    assert p.exit_code is None or p.exit_code == 0
    assert p.stdout.startswith("still-true")  # output produced before the timeout survives
    assert p.polls >= 2


def test_unknown_watch_id_errors():
    with pytest.raises(ValueError, match="unknown watcher id"):
        run(h.wait_for_poll("deadbeef"))


def test_watch_args_validated():
    with pytest.raises(ValueError):
        run(h.wait_for_start(until="true", timeout=0))
    with pytest.raises(ValueError):
        run(h.wait_for_start(until="true", timeout=h.MAX_WATCH + 1))
    with pytest.raises(ValueError):
        run(h.wait_for_start(until="true", poll=0.01))
    with pytest.raises(ValueError):
        run(h.wait_for_start(until="   "))


def test_watches_share_the_process_capacity(monkeypatch):
    async def scenario():
        monkeypatch.setattr(h, "MAX_PROCESSES", 2)
        h._kill_all_processes()  # start from an empty pool regardless of suite order
        h._processes.clear()
        h._watchers.clear()
        a = await h.wait_for_start(until="false", poll=5, timeout=60)  # non-zero exit → satisfied instantly
        b = await h.process_start("sleep 30")
        p = await _poll_until(a.id, lambda p: p.done)
        await h.wait_for_poll(a.id)  # drains the finished watch so it frees its slot
        await h.wait_for_start(until="false", poll=5, timeout=60)  # allowed: 2 live again
        with pytest.raises(RuntimeError, match="too many"):
            await h.process_start("sleep 5")
        await h.process_kill(b.id)
        return p

    p = run(scenario())
    assert p.done and p.satisfied
