"""The acceptance test: a real client, a real MCP server, a real consumer.

Nothing here is faked. A separate `mcp-switchboard-client` process spawns a real
stdio MCP server and tunnels it to a real hub over a real WebSocket; then the
tool is called twice, once the way the web console calls it and once the way n8n
does (an MCP client speaking Streamable HTTP). Both must work, and both must land
in the call log and the metrics.

This is precisely what the gateway this project replaces could never do: there,
responses from a tunneled server were received and dropped.
"""

from __future__ import annotations

import asyncio
import json
import os
import socket
import subprocess
import sys
from contextlib import closing
from pathlib import Path

import httpx
import pytest
import uvicorn

REPO = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(REPO / "hub" / "src"))
sys.path.insert(0, str(REPO / "client" / "src"))

from mcp import ClientSession  # noqa: E402
from mcp.client.streamable_http import streamable_http_client  # noqa: E402

from mcp_switchboard_hub.app import build_hub  # noqa: E402
from mcp_switchboard_hub.config import Settings  # noqa: E402

TOKEN = "e2e-test-token"
LABEL = "testbox"


def free_port() -> int:
    with closing(socket.socket()) as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


async def wait_for(predicate, timeout: float = 30.0, interval: float = 0.2):
    """Poll until predicate returns something truthy, else fail with what it last saw."""
    loop = asyncio.get_running_loop()
    deadline = loop.time() + timeout
    last = None
    while loop.time() < deadline:
        last = await predicate()
        if last:
            return last
        await asyncio.sleep(interval)
    raise AssertionError(f"timed out after {timeout}s; last saw: {last!r}")


@pytest.fixture
async def running_hub(tmp_path):
    settings = Settings(
        tunnel_host="127.0.0.1",
        tunnel_port=free_port(),
        tunnel_token=TOKEN,
        private_host="127.0.0.1",
        private_port=free_port(),
        data_dir=tmp_path / "state",
        log_level="WARNING",
        tools_timeout=20.0,
    )
    hub = build_hub(settings)

    tunnel = uvicorn.Server(
        uvicorn.Config(hub.tunnel_app, host=settings.tunnel_host, port=settings.tunnel_port,
                       log_level="warning")
    )
    private = uvicorn.Server(
        uvicorn.Config(hub.private_app, host=settings.private_host, port=settings.private_port,
                       log_level="warning")
    )
    for server in (tunnel, private):
        server.install_signal_handlers = lambda: None

    async with hub.lifespan():
        tasks = [asyncio.create_task(s.serve()) for s in (tunnel, private)]
        await wait_for(lambda: asyncio.sleep(0, result=tunnel.started and private.started))
        try:
            yield hub, settings
        finally:
            for s in (tunnel, private):
                s.should_exit = True
            await asyncio.gather(*tasks, return_exceptions=True)


@pytest.fixture
def client_process(tmp_path, running_hub):
    _, settings = running_hub
    config = {
        "mcpServers": {
            "demo": {
                "command": sys.executable,
                "args": [str(REPO / "tests" / "fake_mcp_server.py"), "demo"],
            }
        }
    }
    config_path = tmp_path / "mcp.json"
    config_path.write_text(json.dumps(config))

    env = dict(os.environ)
    env["PYTHONPATH"] = os.pathsep.join([str(REPO / "client" / "src"), str(REPO / "hub" / "src")])
    env["PYTHONUNBUFFERED"] = "1"

    proc = subprocess.Popen(
        [
            sys.executable, "-m", "mcp_switchboard_client",
            "--hub-url", f"ws://127.0.0.1:{settings.tunnel_port}",
            "--token", TOKEN,
            "--label", LABEL,
            "--config", str(config_path),
            "--log-level", "INFO",
        ],
        cwd=tmp_path,
        env=env,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
    )
    try:
        yield proc
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            proc.kill()


