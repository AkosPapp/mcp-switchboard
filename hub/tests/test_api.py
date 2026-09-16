"""Tests for the hub's HTTP API and console.

Everything runs against a fake Service implementing the same contract the real
one does, so this file stays honest about the boundary: the API layer must not
need anything from the service beyond what is written down here.
"""

from __future__ import annotations

import asyncio
import json
from typing import Any, Dict, List, Optional

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

from mcp_switchboard_hub.api import (
    DEFAULT_CALL_LIMIT,
    MAX_CALL_LIMIT,
    create_api_router,
)

SNAPSHOT: Dict[str, Any] = {
    "connections": [
        {
            "id": "c1a2b3c4d5e6",
            "label": "legion5",
            "connectedAt": "2026-09-16T19:42:38+00:00",
            "client": {"name": "mcp-switchboard-client", "version": "0.2.0", "instance": "legion5-1"},
            "servers": [
                {
                    "name": "git",
                    "state": "running",
                    "error": None,
                    "toolCount": 2,
                    "tools": [
                        {
                            "name": "git_status",
                            "exposedName": "legion5__git__git_status",
                            "title": "git_status · git @ legion5",
                            "description": "Show the working tree status.",
                            "inputSchema": {
                                "type": "object",
                                "properties": {"repo_path": {"type": "string"}},
                                "required": ["repo_path"],
                            },
                        },
                        {
                            "name": "git_log",
                            "exposedName": "legion5__git__git_log",
                            "title": "git_log · git @ legion5",
                            "description": "Show commits.",
                            "inputSchema": {"type": "object", "properties": {}},
                        },
                    ],
                },
                {
                    "name": "fs",
                    "state": "failed",
                    "error": "exited with code 1",
                    "toolCount": 0,
                    "tools": [],
                },
            ],
        }
    ]
}


class FakeRecord:
    """Stands in for CallRecord: the API only ever asks for ``to_json()``."""

    def __init__(self, **fields: Any) -> None:
        self.fields = fields

    def to_json(self) -> Dict[str, Any]:
        return dict(self.fields)


def make_record(
    call_id: str = "call-1",
    tool: str = "git_status",
    status: str = "ok",
    **overrides: Any,
) -> FakeRecord:
    fields: Dict[str, Any] = {
        "id": call_id,
        "connectionId": "c1a2b3c4d5e6",
        "label": "legion5",
        "server": "git",
        "tool": tool,
        "exposedName": "legion5__git__" + tool,
        "arguments": {"repo_path": "/srv/app"},
        "result": {"content": [{"type": "text", "text": "clean"}]} if status == "ok" else None,
        "error": None if status == "ok" else "fatal: not a git repository",
        "status": status,
        "source": "console",
        "startedAt": "2026-09-16T19:44:00+00:00",
        "durationMs": 12,
    }
    fields.update(overrides)
    return FakeRecord(**fields)


class FakeCallStore:
    def __init__(self, records: Optional[List[FakeRecord]] = None) -> None:
        self.records = records or []
        self.queries: List[Dict[str, Any]] = []

    async def list(
        self,
        *,
        limit: int,
        offset: int = 0,
        server: Optional[str] = None,
        tool: Optional[str] = None,
        status: Optional[str] = None,
        label: Optional[str] = None,
    ) -> List[FakeRecord]:
        self.queries.append(
            {
                "limit": limit,
                "offset": offset,
                "server": server,
                "tool": tool,
                "status": status,
                "label": label,
            }
        )
        rows = [r.to_json() for r in self.records]
        for field, wanted in (("server", server), ("tool", tool), ("status", status), ("label", label)):
            if wanted:
                rows = [row for row in rows if row.get(field) == wanted]
        return [FakeRecord(**row) for row in rows[offset : offset + limit]]

    async def get(self, call_id: str) -> Optional[FakeRecord]:
        for record in self.records:
            if record.fields["id"] == call_id:
                return record
        return None

    async def stats(self) -> Dict[str, Any]:
        return {
            "total": len(self.records),
            "ok": sum(1 for r in self.records if r.fields["status"] == "ok"),
            "error": sum(1 for r in self.records if r.fields["status"] != "ok"),
        }


class FakeMetrics:
    payload = b"# HELP switchboard_calls_total Calls.\nswitchboard_calls_total 3\n"
    content_type = "text/plain; version=0.0.4; charset=utf-8"

    def render(self):
        return self.payload, self.content_type


