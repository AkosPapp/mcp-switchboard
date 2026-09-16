"""Durable history of tool calls, backed by SQLite.

Every tool invocation that passes through the hub - from the web console, from
an MCP consumer such as n8n, or from the REST API - is appended here so the
console can show what happened after the fact. The store is deliberately
standalone: it knows nothing about tunnels, registries or MCP, it just takes a
CallRecord and persists it.

Retention is bounded twice over, by age and by row count, so an unattended hub
cannot grow its database without limit. Call purge() periodically (a daily
task is plenty).
"""

from __future__ import annotations

import asyncio
import json
import logging
import uuid
from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any, Optional

import aiosqlite

log = logging.getLogger(__name__)

STATUS_OK = "ok"
STATUS_ERROR = "error"

SOURCE_CONSOLE = "console"
SOURCE_MCP = "mcp"
SOURCE_API = "api"

# A console page never wants more than a screenful; this stops an API caller
# from asking for the whole table in one query.
MAX_LIMIT = 1000

_SCHEMA = """
CREATE TABLE IF NOT EXISTS calls (
    id                TEXT PRIMARY KEY,
    connection_id     TEXT NOT NULL,
    label             TEXT NOT NULL,
    server            TEXT NOT NULL,
    tool              TEXT NOT NULL,
    exposed_name      TEXT NOT NULL,
    arguments         TEXT NOT NULL,
    result            TEXT,
    error             TEXT,
    status            TEXT NOT NULL,
    source            TEXT NOT NULL,
    started_at        TEXT NOT NULL,
    started_at_epoch  REAL NOT NULL,
    duration_ms       REAL NOT NULL
);
CREATE INDEX IF NOT EXISTS calls_started_at_idx ON calls (started_at_epoch DESC);
CREATE INDEX IF NOT EXISTS calls_server_idx ON calls (server);
CREATE INDEX IF NOT EXISTS calls_tool_idx ON calls (tool);
CREATE INDEX IF NOT EXISTS calls_status_idx ON calls (status);
CREATE INDEX IF NOT EXISTS calls_label_idx ON calls (label);
"""

_COLUMNS = (
    "id, connection_id, label, server, tool, exposed_name, arguments, result, "
    "error, status, source, started_at, started_at_epoch, duration_ms"
)


def new_call_id() -> str:
    """A fresh call id. Hex rather than dashed uuid: it goes in URLs."""
    return uuid.uuid4().hex


def utcnow() -> datetime:
    return datetime.now(timezone.utc)


def _as_utc(value: datetime) -> datetime:
    """Force a datetime to timezone-aware UTC.

    Naive values are assumed to be UTC rather than local time - everything in
    the hub timestamps with utcnow(), and guessing local time here would make
    stored history depend on the server's TZ.
    """
    if value.tzinfo is None:
        return value.replace(tzinfo=timezone.utc)
    return value.astimezone(timezone.utc)


def _dumps(value: Any) -> str:
    """JSON for storage. Never raises: history must not break a tool call."""
    try:
        return json.dumps(value, default=str)
    except (TypeError, ValueError):  # pragma: no cover - default=str covers ~all
        return json.dumps({"unserializable": repr(value)})


def _loads(raw: str | None, fallback: Any) -> Any:
    if raw is None:
        return None
    try:
        return json.loads(raw)
    except (TypeError, ValueError):
        return fallback


@dataclass
class CallRecord:
    """One tool call, start to finish.

    Fields after `arguments` have defaults so an in-flight call can be built
    up before the result is known, but the field order is part of the contract
    - positional construction stays valid.
    """

    id: str
    connection_id: str
    label: str
    server: str
    tool: str
    exposed_name: str
    arguments: dict
    result: Optional[dict] = None
    error: Optional[str] = None
    status: str = STATUS_OK
    source: str = SOURCE_API
    started_at: datetime = field(default_factory=utcnow)
    duration_ms: float = 0.0

    def __post_init__(self) -> None:
        self.started_at = _as_utc(self.started_at)

    def to_json(self) -> dict:
        """camelCase payload, served verbatim by the HTTP API.

        startedAt is ISO-8601 with an explicit +00:00 offset: JavaScript's
        Date parses it and Python's fromisoformat round-trips it on 3.10,
        which a trailing "Z" does not.
        """
        return {
            "id": self.id,
            "connectionId": self.connection_id,
            "label": self.label,
            "server": self.server,
            "tool": self.tool,
            "exposedName": self.exposed_name,
            "arguments": self.arguments,
            "result": self.result,
            "error": self.error,
            "status": self.status,
            "source": self.source,
            "startedAt": self.started_at.isoformat(),
            "durationMs": self.duration_ms,
        }

    @classmethod
    def from_row(cls, row: Any) -> "CallRecord":
        started_raw = row["started_at"]
        try:
            started = _as_utc(datetime.fromisoformat(started_raw))
        except (TypeError, ValueError):
            started = datetime.fromtimestamp(row["started_at_epoch"], tz=timezone.utc)
        return cls(
            id=row["id"],
            connection_id=row["connection_id"],
            label=row["label"],
            server=row["server"],
            tool=row["tool"],
            exposed_name=row["exposed_name"],
            arguments=_loads(row["arguments"], {}) or {},
            result=_loads(row["result"], None),
            error=row["error"],
            status=row["status"],
            source=row["source"],
            started_at=started,
            duration_ms=row["duration_ms"],
        )


