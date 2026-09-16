"""Builds the two listeners.

They are separate apps on separate ports for a deliberate reason: only the tunnel
endpoint is meant to be reachable from the internet, and it always requires a
token. The console, the API, the MCP endpoint and /metrics live on a second
listener that binds loopback, so they are not protected by a password - they are
simply not reachable. That is a stronger guarantee than a shared secret, and it
is why the private listener is unauthenticated by default.
"""

from __future__ import annotations

import contextlib
import logging
from dataclasses import dataclass
from typing import AsyncIterator

from fastapi import FastAPI, Request, WebSocket
from fastapi.responses import JSONResponse

from . import protocol
from .api import create_api_router
from .calls import CallStore
from .config import Settings
from .loki import LokiExporter
from .mcp_endpoint import McpEndpoint
from .metrics import Metrics
from .registry import Registry
from .service import Service
from .tunnel import TunnelHandler

LOGGER = logging.getLogger("mcp_switchboard_hub.app")


@dataclass
class Hub:
    settings: Settings
    service: Service
    mcp: McpEndpoint
    tunnel_app: FastAPI
    private_app: FastAPI

    @contextlib.asynccontextmanager
    async def lifespan(self) -> AsyncIterator["Hub"]:
        """Own everything process-wide, rather than per-app.

        The MCP session manager's run() must be entered exactly once, and both
        apps share one Service, so the lifecycle belongs here and not in either
        app's own lifespan.
        """
        async with contextlib.AsyncExitStack() as stack:
            await self.service.start()
            stack.push_async_callback(self.service.close)
            await stack.enter_async_context(self.mcp.run())
            yield self


def build_hub(settings: Settings) -> Hub:
    registry = Registry()
    metrics = Metrics()
    calls = CallStore(settings.db_path, retention_days=settings.retention_days, max_rows=settings.max_rows)
    loki = LokiExporter(
        url=settings.loki_url,
        labels=settings.loki_labels,
        enabled=settings.loki_enabled,
        metrics=metrics,
    )

    service = Service(settings, registry, calls, metrics, loki)
    mcp = McpEndpoint(registry, service)
    service.mcp = mcp

    handler = TunnelHandler(
        registry,
        token=settings.tunnel_token,
        tools_timeout=settings.tools_timeout,
        on_change=service.on_topology_change,
        metrics=metrics,
    )

    tunnel_app = FastAPI(title="mcp-switchboard tunnel", docs_url=None, redoc_url=None)

    @tunnel_app.websocket(protocol.TUNNEL_PATH)
    async def tunnel_endpoint(websocket: WebSocket) -> None:  # pragma: no cover - exercised end to end
        await handler.handle(websocket)

    @tunnel_app.get("/health")
    async def tunnel_health() -> dict:
        return {"status": "ok"}

    private_app = FastAPI(title="mcp-switchboard", docs_url=None, redoc_url=None)
    private_app.state.service = service

    if settings.private_token:
        _require_token(private_app, settings.private_token)

    private_app.include_router(create_api_router())
    for route in mcp.routes():
        private_app.router.routes.append(route)

    @private_app.get("/health")
    async def private_health() -> dict:
        return {"status": "ok"}

    return Hub(settings=settings, service=service, mcp=mcp, tunnel_app=tunnel_app, private_app=private_app)


def _require_token(app: FastAPI, token: str) -> None:
    """Optional bearer auth for the private listener, for when it is exposed anyway."""
    import hmac

    @app.middleware("http")
    async def check_token(request: Request, call_next):
        if request.url.path == "/health":
            return await call_next(request)
        header = request.headers.get("authorization", "")
        supplied = header[len("Bearer ") :].strip() if header.startswith("Bearer ") else ""
        if not hmac.compare_digest(supplied, token):
            return JSONResponse({"detail": "unauthorized"}, status_code=401)
        return await call_next(request)
