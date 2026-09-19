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
            },
            "lsp": {
                "command": sys.executable,
                "args": [str(REPO / "tests" / "fake_mcp_server.py"), "lsp"],
                "project": "nix",
            },
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

        async def both_servers_up():
            snapshot = (await http.get("/api/connections")).json()
            for connection in snapshot["connections"]:
                ready = {s["name"]: s for s in connection["servers"] if s["state"] == "running" and s["tools"]}
                if "demo" in ready and "lsp" in ready and "harness" in ready:
                    return connection, ready
            return None

        if client_process.poll() is not None:
            pytest.fail(f"client exited early:\n{client_process.stdout.read()}")

        connection, ready = await wait_for(both_servers_up)
        server = ready["demo"]
        lsp = ready["lsp"]

        # The console must show exactly what a consumer will see, including the
        # host (and the project, for a server that has one), since that is how
        # you tell tools on different machines/projects apart.
        assert connection["label"] == LABEL
        assert server["name"] == "demo"
        assert server["project"] is None
        names = {t["name"]: t["exposedName"] for t in server["tools"]}
        assert names["echo"] == f"{LABEL}__demo__echo"

        assert lsp["project"] == "nix"
        lsp_names = {t["name"]: t["exposedName"] for t in lsp["tools"]}
        assert lsp_names["echo"] == f"{LABEL}__nix__lsp__echo"

        # The built-in coding harness is added by default, with no mcp.json entry.
        harness_tools = {t["name"] for t in ready["harness"]["tools"]}
        assert {"run_command", "file_read", "git_diff", "process_start"} <= harness_tools

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
                assert f"{LABEL}__nix__lsp__echo" in exposed

                tool = next(t for t in listed.tools if t.name == f"{LABEL}__demo__echo")
                # Origin is tagged where a human and an LLM will each look.
                assert LABEL in (tool.title or "")
                assert f"[{LABEL} · demo]" in (tool.description or "")

                lsp_tool = next(t for t in listed.tools if t.name == f"{LABEL}__nix__lsp__echo")
                assert "nix" in (lsp_tool.title or "")
                assert f"[{LABEL} · nix · lsp]" in (lsp_tool.description or "")

                result = await session.call_tool(f"{LABEL}__demo__echo", {"message": "from-mcp"})
                assert not result.is_error
                assert "echo: from-mcp" in result.content[0].text

        # --- host scope: drops the label, keeps the project ---
        async with streamable_http_client(f"{base}/mcp/host/{LABEL}") as (read, write):
            async with ClientSession(read, write) as session:
                await session.initialize()
                scoped = {t.name for t in (await session.list_tools()).tools}
                assert "demo__echo" in scoped
                assert "nix__lsp__echo" in scoped
                assert f"{LABEL}__demo__echo" not in scoped

        # --- project scope: only that project's servers, project dropped from the name ---
        async with streamable_http_client(f"{base}/mcp/host/{LABEL}/project/nix") as (read, write):
            async with ClientSession(read, write) as session:
                await session.initialize()
                scoped = {t.name for t in (await session.list_tools()).tools}
                assert scoped == {"lsp__echo", "lsp__boom"}

        # --- project+server scope: just the bare tool name ---
        async with streamable_http_client(f"{base}/mcp/host/{LABEL}/project/nix/server/lsp") as (read, write):
            async with ClientSession(read, write) as session:
                await session.initialize()
                scoped = {t.name for t in (await session.list_tools()).tools}
                assert scoped == {"echo", "boom"}
                result = await session.call_tool("echo", {"message": "scoped"})
                assert "echo: scoped" in result.content[0].text

        # --- the legacy host/server scope still works for a project-less server ---
        async with streamable_http_client(f"{base}/mcp/host/{LABEL}/server/demo") as (read, write):
            async with ClientSession(read, write) as session:
                await session.initialize()
                scoped = {t.name for t in (await session.list_tools()).tools}
                assert scoped == {"echo", "boom"}

        # --- the Endpoints panel reflects what is actually connected ---
        endpoints = (await http.get("/api/endpoints")).json()
        assert endpoints["localBaseUrl"] == base
        assert endpoints["publicUrl"] is None
        assert endpoints["installCommand"] is None  # MCP_SWITCHBOARD_PUBLIC_URL is unset
        paths = {row["path"] for row in endpoints["rows"]}
        assert {
            "/mcp",
            f"/mcp/host/{LABEL}",
            f"/mcp/host/{LABEL}/project/nix",
            f"/mcp/host/{LABEL}/project/nix/server/lsp",
            f"/mcp/host/{LABEL}/server/demo",
        } <= paths

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
async def test_rapid_restarts_do_not_lose_tools(running_hub, client_process):
    """Integration coverage for repeated restarts against a real subprocess.

    This is the black-box companion to
    hub/tests/test_tunnel.py::test_stop_channel_waits_for_the_old_owner_task_to_finish,
    which deterministically proves the actual fix (a fast restart could
    previously start a new owner task while the old one was still unwinding
    in the background, and the two would race over shared registry state -
    symptom: "unhandled errors in a TaskGroup", server up with no tools). This
    real-subprocess version depends on OS scheduling to hit the exact window
    and did not reliably reproduce the original bug even with the fix
    reverted in this environment, so it does not by itself prove the fix -
    it proves restarts keep working under real, repeated churn.
    """
    hub, settings = running_hub
    base = f"http://127.0.0.1:{settings.private_port}"

    async with httpx.AsyncClient(base_url=base, timeout=20.0) as http:

        async def demo_ready():
            snapshot = (await http.get("/api/connections")).json()
            for connection in snapshot["connections"]:
                for server in connection["servers"]:
                    if server["name"] == "demo" and server["state"] == "running" and server["tools"]:
                        return connection["id"]
            return None

        if client_process.poll() is not None:
            pytest.fail(f"client exited early:\n{client_process.stdout.read()}")

        connection_id = await wait_for(demo_ready)

        # Fire several restarts back to back - the hub only sends the frame
        # and returns, so this reproduces "starting" then "running" state
        # frames arriving in quick succession, which is exactly the race
        # window the fix closes.
        for _ in range(5):
            response = await http.post(f"/api/connections/{connection_id}/servers/demo/restart")
            assert response.status_code == 204

        async def demo_recovered():
            snapshot = (await http.get("/api/connections")).json()
            for connection in snapshot["connections"]:
                if connection["id"] != connection_id:
                    continue
                for server in connection["servers"]:
                    if server["name"] != "demo":
                        continue
                    if server["state"] == "failed":
                        pytest.fail(f"demo failed to recover after rapid restarts: {server['error']}")
                    if server["state"] == "running" and server["tools"]:
                        return True
            return None

        await wait_for(demo_recovered, timeout=30.0)

        # And it must actually work afterwards, not just claim to have tools.
        response = await http.post(
            f"/api/connections/{connection_id}/servers/demo/tools/echo/call",
            json={"arguments": {"message": "post-restart"}},
        )
        assert response.status_code == 200
        assert response.json()["status"] == "ok"


