"""LokiExporter: batching, payload shape, and never hurting the caller.

Most tests drive a real httpx.AsyncClient over a MockTransport so request
building and JSON encoding are exercised for real; one test runs against an
actual socket server to pin the wire format.
"""

from __future__ import annotations

import asyncio
import json
import logging
import time

import httpx
import pytest

from mcp_switchboard_hub.loki import LokiExporter
from mcp_switchboard_hub.metrics import Metrics

pytestmark = pytest.mark.asyncio

URL = "http://loki.test:3100"


def recording_transport(sink: list, *, status: int = 204, raises: Exception | None = None):
    def handler(request: httpx.Request) -> httpx.Response:
        sink.append({"url": str(request.url), "json": json.loads(request.content)})
        if raises is not None:
            raise raises
        return httpx.Response(status)

    return httpx.MockTransport(handler)


async def wait_until(predicate, timeout: float = 3.0) -> bool:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if predicate():
            return True
        await asyncio.sleep(0.01)
    return bool(predicate())


def dropped(metrics: Metrics) -> float:
    return metrics.registry.get_sample_value("mcpsb_loki_dropped_total")


async def test_push_payload_shape():
    sink: list = []
    exporter = LokiExporter(
        url=URL,
        labels={"service": "mcp-switchboard", "env": "prod"},
        enabled=True,
        flush_interval=0.05,
        transport=recording_transport(sink),
    )
    await exporter.start()
    before_ns = time.time_ns()
    exporter.emit("tool_call", {"tool": "git_status", "duration_ms": 12.5}, {"label": "legion5"})
    assert await wait_until(lambda: sink)
    await exporter.close()
    after_ns = time.time_ns()

    assert sink[0]["url"] == "http://loki.test:3100/loki/api/v1/push"
    payload = sink[0]["json"]
    assert list(payload) == ["streams"]
    (stream,) = payload["streams"]
    assert stream["stream"] == {
        "service": "mcp-switchboard",
        "env": "prod",
        "event": "tool_call",
        "label": "legion5",
    }
    (entry,) = stream["values"]
    ts, line = entry
    assert isinstance(ts, str) and ts.isdigit()
    assert before_ns <= int(ts) <= after_ns  # nanoseconds, not seconds or millis
    assert json.loads(line) == {
        "event": "tool_call",
        "tool": "git_status",
        "duration_ms": 12.5,
    }


async def test_batches_multiple_events_per_request():
    sink: list = []
    exporter = LokiExporter(
        url=URL,
        enabled=True,
        batch_size=3,
        flush_interval=0.05,
        transport=recording_transport(sink),
    )
    await exporter.start()
    for i in range(7):
        exporter.emit("frame", {"n": i})
    assert await wait_until(lambda: sum(len(s["json"]["streams"][0]["values"]) for s in sink) == 7)
    await exporter.close()

    # 7 events at batch_size 3 is three POSTs, not seven.
    assert [len(s["json"]["streams"][0]["values"]) for s in sink] == [3, 3, 1]
    lines = [
        json.loads(v[1])["n"] for s in sink for v in s["json"]["streams"][0]["values"]
    ]
    assert lines == list(range(7))


async def test_one_request_carries_several_streams():
    sink: list = []
    exporter = LokiExporter(
        url=URL,
        labels={"service": "hub"},
        enabled=True,
        flush_interval=0.05,
        transport=recording_transport(sink),
    )
    await exporter.start()
    exporter.emit("tool_call", {"n": 1}, {"label": "legion5"})
    exporter.emit("tool_call", {"n": 2}, {"label": "legion5"})
    exporter.emit("connect", {"n": 3}, {"label": "nas"})
    assert await wait_until(lambda: sink)
    await exporter.close()

    streams = sink[0]["json"]["streams"]
    by_label = {tuple(sorted(s["stream"].items())): s for s in streams}
    assert len(by_label) == 2
    legion = by_label[
        tuple(sorted({"service": "hub", "event": "tool_call", "label": "legion5"}.items()))
    ]
    assert len(legion["values"]) == 2
    assert [int(v[0]) for v in legion["values"]] == sorted(int(v[0]) for v in legion["values"])


async def test_labels_are_sanitised_and_bounded():
    sink: list = []
    exporter = LokiExporter(
        url=URL,
        enabled=True,
        flush_interval=0.05,
        transport=recording_transport(sink),
    )
    await exporter.start()
    exporter.emit("e", {}, {"machine-label": "a" * 500, "9bad": "x"})
    assert await wait_until(lambda: sink)
    await exporter.close()

    stream = sink[0]["json"]["streams"][0]["stream"]
    assert stream["machine_label"] == "a" * 128
    assert stream["_9bad"] == "x"


