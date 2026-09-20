"""The acceptance test: a real client, a real MCP server, a real consumer.

Nothing here is faked, and since the rewrite nothing here is even in-process:
the hub is the compiled Go binary, run as a subprocess. A separate
`mcp-switchboard-client` process spawns a real stdio MCP server and tunnels it
to that hub over a real WebSocket; then the tool is called twice, once the way
the web console calls it and once the way n8n does (an MCP client speaking
Streamable HTTP). Both must work, and both must land in the call log and the
metrics.

This is precisely what the gateway this project replaces could never do: there,
responses from a tunneled server were received and dropped.

This suite is also the gate on the Go rewrite (spec.md 15, step 3): it was
written against the Python hub and is deliberately unchanged in what it asserts,
so that passing it means the rewrite reproduces the old behaviour rather than
redefining it. Only the fixture that starts a hub had to change.
"""

from __future__ import annotations

import asyncio
import json
import os
import re
import socket
import subprocess
import sys
from contextlib import closing
from pathlib import Path

import httpx
import pytest

REPO = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(REPO / "client" / "src"))

from mcp import ClientSession  # noqa: E402
from mcp.client.streamable_http import streamable_http_client  # noqa: E402

TOKEN = "e2e-test-token"
LABEL = "testbox"

# Where the compiled hub lands. Built once per session by the hub_binary
# fixture; set MCP_SWITCHBOARD_HUB_BINARY to point at a prebuilt one (CI builds
# it in an earlier step rather than paying for a Go build inside pytest).
HUB_MODULE = REPO / "hub"


class HubProcess:
    """A running hub, addressed the way anything else would address it."""

    def __init__(self, process: subprocess.Popen, tunnel_port: int, private_port: int, log: Path):
        self.process = process
        self.tunnel_port = tunnel_port
        self.private_port = private_port
        self.log = log

    @property
    def base(self) -> str:
        return f"http://127.0.0.1:{self.private_port}"

    def log_text(self) -> str:
        try:
            return self.log.read_text()
        except OSError:
            return "(no hub log)"


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


@pytest.fixture(scope="session")
def hub_binary(tmp_path_factory) -> str:
    """The compiled hub. Built once, reused by every test in the session."""
    prebuilt = os.environ.get("MCP_SWITCHBOARD_HUB_BINARY")
    if prebuilt:
        return prebuilt

    binary = tmp_path_factory.mktemp("hub-build") / "mcp-switchboard-hub"
    build = subprocess.run(
        ["go", "build", "-o", str(binary), "./cmd/mcp-switchboard-hub"],
        cwd=HUB_MODULE,
        env={**os.environ, "CGO_ENABLED": "0"},
        capture_output=True,
        text=True,
    )
    if build.returncode != 0:
        pytest.skip(f"cannot build the hub binary (is go on PATH?):\n{build.stderr}")
    return str(binary)


async def start_hub(binary: str, data_dir: Path, **extra_env: str) -> HubProcess:
    """Spawn a hub and wait for it to answer."""
    tunnel_port, private_port = free_port(), free_port()
    env = {
        **os.environ,
        "MCP_SWITCHBOARD_TUNNEL_HOST": "127.0.0.1",
        "MCP_SWITCHBOARD_TUNNEL_PORT": str(tunnel_port),
        "MCP_SWITCHBOARD_TUNNEL_TOKEN": TOKEN,
        "MCP_SWITCHBOARD_PRIVATE_HOST": "127.0.0.1",
        "MCP_SWITCHBOARD_PRIVATE_PORT": str(private_port),
        "MCP_SWITCHBOARD_DATA_DIR": str(data_dir),
        # Quiet by default; export MCP_SWITCHBOARD_LOG_LEVEL=INFO to get the
        # hub's own narration into the log a failing test prints.
        "MCP_SWITCHBOARD_LOG_LEVEL": os.environ.get("MCP_SWITCHBOARD_LOG_LEVEL", "WARN"),
        "MCP_SWITCHBOARD_TOOLS_TIMEOUT": "20",
        **extra_env,
    }
    # To a file rather than a pipe: a pipe nobody drains fills and blocks the
    # hub once it has logged a few hundred lines, and the log is wanted after
    # the fact anyway.
    data_dir.mkdir(parents=True, exist_ok=True)
    log_path = data_dir / "hub.log"
    log_file = open(log_path, "w")
    # Run from the data directory, not from the repo. The hub reads ./.env by
    # default, and this checkout has one: started from the repo root it would
    # inherit the developer's own PUBLIC_URL and token, and the test would be
    # asserting against whatever happens to be in that file.
    process = subprocess.Popen(
        [binary], cwd=data_dir, env=env, stdout=log_file, stderr=subprocess.STDOUT
    )
    hub = HubProcess(process, tunnel_port, private_port, log_path)

    async def answering():
        if process.poll() is not None:
            pytest.fail(f"the hub exited early:\n{hub.log_text()}")
        try:
            async with httpx.AsyncClient(timeout=2.0) as http:
                return (await http.get(f"{hub.base}/health")).status_code == 200
        except httpx.HTTPError:
            return False

    await wait_for(answering, timeout=30.0)
    return hub


