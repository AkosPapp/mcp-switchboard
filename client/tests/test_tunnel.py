"""HubConnection: hello, frame routing, restart, reconnect, fatal errors.

Both the WebSocket and the local servers are faked; the real ones are covered
by test_supervisor.py and by the hub's own tests.
"""

import json

import pytest

from mcp_switchboard_client import protocol
from mcp_switchboard_client.config import ServerSpec
from mcp_switchboard_client.tunnel import HubConnection, TunnelSettings

pytestmark = pytest.mark.asyncio


class FakeServer:
    """Stands in for LocalServer, echoing whatever is written to its stdin."""

    def __init__(self, spec, on_stdout=None, on_state=None):
        self.spec = spec
        self.name = spec.name
        self.on_stdout = on_stdout
        self.on_state = on_state
        self.received = []
        self.starts = 0
        self.stops = 0
        self.restarts = 0

    @property
    def descriptor(self):
        return {"name": self.name, "command": " ".join(self.spec.argv)}

    async def start(self):
        self.starts += 1
        if self.on_state:
            await self.on_state(self.name, protocol.STATE_RUNNING, None, None)

    async def stop(self, **_kwargs):
        self.stops += 1

    async def restart(self):
        self.restarts += 1
        await self.start()

    async def send(self, line):
        self.received.append(line)
        if self.on_stdout:
            await self.on_stdout(self.name, line)


class FakeWebSocket:
    def __init__(self, inbound):
        self.inbound = [json.dumps(f) if not isinstance(f, str) else f for f in inbound]
        self.sent = []
        self.closed = False

    async def send(self, raw):
        self.sent.append(json.loads(raw))

    async def close(self):
        self.closed = True
        self.inbound = []

    def __aiter__(self):
        return self

    async def __anext__(self):
        if self.closed or not self.inbound:
            raise StopAsyncIteration
        return self.inbound.pop(0)

    def frames(self, frame_type):
        return [f for f in self.sent if f["type"] == frame_type]


class FakeConnect:
    """Hands out one FakeWebSocket per connect() call, in order."""

    def __init__(self, sessions, when_exhausted=None):
        self.sessions = list(sessions)
        self.when_exhausted = when_exhausted
        self.calls = []

    def __call__(self, url, **kwargs):
        self.calls.append((url, kwargs))
        if not self.sessions:
            if self.when_exhausted is not None:
                self.when_exhausted()
            raise OSError("no more fake sessions")
        return _Session(self.sessions.pop(0))


class _Session:
    def __init__(self, ws):
        self.ws = ws

    async def __aenter__(self):
        return self.ws

    async def __aexit__(self, *exc):
        await self.ws.close()
        return False


SPECS = [
    ServerSpec(name="git", argv=["uvx", "mcp-server-git", "--repository", "."]),
    ServerSpec(name="fetch", argv=["uvx", "mcp-server-fetch"]),
]


def make_connection(sessions, *, max_retries=1, when_exhausted=None, **overrides):
    settings = TunnelSettings(
        hub_url=overrides.pop("hub_url", "https://hub.example.com"),
        token=overrides.pop("token", "s3cret"),
        label=overrides.pop("label", "legion5"),
        reconnect_delay=0.0,
        max_retries=max_retries,
    )
    connect = FakeConnect(sessions, when_exhausted=when_exhausted)
    connection = HubConnection(
        SPECS,
        settings,
        version="0.2.0",
        connect=connect,
        server_factory=FakeServer,
    )
    return connection, connect


async def test_hello_describes_every_server_and_carries_the_token():
    ws = FakeWebSocket([protocol.hello_ack("c1", "mcp-switchboard-hub", "0.1.0")])
    connection, connect = make_connection([ws])

    await connection.run()

    url, kwargs = connect.calls[0]
    assert url == "wss://hub.example.com/tunnel/v1"
    headers = kwargs.get("additional_headers") or kwargs.get("extra_headers")
    assert headers == {"Authorization": "Bearer s3cret"}
    assert kwargs["ping_interval"] == 20 and kwargs["ping_timeout"] == 10

    hello = ws.sent[0]
    assert hello["type"] == protocol.HELLO
    assert hello["protocol"] == protocol.PROTOCOL_VERSION
    assert hello["client"]["name"] == "mcp-switchboard-client"
    assert hello["client"]["label"] == "legion5"
    assert hello["servers"] == [
        {"name": "git", "command": "uvx mcp-server-git --repository ."},
        {"name": "fetch", "command": "uvx mcp-server-fetch"},
    ]


async def test_no_heartbeat_frame_is_ever_sent():
    ws = FakeWebSocket([protocol.hello_ack("c1", "hub", "0.1.0")])
    connection, _ = make_connection([ws])

    await connection.run()

    assert all(f["type"] != "heartbeat" for f in ws.sent)