class FakeService:
    """The contract the API codes against, and nothing more."""

    def __init__(self, records: Optional[List[FakeRecord]] = None, events=None) -> None:
        self.calls = FakeCallStore(records)
        self.metrics = FakeMetrics()
        self.executed: List[Dict[str, Any]] = []
        self.restarts: List[Dict[str, str]] = []
        self.snapshot_calls = 0
        self._events = events

    def snapshot(self) -> Dict[str, Any]:
        self.snapshot_calls += 1
        return SNAPSHOT

    async def execute_tool_call(
        self, *, connection_id: str, server: str, tool: str, arguments: dict, source: str
    ) -> FakeRecord:
        self.executed.append(
            {
                "connection_id": connection_id,
                "server": server,
                "tool": tool,
                "arguments": arguments,
                "source": source,
            }
        )
        if tool == "nope":
            raise LookupError("unknown tool 'nope' on git@legion5")
        if tool == "git_log":
            return make_record(call_id="call-err", tool=tool, status="error", arguments=arguments)
        return make_record(call_id="call-ok", tool=tool, status="ok", arguments=arguments)

    async def restart_server(self, connection_id: str, server: str) -> None:
        self.restarts.append({"connection_id": connection_id, "server": server})

    def subscribe(self):
        if self._events is not None:
            return self._events()

        async def stream():
            yield {"type": "connections"}
            yield {"type": "call", "call": make_record().to_json()}

        return stream()


def build_client(service: FakeService) -> TestClient:
    app = FastAPI()
    app.state.service = service
    app.include_router(create_api_router())
    return TestClient(app)


@pytest.fixture()
def service() -> FakeService:
    return FakeService(
        records=[
            make_record(call_id="call-1", tool="git_status", status="ok"),
            make_record(call_id="call-2", tool="git_log", status="error"),
        ]
    )


@pytest.fixture()
def client(service: FakeService) -> TestClient:
    with build_client(service) as test_client:
        yield test_client


# ---------------------------------------------------------------- snapshot


def test_connections_passes_the_snapshot_through(client, service):
    response = client.get("/api/connections")
    assert response.status_code == 200
    assert response.json() == SNAPSHOT
    assert service.snapshot_calls == 1


def test_missing_service_is_503():
    app = FastAPI()
    app.include_router(create_api_router())
    with TestClient(app) as bare:
        assert bare.get("/api/connections").status_code == 503


# ------------------------------------------------------------ manual calls


def test_manual_call_returns_the_record(client, service):
    response = client.post(
        "/api/connections/c1a2b3c4d5e6/servers/git/tools/git_status/call",
        json={"arguments": {"repo_path": "/srv/app"}},
    )
    assert response.status_code == 200
    body = response.json()
    assert body["status"] == "ok"
    assert body["exposedName"] == "legion5__git__git_status"
    assert body["result"]["content"][0]["text"] == "clean"
    assert service.executed == [
        {
            "connection_id": "c1a2b3c4d5e6",
            "server": "git",
            "tool": "git_status",
            "arguments": {"repo_path": "/srv/app"},
            "source": "console",
        }
    ]


def test_call_without_a_body_sends_empty_arguments(client, service):
    response = client.post("/api/connections/c1a2b3c4d5e6/servers/git/tools/git_status/call")
    assert response.status_code == 200
    assert service.executed[0]["arguments"] == {}


def test_tool_error_is_still_a_200(client):
    """An MCP error is a successful call that returned an error, not a failure."""
    response = client.post(
        "/api/connections/c1a2b3c4d5e6/servers/git/tools/git_log/call",
        json={"arguments": {}},
    )
    assert response.status_code == 200
    body = response.json()
    assert body["status"] == "error"
    assert body["error"] == "fatal: not a git repository"
    assert body["result"] is None


def test_unknown_tool_is_404(client):
    response = client.post(
        "/api/connections/c1a2b3c4d5e6/servers/git/tools/nope/call",
        json={"arguments": {}},
    )
    assert response.status_code == 404
    assert "nope" in response.json()["detail"]


def test_restart_returns_204(client, service):
    response = client.post("/api/connections/c1a2b3c4d5e6/servers/git/restart")
    assert response.status_code == 204
    assert response.content == b""
    assert service.restarts == [{"connection_id": "c1a2b3c4d5e6", "server": "git"}]


# ------------------------------------------------------------------ calls


