"""HTTP API and web console for the hub.

Everything here is a thin shell over ``request.app.state.service``: this module
holds no state of its own, so it can be mounted by whatever composes the app
without an initialisation order to get wrong.

Two deliberate choices worth knowing about:

* A tool that answers with an MCP error is a **successful call that returned an
  error**: 200 with ``status: "error"`` in the body. Only an unknown
  connection/server/tool (``LookupError``) is a 404. Folding the two together
  would make the console report transport failures as tool bugs.
* The console is served from ``web/`` through ``importlib.resources``, not from
  a path relative to the source tree, so it works the same from a wheel, a Nix
  store path, or a git checkout.
"""

from __future__ import annotations

import asyncio
import json
import logging
import re
from contextlib import suppress
from importlib import resources
from typing import Any, Dict, Optional, Tuple

from fastapi import APIRouter, Depends, HTTPException, Request, Response
from fastapi.responses import HTMLResponse, StreamingResponse
from pydantic import BaseModel, Field

LOG = logging.getLogger(__name__)

#: How long the SSE stream may stay silent before it emits a comment. Proxies
#: and browsers drop idle connections well before a quiet hub would otherwise
#: say anything.
KEEPALIVE_SECONDS = 15.0

#: Events buffered per SSE client before the oldest are dropped. A reader that
#: falls this far behind has lost history either way; staying live matters more.
EVENT_QUEUE_SIZE = 1000

DEFAULT_CALL_LIMIT = 100
MAX_CALL_LIMIT = 1000

CALL_SOURCE_CONSOLE = "console"

_STREAM_END = object()

_SAFE_SEGMENT = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]*$")

_CONTENT_TYPES = {
    ".html": "text/html; charset=utf-8",
    ".js": "text/javascript; charset=utf-8",
    ".css": "text/css; charset=utf-8",
    ".json": "application/json",
    ".map": "application/json",
    ".svg": "image/svg+xml",
    ".png": "image/png",
    ".ico": "image/x-icon",
    ".woff2": "font/woff2",
    ".txt": "text/plain; charset=utf-8",
}


# --------------------------------------------------------------------------
# service access
# --------------------------------------------------------------------------


def get_service(request: Request) -> Any:
    """The live service object, or 503 while the app is still starting."""
    service = getattr(request.app.state, "service", None)
    if service is None:
        raise HTTPException(status_code=503, detail="hub service is not ready")
    return service


ServiceDep = Depends(get_service)


def record_json(record: Any) -> Any:
    """Normalise a CallRecord to its JSON form, passing dicts through."""
    to_json = getattr(record, "to_json", None)
    return to_json() if callable(to_json) else record


def _clean(value: Optional[str]) -> Optional[str]:
    """Treat an empty query parameter as an absent filter."""
    if value is None:
        return None
    value = value.strip()
    return value or None


# --------------------------------------------------------------------------
# static assets
# --------------------------------------------------------------------------


def web_root():
    """Traversable root of the bundled console."""
    return resources.files(__package__ or "mcp_switchboard_hub").joinpath("web")


def content_type_for(name: str) -> str:
    suffix = name[name.rfind(".") :].lower() if "." in name else ""
    return _CONTENT_TYPES.get(suffix, "application/octet-stream")


def read_web_asset(relative: str) -> Tuple[bytes, str]:
    """Read ``web/<relative>``; 404 on anything that is not a plain asset."""
    parts = [part for part in relative.split("/") if part]
    if not parts or not all(_SAFE_SEGMENT.match(part) for part in parts):
        raise HTTPException(status_code=404, detail="not found")
    node = web_root()
    for part in parts:
        node = node.joinpath(part)
    try:
        payload = node.read_bytes()
    except (FileNotFoundError, NotADirectoryError, IsADirectoryError, PermissionError, OSError):
        raise HTTPException(status_code=404, detail="not found") from None
    return payload, content_type_for(parts[-1])


# --------------------------------------------------------------------------
# SSE
# --------------------------------------------------------------------------


def sse_message(event: Any) -> str:
    payload = json.dumps(event, separators=(",", ":"), default=str)
    return f"data: {payload}\n\n"


async def _pump(source: Any, queue: "asyncio.Queue[Any]") -> None:
    """Drain the service subscription into a per-client queue."""
    try:
        async for event in source:
            if queue.full():
                with suppress(asyncio.QueueEmpty):
                    queue.get_nowait()
            queue.put_nowait(event)
    except asyncio.CancelledError:
        raise
    except Exception:  # a broken subscription ends the stream, not the process
        LOG.exception("event subscription failed")
    finally:
        with suppress(asyncio.QueueFull):
            queue.put_nowait(_STREAM_END)


