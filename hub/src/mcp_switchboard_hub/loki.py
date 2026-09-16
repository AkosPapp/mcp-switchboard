"""Fire-and-forget structured logging to Grafana Loki.

The hub serves tool calls; shipping logs is strictly secondary to that. So the
producer side (emit) is synchronous, non-blocking and cannot raise, and all the
network work happens on one background task that batches whatever has piled up.
If Loki is slow, wedged or gone, events are counted as dropped and the hub
carries on at full speed.

LABEL DISCIPLINE: Loki indexes stream labels, so every distinct combination of
label values creates a stream. Labels here must stay low-cardinality - machine
label, server name, event name, level. Never pass tool arguments, call ids,
error strings, durations or anything else unbounded as a label; those belong in
`fields`, which is serialized into the log line and not indexed.
"""

from __future__ import annotations

import asyncio
import json
import logging
import re
import time
from collections import deque
from contextlib import suppress
from typing import TYPE_CHECKING, Any

import httpx

if TYPE_CHECKING:  # pragma: no cover - typing only
    from .metrics import Metrics

log = logging.getLogger(__name__)

PUSH_PATH = "/loki/api/v1/push"

# Loki label names are [a-zA-Z_][a-zA-Z0-9_]*; anything else is rewritten.
_LABEL_NAME_BAD = re.compile(r"[^a-zA-Z0-9_]")

# A backstop against a caller that ignores the label discipline above: a long
# value gets truncated rather than minting an unbounded number of streams.
MAX_LABEL_VALUE = 128