@pytest.mark.anyio
async def test_endpoints_panel_renders_install_command_when_public_url_is_set(tmp_path):
    settings = Settings(
        tunnel_host="127.0.0.1",
        tunnel_port=free_port(),
        tunnel_token=TOKEN,
        private_host="127.0.0.1",
        private_port=free_port(),
        data_dir=tmp_path / "state",
        log_level="WARNING",
        public_url="https://hp.example.ts.net:8443",
    )
    hub = build_hub(settings)
    private = uvicorn.Server(
        uvicorn.Config(hub.private_app, host=settings.private_host, port=settings.private_port, log_level="warning")
    )
    private.install_signal_handlers = lambda: None

    async with hub.lifespan():
        task = asyncio.create_task(private.serve())
        await wait_for(lambda: asyncio.sleep(0, result=private.started))
        try:
            async with httpx.AsyncClient(base_url=f"http://127.0.0.1:{settings.private_port}") as http:
                body = (await http.get("/api/endpoints")).json()
                assert body["publicUrl"] == "https://hp.example.ts.net:8443"
                assert body["installCommand"] == (
                    f"curl -fsSL https://akospapp.github.io/mcp-switchboard/install.sh | sh -s -- "
                    f"--hub-url https://hp.example.ts.net:8443 --token {TOKEN}"
                )
        finally:
            private.should_exit = True
            await task


@pytest.mark.anyio
async def test_tunnel_rejects_a_bad_token(running_hub):
    import websockets

    _, settings = running_hub
    url = f"ws://127.0.0.1:{settings.tunnel_port}/tunnel/v1"

    with pytest.raises(Exception) as excinfo:
        async with websockets.connect(url, additional_headers={"Authorization": "Bearer wrong"}):
            pass
    assert "403" in str(excinfo.value) or "401" in str(excinfo.value) or "rejected" in str(excinfo.value).lower()
