"""Per-server outbound tunnel: local stdio MCP subprocess <-> gateway WebSocket.

Wire-compatible with Context Forge's own ``mcpgateway.reverse_proxy`` client:
same ``/reverse-proxy/ws`` endpoint, same envelope shape
(``{"type": ..., "sessionId": ..., ...}`` with register/request/response/
notification/heartbeat/unregister/error message types), same
``Authorization: Bearer`` + ``X-Session-ID`` headers, and the same
reconnect/backoff shape (initial delay doubling each attempt, capped at 60s).
"""

from __future__ import annotations

import asyncio
import json
import logging
import os
import uuid
from contextlib import suppress
from dataclasses import dataclass
from enum import Enum
from typing import Any, Dict, List, Optional
from urllib.parse import urljoin

import websockets
from websockets.exceptions import ConnectionClosed

from .config import ServerSpec

DEFAULT_RECONNECT_DELAY = 1.0
DEFAULT_MAX_RETRIES = 0  # 0 = infinite
DEFAULT_KEEPALIVE_INTERVAL = 30
MAX_BACKOFF_DELAY = 60.0


class MessageType(str, Enum):
    REGISTER = "register"
    UNREGISTER = "unregister"
    HEARTBEAT = "heartbeat"
    ERROR = "error"
    REQUEST = "request"
    RESPONSE = "response"
    NOTIFICATION = "notification"


@dataclass
class ReconnectSettings:
    reconnect_delay: float = DEFAULT_RECONNECT_DELAY
    max_retries: int = DEFAULT_MAX_RETRIES
    keepalive_interval: float = DEFAULT_KEEPALIVE_INTERVAL


def _gateway_ws_url(gateway_url: str) -> str:
    ws_url = gateway_url.replace("http://", "ws://").replace("https://", "wss://")
    if not ws_url.startswith(("ws://", "wss://")):
        ws_url = f"wss://{ws_url}"
    if "/reverse-proxy" not in ws_url:
        ws_url = urljoin(ws_url if ws_url.endswith("/") else ws_url + "/", "reverse-proxy/ws")
    return ws_url