def test_calls_listing_returns_records_and_stats(client):
    body = client.get("/api/calls").json()
    assert [call["id"] for call in body["calls"]] == ["call-1", "call-2"]
    assert body["stats"] == {"total": 2, "ok": 1, "error": 1}
    assert body["limit"] == DEFAULT_CALL_LIMIT


def test_calls_listing_applies_filters(client, service):
    body = client.get("/api/calls", params={"status": "error", "tool": "git_log"}).json()
    assert [call["id"] for call in body["calls"]] == ["call-2"]
    assert service.calls.queries[-1]["status"] == "error"
    assert service.calls.queries[-1]["tool"] == "git_log"
    assert service.calls.queries[-1]["server"] is None


def test_blank_filters_are_dropped(client, service):
    client.get("/api/calls", params={"server": "  ", "label": ""})
    assert service.calls.queries[-1]["server"] is None
    assert service.calls.queries[-1]["label"] is None


def test_limit_is_capped_and_offset_floored(client, service):
    client.get("/api/calls", params={"limit": 5000, "offset": -3})
    assert service.calls.queries[-1]["limit"] == MAX_CALL_LIMIT
    assert service.calls.queries[-1]["offset"] == 0
    client.get("/api/calls", params={"limit": 0})
    assert service.calls.queries[-1]["limit"] == 1


def test_offset_paginates(client):
    body = client.get("/api/calls", params={"limit": 1, "offset": 1}).json()
    assert [call["id"] for call in body["calls"]] == ["call-2"]


def test_single_call_lookup(client):
    assert client.get("/api/calls/call-1").json()["id"] == "call-1"
    assert client.get("/api/calls/missing").status_code == 404


# ---------------------------------------------------------------- metrics


def test_metrics_passthrough(client):
    response = client.get("/metrics")
    assert response.status_code == 200
    assert response.content == FakeMetrics.payload
    assert response.headers["content-type"] == FakeMetrics.content_type


# ------------------------------------------------------------------- SSE


def _data_frames(response) -> List[Dict[str, Any]]:
    frames = []
    for line in response.iter_lines():
        if line.startswith("data:"):
            frames.append(json.loads(line[len("data:") :].strip()))
    return frames


def test_events_streams_and_terminates(client):
    with client.stream("GET", "/api/events") as response:
        assert response.status_code == 200
        assert response.headers["content-type"].startswith("text/event-stream")
        assert response.headers["cache-control"].startswith("no-cache")
        frames = _data_frames(response)  # returns once the source is exhausted
    assert frames[0] == {"type": "connections"}
    assert frames[1]["type"] == "call"
    assert frames[1]["call"]["tool"] == "git_status"


def test_events_sends_keepalives_while_idle(monkeypatch):
    from mcp_switchboard_hub import api as api_module

    monkeypatch.setattr(api_module, "KEEPALIVE_SECONDS", 0.02)

    def events():
        async def stream():
            await asyncio.sleep(0.12)
            yield {"type": "connections"}

        return stream()

    with build_client(FakeService(events=events)) as idle_client:
        with idle_client.stream("GET", "/api/events") as response:
            body = "".join(response.iter_text())
    assert ": keepalive" in body
    assert 'data: {"type":"connections"}' in body


def test_broken_subscription_ends_the_stream(client):
    def events():
        async def stream():
            yield {"type": "connections"}
            raise RuntimeError("subscription died")

        return stream()

    with build_client(FakeService(events=events)) as broken:
        with broken.stream("GET", "/api/events") as response:
            frames = _data_frames(response)
    assert frames == [{"type": "connections"}]


# ------------------------------------------------------------------ console


def test_root_serves_the_console(client):
    response = client.get("/")
    assert response.status_code == 200
    assert response.headers["content-type"].startswith("text/html")
    assert "<!doctype html>" in response.text.lower()
    assert "static/app.js" in response.text


def test_static_assets_are_served_with_types(client):
    css = client.get("/static/style.css")
    assert css.status_code == 200
    assert css.headers["content-type"].startswith("text/css")
    js = client.get("/static/app.js")
    assert js.status_code == 200
    assert js.headers["content-type"].startswith("text/javascript")
    assert client.get("/static/favicon.svg").headers["content-type"] == "image/svg+xml"


@pytest.mark.parametrize("path", ["/static/nope.js", "/static/%2e%2e/api.py", "/static/.env", "/static/"])
def test_unknown_or_unsafe_assets_are_404(client, path):
    assert client.get(path).status_code == 404
