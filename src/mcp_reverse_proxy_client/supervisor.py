"""Runs one ServerTunnel per configured server concurrently, with clean shutdown."""

from __future__ import annotations

import asyncio
import logging
from typing import List

from .config import ServerSpec
from .proxy import ReconnectSettings, ServerTunnel

LOGGER = logging.getLogger("mcp_reverse_proxy_client.supervisor")


async def run_all(
    specs: List[ServerSpec],
    gateway_url: str,
    token: str,
    settings: ReconnectSettings,
    stop_event: asyncio.Event,
) -> None:
    tunnels = [ServerTunnel(spec, gateway_url, token, settings) for spec in specs]
    LOGGER.info("starting %d tunnel(s): %s", len(tunnels), ", ".join(s.name for s in specs))

    run_tasks = [asyncio.create_task(t.run_forever(), name=f"tunnel-{t.spec.name}") for t in tunnels]

    stop_task = asyncio.create_task(stop_event.wait())
    done, pending = await asyncio.wait(
        [*run_tasks, stop_task],
        return_when=asyncio.FIRST_COMPLETED,
    )

    LOGGER.info("shutting down all tunnels...")
    await asyncio.gather(*(t.shut_down() for t in tunnels), return_exceptions=True)

    for task in run_tasks:
        task.cancel()
    if not stop_task.done():
        stop_task.cancel()

    await asyncio.gather(*run_tasks, stop_task, return_exceptions=True)
    LOGGER.info("shutdown complete")