async def test_emit_is_a_noop_while_disabled():
    sink: list = []
    metrics = Metrics()
    exporter = LokiExporter(
        url=URL,
        enabled=False,
        metrics=metrics,
        flush_interval=0.05,
        transport=recording_transport(sink),
    )
    await exporter.start()
    for _ in range(50):
        exporter.emit("tool_call", {"tool": "git_status"})
    await asyncio.sleep(0.15)
    await exporter.close()

    assert sink == []
    assert exporter.pending == 0
    # Not running Loki is a configuration choice, not a dropped log.
    assert dropped(metrics) == 0.0


async def test_enabled_without_url_is_disabled():
    exporter = LokiExporter(url="", enabled=True)
    assert exporter.enabled is False
    await exporter.start()
    exporter.emit("e", {})
    assert exporter.pending == 0
    await exporter.close()


async def test_queue_full_drops_are_counted():
    metrics = Metrics()
    exporter = LokiExporter(
        url=URL,
        enabled=True,
        queue_size=2,
        batch_size=100,
        flush_interval=30.0,
        metrics=metrics,
        transport=recording_transport([]),
    )
    # Not started: nothing drains, so the queue fills and stays full.
    for i in range(10):
        exporter.emit("e", {"n": i})

    assert exporter.pending == 2
    assert dropped(metrics) == 8.0
    # Oldest entries are kept; the newest are the ones shed.
    assert [json.loads(e[2])["n"] for e in exporter._queue] == [0, 1]
    await exporter.close()


async def test_failing_sink_never_raises_and_counts_drops(caplog):
    metrics = Metrics()
    exporter = LokiExporter(
        url=URL,
        enabled=True,
        batch_size=2,
        flush_interval=0.05,
        metrics=metrics,
        transport=recording_transport([], raises=httpx.ConnectError("loki is down")),
    )
    with caplog.at_level(logging.WARNING, logger="mcp_switchboard_hub.loki"):
        await exporter.start()
        for i in range(6):
            exporter.emit("tool_call", {"n": i})  # must not raise
        assert await wait_until(lambda: dropped(metrics) >= 6.0)
        await exporter.close()

    assert dropped(metrics) == 6.0
    # One warning for the whole outage, not one per failed batch.
    warnings = [r for r in caplog.records if r.levelno >= logging.WARNING]
    assert len(warnings) == 1
    assert "loki" in warnings[0].getMessage().lower()


async def test_exporter_recovers_after_an_outage():
    metrics = Metrics()
    sink: list = []
    state = {"down": True}

    def handler(request: httpx.Request) -> httpx.Response:
        if state["down"]:
            raise httpx.ConnectError("loki is down")
        sink.append(json.loads(request.content))
        return httpx.Response(204)

    exporter = LokiExporter(
        url=URL,
        enabled=True,
        batch_size=1,
        flush_interval=0.05,
        metrics=metrics,
        transport=httpx.MockTransport(handler),
    )
    await exporter.start()
    exporter.emit("e", {"n": 0})
    assert await wait_until(lambda: dropped(metrics) == 1.0)

    state["down"] = False
    exporter.emit("e", {"n": 1})
    assert await wait_until(lambda: sink)
    await exporter.close()

    assert json.loads(sink[0]["streams"][0]["values"][0][1])["n"] == 1
    assert dropped(metrics) == 1.0


async def test_http_error_status_counts_as_a_drop():
    metrics = Metrics()
    exporter = LokiExporter(
        url=URL,
        enabled=True,
        batch_size=10,
        flush_interval=0.05,
        metrics=metrics,
        transport=recording_transport([], status=500),
    )
    await exporter.start()
    exporter.emit("e", {"n": 1})
    exporter.emit("e", {"n": 2})
    assert await wait_until(lambda: dropped(metrics) >= 2.0)
    await exporter.close()
    assert dropped(metrics) == 2.0


async def test_close_flushes_what_is_queued():
    sink: list = []
    exporter = LokiExporter(
        url=URL,
        enabled=True,
        batch_size=1000,
        flush_interval=60.0,  # never fires on its own during this test
        transport=recording_transport(sink),
    )
    await exporter.start()
    exporter.emit("shutdown", {"n": 1})
    exporter.emit("shutdown", {"n": 2})
    assert sink == []

    await exporter.close()

    assert len(sink) == 1
    assert len(sink[0]["json"]["streams"][0]["values"]) == 2
    assert exporter.pending == 0