class LokiExporter:
    """Batches structured events and pushes them to Loki.

    `transport` is a test seam (httpx.MockTransport or similar); production
    callers leave it None.
    """

    def __init__(
        self,
        *,
        url: str,
        labels: dict[str, str] | None = None,
        enabled: bool = False,
        batch_size: int = 100,
        flush_interval: float = 2.0,
        queue_size: int = 10_000,
        metrics: "Metrics | None" = None,
        timeout: float = 5.0,
        transport: httpx.AsyncBaseTransport | None = None,
    ) -> None:
        self.url = (url or "").rstrip("/")
        self.static_labels = _clean_labels(labels or {})
        # A configured-on exporter with no URL would drop everything while
        # pretending to work; treat it as off.
        self.enabled = bool(enabled and self.url)
        self.batch_size = max(1, int(batch_size))
        self.flush_interval = max(0.01, float(flush_interval))
        self.queue_size = max(1, int(queue_size))
        self.metrics = metrics
        self.timeout = float(timeout)
        self.push_url = self.url + PUSH_PATH

        self._transport = transport
        self._client: httpx.AsyncClient | None = None
        self._task: asyncio.Task | None = None
        self._queue: deque[tuple[int, tuple[tuple[str, str], ...], str]] = deque()
        self._wake = asyncio.Event()
        self._closing = False
        self._failing = False
        self._failed_batches = 0
        # Shutdown must not hang on an unreachable Loki.
        self._close_budget = max(1.0, self.timeout)

    # ---------------------------------------------------------------- producer

    def emit(self, event: str, fields: dict, labels: dict[str, str] | None = None) -> None:
        """Queue one event. Never blocks, never awaits, never raises.

        Called from request paths, including tool-call handling, so every
        failure mode here is a drop plus a counter bump.
        """
        if not self.enabled:
            # Loki is simply not configured: that is not a loss, so it is not
            # counted as a drop (the counter would otherwise tick on every
            # event for everyone who does not run Loki).
            return
        try:
            entry = self._build(event, fields, labels)
        except Exception:  # pragma: no cover - _build is already defensive
            self._drop(1)
            return

        if len(self._queue) >= self.queue_size:
            # Drop the new event rather than the backlog: the oldest entries
            # are the ones already being flushed.
            self._drop(1)
            return

        self._queue.append(entry)
        if len(self._queue) >= self.batch_size:
            self._wake_soon()

    def _build(
        self, event: str, fields: dict, labels: dict[str, str] | None
    ) -> tuple[int, tuple[tuple[str, str], ...], str]:
        stream = dict(self.static_labels)
        stream["event"] = _clean_value(str(event))
        if labels:
            stream.update(_clean_labels(labels))

        line: dict[str, Any] = {"event": event}
        if fields:
            line.update(fields)
        try:
            payload = json.dumps(line, separators=(",", ":"), default=str)
        except (TypeError, ValueError):
            payload = json.dumps({"event": event, "error": "unserializable fields"})

        # Serialize now, not at flush time: the caller is free to mutate its
        # fields dict the moment emit() returns.
        return (time.time_ns(), tuple(sorted(stream.items())), payload)

    def _drop(self, n: int = 1) -> None:
        if self.metrics is not None:
            with suppress(Exception):
                self.metrics.count_loki_dropped(n)

    def _wake_soon(self) -> None:
        with suppress(Exception):
            self._wake.set()

    @property
    def pending(self) -> int:
        return len(self._queue)

    # ---------------------------------------------------------------- lifecycle

    async def start(self) -> None:
        if not self.enabled or self._task is not None:
            return
        self._closing = False
        self._client = httpx.AsyncClient(
            timeout=self.timeout,
            transport=self._transport,
            headers={"Content-Type": "application/json"},
        )
        self._task = asyncio.create_task(self._run(), name="loki-exporter")

    async def close(self) -> None:
        self._closing = True
        self._wake_soon()

        task, self._task = self._task, None
        if task is not None:
            # wait_for cancels the task itself once the budget is spent, so a
            # wedged POST cannot hold shutdown open.
            with suppress(asyncio.TimeoutError, asyncio.CancelledError):
                await asyncio.wait_for(task, timeout=self._close_budget)
            if task.done() and not task.cancelled() and task.exception() is not None:
                log.debug("loki exporter task failed", exc_info=task.exception())

        if self._client is not None:
            # Anything the task did not get to. Bounded twice - by a deadline
            # between batches and by wait_for around the whole drain - because
            # shutdown is not allowed to hang on an unresponsive Loki.
            with suppress(Exception):
                await asyncio.wait_for(
                    self._drain(deadline=time.monotonic() + self._close_budget),
                    timeout=self._close_budget,
                )
            with suppress(Exception):
                await self._client.aclose()
            self._client = None

        dropped = len(self._queue)
        if dropped:
            self._drop(dropped)
            self._queue.clear()

    # ----------------------------------------------------------------- consumer

    async def _run(self) -> None:
        while True:
            try:
                await asyncio.wait_for(self._wake.wait(), timeout=self.flush_interval)
            except asyncio.TimeoutError:
                pass  # the periodic tick, not an error
            self._wake.clear()

            # A closing drain gets a deadline; a normal one runs until the
            # queue is empty.
            closing = self._closing
            deadline = time.monotonic() + self._close_budget if closing else None
            try:
                await self._drain(deadline=deadline)
            except asyncio.CancelledError:
                raise
            except Exception:  # pragma: no cover - _post swallows its own errors
                log.debug("loki flush failed", exc_info=True)

            if closing:
                return

    async def _drain(self, *, deadline: float | None = None) -> None:
        while self._queue:
            if deadline is not None and time.monotonic() >= deadline:
                return
            batch: list[tuple[int, tuple[tuple[str, str], ...], str]] = []
            while self._queue and len(batch) < self.batch_size:
                batch.append(self._queue.popleft())
            if batch:
                await self._post(batch)

    async def _post(self, batch: list[tuple[int, tuple[tuple[str, str], ...], str]]) -> None:
        client = self._client
        if client is None:
            self._drop(len(batch))
            return

        payload = build_payload(batch)
        try:
            response = await client.post(self.push_url, json=payload)
            if response.status_code >= 400:
                raise RuntimeError(f"HTTP {response.status_code}: {response.text[:200]}")
        except asyncio.CancelledError:
            # Shutdown cut the flush short; account for the batch before going.
            self._drop(len(batch))
            raise
        except Exception as exc:
            self._drop(len(batch))
            self._note_failure(exc)
        else:
            self._note_success()

    def _note_failure(self, exc: BaseException) -> None:
        self._failed_batches += 1
        if not self._failing:
            # One warning per outage, not one per batch: a down Loki must not
            # drown the hub's own logs.
            self._failing = True
            log.warning("loki push to %s failed, dropping events: %s", self.push_url, exc)

    def _note_success(self) -> None:
        if self._failing:
            log.info("loki push to %s recovered after %d failed batches", self.push_url, self._failed_batches)
            self._failing = False
            self._failed_batches = 0


def build_payload(
    batch: list[tuple[int, tuple[tuple[str, str], ...], str]]
) -> dict[str, Any]:
    """Group entries sharing a label set into Loki streams."""
    streams: dict[tuple[tuple[str, str], ...], list[list[str]]] = {}
    for ts_ns, stream_key, line in batch:
        streams.setdefault(stream_key, []).append([str(ts_ns), line])
    return {
        "streams": [
            {"stream": dict(key), "values": sorted(values, key=lambda v: int(v[0]))}
            for key, values in streams.items()
        ]
    }


def _clean_labels(labels: dict[str, Any]) -> dict[str, str]:
    cleaned: dict[str, str] = {}
    for key, value in labels.items():
        name = _clean_name(str(key))
        if name:
            cleaned[name] = _clean_value(value)
    return cleaned


def _clean_name(name: str) -> str:
    name = _LABEL_NAME_BAD.sub("_", name)
    if name and name[0].isdigit():
        name = "_" + name
    return name


def _clean_value(value: Any) -> str:
    text = value if isinstance(value, str) else str(value)
    return text[:MAX_LABEL_VALUE]