def stop_hub(hub: HubProcess) -> None:
    hub.process.terminate()
    try:
        hub.process.wait(timeout=10)
    except subprocess.TimeoutExpired:
        hub.process.kill()


@pytest.fixture
async def running_hub(tmp_path, hub_binary, request):
    hub = await start_hub(hub_binary, tmp_path / "state")
    try:
        yield hub, hub
    finally:
        stop_hub(hub)
        report = getattr(request.node, "report_call", None)
        if report is not None and report.failed:
            print(f"\n--- hub log ({hub.log}) ---\n{hub.log_text()}")


@pytest.fixture
def client_process(tmp_path, running_hub):
    _, hub = running_hub
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
            "--hub-url", f"ws://127.0.0.1:{hub.tunnel_port}",
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
    hub, _ = running_hub
    base = hub.base

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
        # The gauge reads one connection. Matched loosely because the exposition
        # format allows either "1" or "1.0" for an integral float, and the Go
        # client writes the shorter of the two where the Python one wrote "1.0".
        assert re.search(r"^mcpsb_connections_active 1(\.0)?$", metrics, re.MULTILINE), metrics


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
    hub, _ = running_hub
    base = hub.base

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
        #
        # Retried, because the restarts are fired back to back and the later
        # ones are still landing: a call that arrives while demo is between
        # incarnations gets a 404 ("no tool 'echo' on ..."), which is the
        # correct answer at that instant and not what this test is about. What
        # it is about is that the server converges on working again.
        async def call_succeeds():
            response = await http.post(
                f"/api/connections/{connection_id}/servers/demo/tools/echo/call",
                json={"arguments": {"message": "post-restart"}},
            )
            if response.status_code != 200:
                return None
            body = response.json()
            return body if body["status"] == "ok" else None

        record = await wait_for(call_succeeds, timeout=30.0)
        assert "echo: post-restart" in json.dumps(record["result"])


@pytest.mark.anyio
async def test_endpoints_panel_renders_install_command_when_public_url_is_set(tmp_path, hub_binary):
    # The install command carries the tunnel token, which is why it renders on
    # the private listener only, and only when there is a public URL to dial.
    hub = await start_hub(
        hub_binary,
        tmp_path / "state",
        MCP_SWITCHBOARD_PUBLIC_URL="https://hp.example.ts.net:8443",
    )
    try:
        async with httpx.AsyncClient(base_url=hub.base) as http:
            body = (await http.get("/api/endpoints")).json()
            assert body["publicUrl"] == "https://hp.example.ts.net:8443"
            assert body["installCommand"] == (
                f"curl -fsSL https://akospapp.github.io/mcp-switchboard/install.sh | sh -s -- "
                f"--hub-url https://hp.example.ts.net:8443 --token {TOKEN}"
            )
    finally:
        stop_hub(hub)


@pytest.mark.anyio
async def test_tunnel_rejects_a_bad_token(running_hub):
    import websockets

    _, hub = running_hub
    url = f"ws://127.0.0.1:{hub.tunnel_port}/tunnel/v1"

    with pytest.raises(Exception) as excinfo:
        async with websockets.connect(url, additional_headers={"Authorization": "Bearer wrong"}):
            pass
    assert "403" in str(excinfo.value) or "401" in str(excinfo.value) or "rejected" in str(excinfo.value).lower()