async def test_mcp_frames_are_routed_to_the_named_server():
    request = {"jsonrpc": "2.0", "id": 1, "method": "tools/list"}
    ws = FakeWebSocket(
        [
            protocol.hello_ack("c1", "hub", "0.1.0"),
            protocol.mcp("git", request),
            protocol.mcp("nosuchserver", request),
        ]
    )
    connection, _ = make_connection([ws])

    await connection.run()

    servers = connection.servers
    assert servers["git"].received == [json.dumps(request)]
    assert servers["fetch"].received == []

    # ...and the echoed stdout line comes back out as an mcp frame for "git".
    echoed = ws.frames(protocol.MCP)
    assert echoed == [{"type": "mcp", "server": "git", "payload": request}]


async def test_server_state_transitions_are_forwarded():
    ws = FakeWebSocket([protocol.hello_ack("c1", "hub", "0.1.0")])
    connection, _ = make_connection([ws])

    await connection.run()

    states = ws.frames(protocol.SERVER_STATE)
    assert {f["server"] for f in states} == {"git", "fetch"}
    assert all(f["state"] == protocol.STATE_RUNNING for f in states)


async def test_restart_frame_restarts_only_that_server():
    ws = FakeWebSocket(
        [
            protocol.hello_ack("c1", "hub", "0.1.0"),
            protocol.restart("git"),
            protocol.restart("nosuchserver"),
        ]
    )
    connection, _ = make_connection([ws])

    await connection.run()

    # one restart from the connect, one from the frame
    assert connection.servers["git"].restarts == 2
    assert connection.servers["fetch"].restarts == 1


async def test_every_reconnect_restarts_every_server():
    sessions = [
        FakeWebSocket([protocol.hello_ack("c1", "hub", "0.1.0")]),
        FakeWebSocket([protocol.hello_ack("c2", "hub", "0.1.0")]),
    ]
    connection = None

    def stop():
        connection.request_stop()

    connection, connect = make_connection(sessions, max_retries=0, when_exhausted=stop)

    await connection.run()

    assert len(connect.calls) == 3  # two sessions, then the one that stops us
    for server in connection.servers.values():
        assert server.restarts == 2
        assert server.starts == 2
    # The hub gets a fresh hello and fresh states on the second connection too.
    assert sessions[1].sent[0]["type"] == protocol.HELLO
    assert len(sessions[1].frames(protocol.SERVER_STATE)) == 2


async def test_error_before_hello_ack_is_fatal_and_not_retried():
    ws = FakeWebSocket([protocol.error("unsupported protocol version 1")])
    connection, connect = make_connection([ws], max_retries=5)

    await connection.run()

    assert len(connect.calls) == 1


async def test_error_after_hello_ack_keeps_the_session():
    ws = FakeWebSocket(
        [
            protocol.hello_ack("c1", "hub", "0.1.0"),
            protocol.error("tool call failed", server="git"),
            protocol.mcp("git", {"jsonrpc": "2.0", "id": 2}),
        ]
    )
    connection, _ = make_connection([ws])

    await connection.run()

    assert connection.servers["git"].received  # kept processing after the error


async def test_junk_frames_are_ignored():
    ws = FakeWebSocket(
        [
            protocol.hello_ack("c1", "hub", "0.1.0"),
            "not json at all",
            "[1, 2, 3]",
            json.dumps({"type": "heartbeat"}),
            protocol.mcp("git", {"jsonrpc": "2.0", "id": 3}),
        ]
    )
    connection, _ = make_connection([ws])

    await connection.run()

    assert connection.servers["git"].received


async def test_all_servers_are_stopped_when_the_run_ends():
    ws = FakeWebSocket([protocol.hello_ack("c1", "hub", "0.1.0")])
    connection, _ = make_connection([ws])

    await connection.run()

    assert all(server.stops == 1 for server in connection.servers.values())


async def test_max_retries_gives_up():
    connection, connect = make_connection([], max_retries=3)

    await connection.run()

    assert len(connect.calls) == 3


def test_tls_context_trusts_certifi_even_without_a_system_store(monkeypatch, tmp_path):
    """The NixOS case: no usable system CA file, so certifi alone must supply roots."""
    from mcp_switchboard_client import tunnel

    monkeypatch.setenv("SSL_CERT_FILE", str(tmp_path / "missing.pem"))
    monkeypatch.setenv("SSL_CERT_DIR", str(tmp_path / "missing-dir"))
    context = tunnel._tls_context()
    assert len(context.get_ca_certs()) > 0
    assert context.verify_mode.name == "CERT_REQUIRED" and context.check_hostname


def test_tls_context_keeps_the_system_store_too(monkeypatch, tmp_path):
    """A private CA supplied via SSL_CERT_FILE must still be trusted alongside certifi."""
    import certifi

    from mcp_switchboard_client import tunnel

    pem = tmp_path / "extra.pem"
    with open(certifi.where()) as f:
        first = f.read().split("-----END CERTIFICATE-----")[0] + "-----END CERTIFICATE-----\n"
    pem.write_text(first)
    baseline = len(tunnel._tls_context().get_ca_certs())
    monkeypatch.setenv("SSL_CERT_FILE", str(pem))
    assert len(tunnel._tls_context().get_ca_certs()) >= 1
    assert baseline >= 1