class ServerTunnel:
    """Owns one local subprocess and one persistent outbound tunnel to the gateway."""

    def __init__(
        self,
        spec: ServerSpec,
        gateway_url: str,
        token: Optional[str],
        settings: ReconnectSettings,
    ) -> None:
        self.spec = spec
        self.gateway_url = gateway_url
        self.token = token
        self.settings = settings
        self.session_id = uuid.uuid4().hex
        self.log = logging.getLogger(f"mcp_reverse_proxy_client.{spec.name}")

        self._process: Optional[asyncio.subprocess.Process] = None
        self._stdout_task: Optional[asyncio.Task] = None
        self._connection = None
        self._connected = asyncio.Event()
        self._shutting_down = False
        self._retry_count = 0

    async def run_forever(self) -> None:
        """Run with auto-reconnect until shut_down() is called."""
        while not self._shutting_down:
            try:
                await self._connect_and_serve()
            except asyncio.CancelledError:
                raise
            except Exception as e:  # noqa: BLE001 - one tunnel's errors must not kill others
                self.log.error("connection error: %s", e)

            if self._shutting_down:
                break

            self._retry_count += 1
            if self.settings.max_retries > 0 and self._retry_count >= self.settings.max_retries:
                self.log.error("max retries (%d) exceeded, giving up", self.settings.max_retries)
                break

            delay = min(self.settings.reconnect_delay * (2 ** self._retry_count), MAX_BACKOFF_DELAY)
            self.log.info("reconnecting in %.1fs (attempt %d)", delay, self._retry_count)
            await asyncio.sleep(delay)

    async def shut_down(self) -> None:
        self._shutting_down = True
        if self._connection is not None:
            try:
                await self._send_to_gateway({"type": MessageType.UNREGISTER.value, "sessionId": self.session_id})
            except Exception:  # noqa: BLE001 - best-effort during shutdown
                pass
            try:
                await self._connection.close()
            except Exception:  # noqa: BLE001
                pass
        await self._stop_process()

    async def _connect_and_serve(self) -> None:
        await self._start_process()

        ws_url = _gateway_ws_url(self.gateway_url)
        headers = {"X-Session-ID": self.session_id}
        if self.token:
            headers["Authorization"] = f"Bearer {self.token}"

        self.log.info("connecting to gateway: %s", ws_url)
        async with websockets.connect(
            ws_url,
            additional_headers=headers,
            ping_interval=20,
            ping_timeout=10,
        ) as connection:
            self._connection = connection
            self._retry_count = 0
            self.log.info("tunnel up (session %s)", self.session_id[:8])

            await self._send_to_gateway(
                {
                    "type": MessageType.REGISTER.value,
                    "sessionId": self.session_id,
                    "server": {
                        "name": f"{self.spec.name}-{self.session_id[:8]}",
                        "description": f"Reverse proxied: {self.spec.name}",
                        "protocol": "stdio",
                    },
                }
            )

            keepalive_task = asyncio.create_task(self._keepalive_loop())
            try:
                async for message in connection:
                    await self._handle_gateway_message(message)
            except ConnectionClosed:
                self.log.warning("gateway connection closed")
            finally:
                keepalive_task.cancel()
                with suppress(asyncio.CancelledError):
                    await keepalive_task
                self._connection = None
                self.log.info("tunnel down")

        await self._stop_process()

    async def _keepalive_loop(self) -> None:
        while True:
            await asyncio.sleep(self.settings.keepalive_interval)
            try:
                await self._send_to_gateway({"type": MessageType.HEARTBEAT.value, "sessionId": self.session_id})
            except Exception as e:  # noqa: BLE001
                self.log.warning("keepalive failed: %s", e)
                return

    async def _send_to_gateway(self, envelope: Dict[str, Any]) -> None:
        if self._connection is None:
            raise RuntimeError("not connected to gateway")
        await self._connection.send(json.dumps(envelope))

    async def _handle_gateway_message(self, raw: str) -> None:
        try:
            data = json.loads(raw)
        except json.JSONDecodeError:
            self.log.error("received non-JSON message from gateway, dropping")
            return

        msg_type = data.get("type")
        if msg_type == MessageType.REQUEST.value:
            payload = data.get("payload", {})
            await self._send_to_process(json.dumps(payload))
        elif msg_type == MessageType.HEARTBEAT.value:
            await self._send_to_gateway({"type": MessageType.HEARTBEAT.value, "sessionId": self.session_id})
        elif msg_type == MessageType.ERROR.value:
            self.log.error("gateway error: %s", data.get("message", "unknown error"))
        else:
            self.log.warning("unknown message type from gateway: %r", msg_type)

    # -- local subprocess management -----------------------------------

    async def _start_process(self) -> None:
        if self._process is not None:
            return

        env = os.environ.copy()
        env.update(self.spec.env)

        self.log.info("starting local server: %s", " ".join(self.spec.argv))
        self._process = await asyncio.create_subprocess_exec(
            *self.spec.argv,
            stdin=asyncio.subprocess.PIPE,
            stdout=asyncio.subprocess.PIPE,
            stderr=None,  # inherit, so local server logs show up on our stderr
            env=env,
            cwd=self.spec.cwd,
        )
        self._stdout_task = asyncio.create_task(self._read_stdout())
        self.log.info("local server started (pid %d)", self._process.pid)

    async def _stop_process(self) -> None:
        if self._process is None:
            return

        if self._stdout_task is not None:
            self._stdout_task.cancel()
            with suppress(asyncio.CancelledError):
                await self._stdout_task
            self._stdout_task = None

        process = self._process
        self._process = None

        if process.returncode is None:
            process.terminate()
            try:
                await asyncio.wait_for(process.wait(), timeout=5)
            except asyncio.TimeoutError:
                self.log.warning("local server did not exit, killing")
                process.kill()
                await process.wait()

        if self.spec.fastmcp_tempfile is not None:
            self.spec.fastmcp_tempfile.unlink(missing_ok=True)

    async def _send_to_process(self, message: str) -> None:
        if self._process is None or self._process.stdin is None:
            self.log.warning("local server not running, dropping request")
            return
        self._process.stdin.write((message + "\n").encode())
        await self._process.stdin.drain()

    async def _read_stdout(self) -> None:
        assert self._process is not None and self._process.stdout is not None
        try:
            while True:
                line = await self._process.stdout.readline()
                if not line:
                    self.log.warning("local server closed stdout")
                    break
                text = line.decode(errors="replace").strip()
                if not text:
                    continue
                await self._handle_process_message(text)
        except asyncio.CancelledError:
            raise
        except Exception as e:  # noqa: BLE001
            self.log.error("error reading local server stdout: %s", e)

    async def _handle_process_message(self, message: str) -> None:
        try:
            data = json.loads(message)
        except json.JSONDecodeError:
            self.log.error("local server produced non-JSON line, dropping: %s", message[:200])
            return

        envelope = {
            "type": (MessageType.RESPONSE.value if "id" in data else MessageType.NOTIFICATION.value),
            "sessionId": self.session_id,
            "payload": data,
        }
        try:
            await self._send_to_gateway(envelope)
        except Exception as e:  # noqa: BLE001
            self.log.warning("dropping message, gateway not connected: %s", e)

