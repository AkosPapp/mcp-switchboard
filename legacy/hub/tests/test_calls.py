"""CallStore against a real on-disk SQLite database."""

from __future__ import annotations

from datetime import datetime, timedelta, timezone

import pytest
import pytest_asyncio

from mcp_switchboard_hub.calls import CallRecord, CallStore, new_call_id

pytestmark = pytest.mark.asyncio


def make_record(
    *,
    tool: str = "git_status",
    server: str = "git",
    label: str = "legion5",
    status: str = "ok",
    source: str = "mcp",
    started_at: datetime | None = None,
    result: dict | None = None,
    error: str | None = None,
    duration_ms: float = 12.5,
) -> CallRecord:
    return CallRecord(
        id=new_call_id(),
        connection_id="conn-1",
        label=label,
        server=server,
        tool=tool,
        exposed_name=f"{label}__{server}__{tool}",
        arguments={"repo": "/srv/code", "porcelain": True},
        result=result,
        error=error,
        status=status,
        source=source,
        started_at=started_at or datetime.now(timezone.utc),
        duration_ms=duration_ms,
    )


@pytest_asyncio.fixture
async def store(tmp_path):
    s = CallStore(tmp_path / "nested" / "calls.db")
    await s.start()
    try:
        yield s
    finally:
        await s.close()


async def test_round_trip(store, tmp_path):
    rec = make_record(result={"content": [{"type": "text", "text": "clean"}]})
    await store.record(rec)

    got = await store.get(rec.id)
    assert got is not None
    assert got.id == rec.id
    assert got.connection_id == "conn-1"
    assert got.label == "legion5"
    assert got.server == "git"
    assert got.tool == "git_status"
    assert got.exposed_name == "legion5__git__git_status"
    assert got.arguments == {"repo": "/srv/code", "porcelain": True}
    assert got.result == {"content": [{"type": "text", "text": "clean"}]}
    assert got.error is None
    assert got.status == "ok"
    assert got.source == "mcp"
    assert got.duration_ms == pytest.approx(12.5)
    # SQLite stores an ISO string; sub-second precision must survive.
    assert got.started_at == rec.started_at
    assert got.started_at.tzinfo is not None

    assert (tmp_path / "nested" / "calls.db").is_file()


async def test_to_json_is_camel_case(store):
    started = datetime(2026, 9, 16, 10, 30, 0, tzinfo=timezone.utc)
    rec = make_record(started_at=started, status="error", error="boom", duration_ms=3.0)
    await store.record(rec)

    payload = (await store.get(rec.id)).to_json()
    assert set(payload) == {
        "id",
        "connectionId",
        "label",
        "server",
        "tool",
        "exposedName",
        "arguments",
        "result",
        "error",
        "status",
        "source",
        "startedAt",
        "durationMs",
    }
    assert payload["startedAt"] == "2026-09-16T10:30:00+00:00"
    assert payload["exposedName"] == "legion5__git__git_status"
    assert payload["error"] == "boom"
    assert payload["durationMs"] == 3.0


async def test_naive_started_at_is_treated_as_utc(store):
    rec = make_record(started_at=datetime(2026, 1, 2, 3, 4, 5))
    assert rec.started_at.tzinfo is timezone.utc
    await store.record(rec)
    assert (await store.get(rec.id)).started_at == datetime(
        2026, 1, 2, 3, 4, 5, tzinfo=timezone.utc
    )


async def test_list_is_newest_first(store):
    base = datetime(2026, 9, 1, tzinfo=timezone.utc)
    ids = []
    for i in range(5):
        rec = make_record(tool=f"tool{i}", started_at=base + timedelta(minutes=i))
        ids.append(rec.id)
        await store.record(rec)

    rows = await store.list()
    assert [r.id for r in rows] == list(reversed(ids))
    assert [r.tool for r in rows] == ["tool4", "tool3", "tool2", "tool1", "tool0"]


async def test_list_limit_and_offset(store):
    base = datetime(2026, 9, 1, tzinfo=timezone.utc)
    for i in range(10):
        await store.record(make_record(tool=f"t{i}", started_at=base + timedelta(minutes=i)))

    page1 = await store.list(limit=3)
    page2 = await store.list(limit=3, offset=3)
    assert [r.tool for r in page1] == ["t9", "t8", "t7"]
    assert [r.tool for r in page2] == ["t6", "t5", "t4"]
    assert await store.list(limit=0) == []