@pytest.mark.anyio
async def test_tool_reaches_both_the_console_and_an_mcp_consumer(running_hub, client_process):
    hub, settings = running_hub
    base = f"http://127.0.0.1:{settings.private_port}"

    async with httpx.AsyncClient(base_url=base, timeout=20.0) as http:

        async def tools_are_up():
            snapshot = (await http.get("/api/connections")).json()
            for connection in snapshot["connections"]:
                for server in connection["servers"]:
                    if server["state"] == "running" and server["tools"]:
                        return connection, server
            return None

        if client_process.poll() is not None:
            pytest.fail(f"client exited early:\n{client_process.stdout.read()}")

        connection, server = await wait_for(tools_are_up)

        # The console must show exactly what a consumer will see, including the
        # host, since that is how you tell two machines' tools apart.
        assert connection["label"] == LABEL
        assert server["name"] == "demo"
        names = {t["name"]: t["exposedName"] for t in server["tools"]}
        assert names["echo"] == f"{LABEL}__demo__echo"

        # --- path 1: the way the web console calls a tool ---
        response = await http.post(
            f"/api/connections/{connection['id']}/servers/demo/tools/echo/call",
            json={"arguments": {"message": "hello"}},
        )
        assert response.status_code == 200, response.text
        record = response.json()
        assert record["status"] == "ok", record
        assert "echo: hello" in json.dumps(record["result"])
        assert record["source"] == "console"

        # A tool that reports an error is still a successful call, and must not
        # be conflated with a transport failure.
        failed = await http.post(
            f"/api/connections/{connection['id']}/servers/demo/tools/boom/call",
            json={"arguments": {}},
        )
        assert failed.status_code == 200
        assert failed.json()["status"] == "error"

        # --- path 2: the way n8n calls a tool ---
        async with streamable_http_client(f"{base}/mcp") as (read, write):
            async with ClientSession(read, write) as session:
                await session.initialize()
                listed = await session.list_tools()
                exposed = {t.name for t in listed.tools}
                assert f"{LABEL}__demo__echo" in exposed

                tool = next(t for t in listed.tools if t.name == f"{LABEL}__demo__echo")
                # Origin is tagged where a human and an LLM will each look.
                assert LABEL in (tool.title or "")
                assert f"[{LABEL} · demo]" in (tool.description or "")

                result = await session.call_tool(f"{LABEL}__demo__echo", {"message": "from-mcp"})
                assert not result.is_error
                assert "echo: from-mcp" in result.content[0].text

        # --- the scoped endpoint drops the host prefix ---
        async with streamable_http_client(f"{base}/mcp/host/{LABEL}") as (read, write):
            async with ClientSession(read, write) as session:
                await session.initialize()
                scoped = {t.name for t in (await session.list_tools()).tools}
                assert "demo__echo" in scoped
                assert f"{LABEL}__demo__echo" not in scoped

        # --- everything was logged, from both sources ---
        calls = (await http.get("/api/calls")).json()["calls"]
        sources = {c["source"] for c in calls}
        assert sources == {"console", "mcp"}, calls
        assert any(c["tool"] == "echo" and c["status"] == "ok" for c in calls)
        assert any(c["tool"] == "boom" and c["status"] == "error" for c in calls)
        assert all(c["label"] == LABEL for c in calls)

        detail = (await http.get(f"/api/calls/{calls[0]['id']}")).json()
        assert detail["id"] == calls[0]["id"]

        # --- and measured ---
        metrics = (await http.get("/metrics")).text
        assert "mcpsb_tool_calls_total" in metrics
        assert f'label="{LABEL}"' in metrics
        assert "mcpsb_connections_active 1.0" in metrics


@pytest.mark.anyio
async def test_tunnel_rejects_a_bad_token(running_hub):
    import websockets

    _, settings = running_hub
    url = f"ws://127.0.0.1:{settings.tunnel_port}/tunnel/v1"

    with pytest.raises(Exception) as excinfo:
        async with websockets.connect(url, additional_headers={"Authorization": "Bearer wrong"}):
            pass
    assert "403" in str(excinfo.value) or "401" in str(excinfo.value) or "rejected" in str(excinfo.value).lower()