class CallStore:
    """Async SQLite-backed call history.

    One connection, serialized by a lock. aiosqlite already runs statements on
    a dedicated thread, so the lock exists only to keep multi-statement work
    (purge) atomic with respect to concurrent writers.
    """

    def __init__(
        self,
        db_path: Path,
        retention_days: int = 30,
        max_rows: int = 100_000,
    ) -> None:
        self.db_path = Path(db_path)
        self.retention_days = retention_days
        self.max_rows = max_rows
        self._db: aiosqlite.Connection | None = None
        self._lock = asyncio.Lock()

    async def start(self) -> None:
        if self._db is not None:
            return
        self.db_path.parent.mkdir(parents=True, exist_ok=True)
        db = await aiosqlite.connect(self.db_path)
        db.row_factory = aiosqlite.Row
        # WAL keeps console reads from blocking tunnel writes; NORMAL is the
        # right durability trade for a log of calls that already happened.
        await db.execute("PRAGMA journal_mode=WAL")
        await db.execute("PRAGMA synchronous=NORMAL")
        await db.executescript(_SCHEMA)
        await db.commit()
        self._db = db

    async def close(self) -> None:
        db, self._db = self._db, None
        if db is not None:
            await db.close()

    @property
    def _conn(self) -> aiosqlite.Connection:
        if self._db is None:
            raise RuntimeError("CallStore.start() must be awaited before use")
        return self._db

    async def record(self, rec: CallRecord) -> None:
        started = _as_utc(rec.started_at)
        async with self._lock:
            await self._conn.execute(
                f"INSERT OR REPLACE INTO calls ({_COLUMNS}) "
                "VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
                (
                    rec.id,
                    rec.connection_id,
                    rec.label,
                    rec.server,
                    rec.tool,
                    rec.exposed_name,
                    _dumps(rec.arguments if rec.arguments is not None else {}),
                    None if rec.result is None else _dumps(rec.result),
                    rec.error,
                    rec.status,
                    rec.source,
                    started.isoformat(),
                    started.timestamp(),
                    float(rec.duration_ms),
                ),
            )
            await self._conn.commit()

    async def list(
        self,
        *,
        limit: int = 100,
        offset: int = 0,
        server: str | None = None,
        tool: str | None = None,
        status: str | None = None,
        label: str | None = None,
    ) -> list[CallRecord]:
        """Newest first. Unknown filters simply match nothing."""
        limit = max(0, min(int(limit), MAX_LIMIT))
        offset = max(0, int(offset))

        where: list[str] = []
        params: list[Any] = []
        for column, value in (
            ("server", server),
            ("tool", tool),
            ("status", status),
            ("label", label),
        ):
            if value is not None:
                where.append(f"{column} = ?")
                params.append(value)

        sql = f"SELECT {_COLUMNS} FROM calls"
        if where:
            sql += " WHERE " + " AND ".join(where)
        # rowid breaks ties so pagination is stable when timestamps collide.
        sql += " ORDER BY started_at_epoch DESC, rowid DESC LIMIT ? OFFSET ?"
        params.extend([limit, offset])

        async with self._conn.execute(sql, params) as cur:
            rows = await cur.fetchall()
        return [CallRecord.from_row(row) for row in rows]

    async def get(self, call_id: str) -> CallRecord | None:
        async with self._conn.execute(
            f"SELECT {_COLUMNS} FROM calls WHERE id = ?", (call_id,)
        ) as cur:
            row = await cur.fetchone()
        return CallRecord.from_row(row) if row is not None else None

    async def purge(self) -> int:
        """Apply both retention rules, returning the number of rows deleted."""
        deleted = 0
        async with self._lock:
            if self.retention_days and self.retention_days > 0:
                cutoff = (utcnow() - timedelta(days=self.retention_days)).timestamp()
                cur = await self._conn.execute(
                    "DELETE FROM calls WHERE started_at_epoch < ?", (cutoff,)
                )
                deleted += cur.rowcount or 0

            if self.max_rows and self.max_rows > 0:
                cur = await self._conn.execute(
                    "DELETE FROM calls WHERE rowid NOT IN ("
                    "  SELECT rowid FROM calls"
                    "  ORDER BY started_at_epoch DESC, rowid DESC"
                    "  LIMIT ?"
                    ")",
                    (self.max_rows,),
                )
                deleted += cur.rowcount or 0

            await self._conn.commit()

        if deleted:
            log.info("purged %d call records", deleted)
        return deleted

    async def stats(self) -> dict:
        async with self._conn.execute(
            "SELECT status, COUNT(*) AS n FROM calls GROUP BY status"
        ) as cur:
            rows = await cur.fetchall()
        by_status = {row["status"]: row["n"] for row in rows}
        return {
            "total": sum(by_status.values()),
            "ok": by_status.get(STATUS_OK, 0),
            "error": by_status.get(STATUS_ERROR, 0),
        }