async def test_list_filters(store):
    base = datetime(2026, 9, 1, tzinfo=timezone.utc)
    await store.record(
        make_record(label="legion5", server="git", tool="git_status", started_at=base)
    )
    await store.record(
        make_record(
            label="legion5",
            server="fs",
            tool="read_file",
            status="error",
            error="nope",
            started_at=base + timedelta(minutes=1),
        )
    )
    await store.record(
        make_record(
            label="nas", server="git", tool="git_log", started_at=base + timedelta(minutes=2)
        )
    )

    assert [r.tool for r in await store.list(server="git")] == ["git_log", "git_status"]
    assert [r.tool for r in await store.list(label="legion5")] == ["read_file", "git_status"]
    assert [r.tool for r in await store.list(status="error")] == ["read_file"]
    assert [r.tool for r in await store.list(tool="git_log")] == ["git_log"]
    assert [r.tool for r in await store.list(label="legion5", server="git")] == ["git_status"]
    assert await store.list(server="does-not-exist") == []


async def test_get_miss_returns_none(store):
    assert await store.get("no-such-call") is None


async def test_stats(store):
    for _ in range(3):
        await store.record(make_record(status="ok"))
    await store.record(make_record(status="error", error="boom"))

    assert await store.stats() == {"total": 4, "ok": 3, "error": 1}


async def test_stats_on_empty_store(store):
    assert await store.stats() == {"total": 0, "ok": 0, "error": 0}


async def test_purge_by_age(tmp_path):
    store = CallStore(tmp_path / "calls.db", retention_days=30, max_rows=1000)
    await store.start()
    try:
        now = datetime.now(timezone.utc)
        fresh = make_record(tool="fresh", started_at=now - timedelta(days=1))
        stale = make_record(tool="stale", started_at=now - timedelta(days=31))
        ancient = make_record(tool="ancient", started_at=now - timedelta(days=400))
        for rec in (fresh, stale, ancient):
            await store.record(rec)

        assert await store.purge() == 2
        assert [r.tool for r in await store.list()] == ["fresh"]
        assert await store.get(stale.id) is None
        # Nothing left to do the second time around.
        assert await store.purge() == 0
    finally:
        await store.close()


async def test_purge_by_row_count(tmp_path):
    store = CallStore(tmp_path / "calls.db", retention_days=3650, max_rows=3)
    await store.start()
    try:
        base = datetime.now(timezone.utc) - timedelta(hours=10)
        for i in range(7):
            await store.record(make_record(tool=f"t{i}", started_at=base + timedelta(minutes=i)))

        assert await store.purge() == 4
        assert [r.tool for r in await store.list()] == ["t6", "t5", "t4"]
        assert (await store.stats())["total"] == 3
    finally:
        await store.close()


async def test_purge_applies_both_rules(tmp_path):
    store = CallStore(tmp_path / "calls.db", retention_days=30, max_rows=2)
    await store.start()
    try:
        now = datetime.now(timezone.utc)
        for i in range(3):
            await store.record(make_record(tool=f"old{i}", started_at=now - timedelta(days=40 + i)))
        for i in range(4):
            await store.record(make_record(tool=f"new{i}", started_at=now - timedelta(minutes=i)))

        # 3 dropped by age, then 2 more of the 4 survivors dropped by count.
        assert await store.purge() == 5
        assert [r.tool for r in await store.list()] == ["new0", "new1"]
    finally:
        await store.close()


async def test_data_survives_reopen(tmp_path):
    path = tmp_path / "calls.db"
    store = CallStore(path)
    await store.start()
    rec = make_record()
    await store.record(rec)
    await store.close()

    reopened = CallStore(path)
    await reopened.start()
    try:
        assert (await reopened.get(rec.id)).tool == "git_status"
        assert (await reopened.stats())["total"] == 1
    finally:
        await reopened.close()


async def test_wal_mode_is_enabled(tmp_path):
    store = CallStore(tmp_path / "calls.db")
    await store.start()
    try:
        async with store._conn.execute("PRAGMA journal_mode") as cur:
            row = await cur.fetchone()
        assert row[0].lower() == "wal"
    finally:
        await store.close()


async def test_use_before_start_is_a_clear_error(tmp_path):
    store = CallStore(tmp_path / "calls.db")
    with pytest.raises(RuntimeError, match="start"):
        await store.get("x")


async def test_unserializable_arguments_do_not_break_recording(store):
    rec = make_record()
    rec.arguments = {"when": datetime(2026, 9, 16, tzinfo=timezone.utc)}
    await store.record(rec)
    # Stored via default=str rather than exploding in the tool-call path.
    assert (await store.get(rec.id)).arguments == {"when": "2026-09-16 00:00:00+00:00"}