async def test_close_is_bounded_when_the_sink_hangs():
    metrics = Metrics()

    async def handler(request: httpx.Request) -> httpx.Response:
        await asyncio.sleep(30)
        return httpx.Response(204)

    exporter = LokiExporter(
        url=URL,
        enabled=True,
        batch_size=1,
        flush_interval=0.05,
        timeout=0.1,
        metrics=metrics,
        transport=httpx.MockTransport(handler),
    )
    await exporter.start()
    exporter.emit("e", {"n": 1})
    exporter.emit("e", {"n": 2})
    await asyncio.sleep(0.1)  # let the flusher get stuck mid-POST

    started = time.monotonic()
    await exporter.close()
    elapsed = time.monotonic() - started

    # Budget is 1s for the task plus 1s for the final drain, not 30s.
    assert elapsed < 5.0
    assert exporter.pending == 0
    assert dropped(metrics) == 2.0


async def test_close_is_safe_without_start_and_twice():
    exporter = LokiExporter(url=URL, enabled=True, transport=recording_transport([]))
    await exporter.close()
    await exporter.start()
    await exporter.close()
    await exporter.close()


class FakeLoki:
    """A minimal real HTTP server, to pin the bytes actually sent."""

    def __init__(self) -> None:
        self.requests: list[dict] = []
        self._server: asyncio.AbstractServer | None = None
        self._writers: list[asyncio.StreamWriter] = []

    async def start(self) -> int:
        self._server = await asyncio.start_server(self._handle, "127.0.0.1", 0)
        return self._server.sockets[0].getsockname()[1]

    async def _handle(self, reader: asyncio.StreamReader, writer: asyncio.StreamWriter) -> None:
        self._writers.append(writer)
        try:
            while True:
                head = await reader.readuntil(b"\r\n\r\n")
                lines = head.decode("latin-1").split("\r\n")
                method, path, _ = lines[0].split(" ")
                headers = {}
                for line in lines[1:]:
                    if ":" in line:
                        name, _, value = line.partition(":")
                        headers[name.strip().lower()] = value.strip()
                body = await reader.readexactly(int(headers.get("content-length", 0)))
                self.requests.append(
                    {
                        "method": method,
                        "path": path,
                        "content_type": headers.get("content-type"),
                        "json": json.loads(body) if body else None,
                    }
                )
                writer.write(b"HTTP/1.1 204 No Content\r\nContent-Length: 0\r\n\r\n")
                await writer.drain()
        except (asyncio.IncompleteReadError, ConnectionResetError, asyncio.CancelledError):
            pass
        finally:
            writer.close()

    async def close(self) -> None:
        for writer in self._writers:
            writer.close()
        if self._server is not None:
            self._server.close()
            await self._server.wait_closed()


async def test_against_a_real_http_server():
    sink = FakeLoki()
    port = await sink.start()
    exporter = LokiExporter(
        url=f"http://127.0.0.1:{port}",
        labels={"service": "mcp-switchboard"},
        enabled=True,
        batch_size=10,
        flush_interval=0.05,
    )
    await exporter.start()
    try:
        exporter.emit("tool_call", {"tool": "git_status", "ok": True}, {"label": "legion5"})
        assert await wait_until(lambda: sink.requests)
    finally:
        await exporter.close()
        await sink.close()

    request = sink.requests[0]
    assert request["method"] == "POST"
    assert request["path"] == "/loki/api/v1/push"
    assert request["content_type"] == "application/json"
    (stream,) = request["json"]["streams"]
    assert stream["stream"] == {
        "service": "mcp-switchboard",
        "event": "tool_call",
        "label": "legion5",
    }
    assert json.loads(stream["values"][0][1]) == {
        "event": "tool_call",
        "tool": "git_status",
        "ok": True,
    }


async def test_emit_never_raises_on_unserializable_fields():
    sink: list = []
    exporter = LokiExporter(
        url=URL, enabled=True, flush_interval=0.05, transport=recording_transport(sink)
    )
    await exporter.start()

    class Weird:
        def __repr__(self) -> str:
            return "<weird>"

    exporter.emit("e", {"obj": Weird(), "set": {1, 2}})
    assert await wait_until(lambda: sink)
    await exporter.close()

    line = json.loads(sink[0]["json"]["streams"][0]["values"][0][1])
    assert line["event"] == "e"
    assert line["obj"] == "<weird>"