async def event_stream(request: Request, service: Any):
    """Yield SSE frames from ``service.subscribe()`` until either side stops."""
    queue: "asyncio.Queue[Any]" = asyncio.Queue(maxsize=EVENT_QUEUE_SIZE)
    source = service.subscribe()
    pump = asyncio.ensure_future(_pump(source, queue))
    try:
        # Flush headers immediately so the browser reports the stream as open.
        yield ": connected\n\n"
        while True:
            try:
                event = await asyncio.wait_for(queue.get(), timeout=KEEPALIVE_SECONDS)
            except asyncio.TimeoutError:
                if await request.is_disconnected():
                    break
                yield ": keepalive\n\n"
                continue
            if event is _STREAM_END:
                break
            yield sse_message(event)
    finally:
        pump.cancel()
        with suppress(asyncio.CancelledError, Exception):
            await pump
        aclose = getattr(source, "aclose", None)
        if callable(aclose):
            with suppress(Exception):
                await aclose()


# --------------------------------------------------------------------------
# request bodies
# --------------------------------------------------------------------------


class ToolCallRequest(BaseModel):
    arguments: Dict[str, Any] = Field(default_factory=dict)


# --------------------------------------------------------------------------
# router
# --------------------------------------------------------------------------


def create_api_router() -> APIRouter:
    """Build the hub's HTTP surface: JSON API, metrics, and the web console."""
    router = APIRouter()

    @router.get("/api/connections")
    async def connections(service: Any = ServiceDep) -> Any:
        return service.snapshot()

    @router.post("/api/connections/{connection_id}/servers/{server}/tools/{tool}/call")
    async def call_tool(
        connection_id: str,
        server: str,
        tool: str,
        body: ToolCallRequest = ToolCallRequest(),
        service: Any = ServiceDep,
    ) -> Any:
        try:
            record = await service.execute_tool_call(
                connection_id=connection_id,
                server=server,
                tool=tool,
                arguments=body.arguments,
                source=CALL_SOURCE_CONSOLE,
            )
        except LookupError as exc:
            raise HTTPException(status_code=404, detail=str(exc) or "unknown tool") from exc
        return record_json(record)

    @router.post(
        "/api/connections/{connection_id}/servers/{server}/restart",
        status_code=204,
        response_class=Response,
    )
    async def restart_server(connection_id: str, server: str, service: Any = ServiceDep) -> Response:
        try:
            await service.restart_server(connection_id, server)
        except LookupError as exc:
            raise HTTPException(status_code=404, detail=str(exc) or "unknown server") from exc
        return Response(status_code=204)

    @router.get("/api/calls")
    async def list_calls(
        limit: int = DEFAULT_CALL_LIMIT,
        offset: int = 0,
        server: Optional[str] = None,
        tool: Optional[str] = None,
        status: Optional[str] = None,
        label: Optional[str] = None,
        service: Any = ServiceDep,
    ) -> Any:
        # Clamped rather than rejected: a console asking for too much should get
        # a page of results, not a validation error.
        limit = max(1, min(int(limit), MAX_CALL_LIMIT))
        offset = max(0, int(offset))
        records = await service.calls.list(
            limit=limit,
            offset=offset,
            server=_clean(server),
            tool=_clean(tool),
            status=_clean(status),
            label=_clean(label),
        )
        return {
            "calls": [record_json(record) for record in records],
            "stats": await service.calls.stats(),
            "limit": limit,
            "offset": offset,
        }

    @router.get("/api/calls/{call_id}")
    async def get_call(call_id: str, service: Any = ServiceDep) -> Any:
        record = await service.calls.get(call_id)
        if record is None:
            raise HTTPException(status_code=404, detail="unknown call")
        return record_json(record)

    @router.get("/api/events")
    async def events(request: Request, service: Any = ServiceDep) -> StreamingResponse:
        return StreamingResponse(
            event_stream(request, service),
            media_type="text/event-stream",
            headers={
                "Cache-Control": "no-cache, no-transform",
                "Connection": "keep-alive",
                # nginx buffers streamed responses unless told not to.
                "X-Accel-Buffering": "no",
            },
        )

    @router.get("/metrics")
    async def metrics(service: Any = ServiceDep) -> Response:
        rendered = service.metrics.render()
        if asyncio.iscoroutine(rendered):
            rendered = await rendered
        payload, content_type = rendered
        return Response(content=payload, media_type=content_type)

    @router.get("/", response_class=HTMLResponse, include_in_schema=False)
    async def console() -> HTMLResponse:
        payload, _ = read_web_asset("index.html")
        return HTMLResponse(content=payload.decode("utf-8"), headers={"Cache-Control": "no-cache"})

    @router.get("/static/{path:path}", include_in_schema=False)
    async def static_asset(path: str) -> Response:
        payload, content_type = read_web_asset(path)
        return Response(
            content=payload,
            media_type=content_type,
            headers={"Cache-Control": "no-cache"},
        )

    return router


__all__ = [
    "CALL_SOURCE_CONSOLE",
    "DEFAULT_CALL_LIMIT",
    "KEEPALIVE_SECONDS",
    "MAX_CALL_LIMIT",
    "ToolCallRequest",
    "create_api_router",
    "get_service",
    "read_web_asset",
    "record_json",
    "web_root",
]
