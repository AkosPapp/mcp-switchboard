"""HubConnection: hello, frame routing, restart, reconnect, fatal errors.

Both the WebSocket and the local servers are faked; the real ones are covered
by test_supervisor.py and by the hub's own tests.
"""

import asyncio
import json
import logging

import pytest

from mcp_switchboard_client import protocol
from mcp_switchboard_client.config import ServerSpec
from mcp_switchboard_client import tunnel as tunnel_mod
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
        **overrides,
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
    assert "unsupported protocol version" in connection.failure


async def test_label_already_connected_is_retried():
    sessions = [
        FakeWebSocket([protocol.error("label legion5 is already connected")]),
        FakeWebSocket([protocol.hello_ack("c1", "hub", "0.1.0")]),
    ]
    connection, connect = make_connection(sessions, max_retries=5)

    await connection.run()

    assert len(connect.calls) >= 2
    assert connection.servers["git"].restarts >= 1  # second session got its ack


async def test_max_retries_exhaustion_sets_failure():
    connection, _ = make_connection([], max_retries=2)

    await connection.run()

    assert connection.failure and "giving up" in connection.failure


async def test_clean_stop_has_no_failure():
    ws = FakeWebSocket([protocol.hello_ack("c1", "hub", "0.1.0")])
    connection, _ = make_connection([ws], max_retries=0, when_exhausted=lambda: connection.request_stop())

    await connection.run()

    assert connection.failure is None


async def test_backoff_delay_is_jittered(monkeypatch):
    delays = []

    async def fake_sleep(self, delay):
        delays.append(delay)
        return False

    monkeypatch.setattr(HubConnection, "_sleep_or_stop", fake_sleep)
    monkeypatch.setattr(tunnel_mod.random, "uniform", lambda a, b: b)
    connection, _ = make_connection([], max_retries=3)
    connection.settings.reconnect_delay = 1.0

    await connection.run()

    assert delays == pytest.approx([1.2, 2.4])
    assert tunnel_mod.BACKOFF_JITTER == 0.2


async def test_slow_server_does_not_stall_others():
    connection, _ = make_connection([], max_retries=1)
    gate = asyncio.Event()
    slow, fast = connection.servers["git"], connection.servers["fetch"]

    async def slow_send(line):
        await gate.wait()
        slow.received.append(line)

    slow.send = slow_send
    connection._ws = FakeWebSocket([])
    try:
        await connection._handle_frame(json.dumps(protocol.mcp("git", {"id": 1})))
        await connection._handle_frame(json.dumps(protocol.mcp("git", {"id": 2})))
        await connection._handle_frame(json.dumps(protocol.mcp("fetch", {"id": 3})))
        await asyncio.sleep(0.05)
        assert len(fast.received) == 1
        assert slow.received == []
        gate.set()
        await asyncio.sleep(0.05)
        assert slow.received == [json.dumps({"id": 1}), json.dumps({"id": 2})]  # ordered
    finally:
        await connection._shutdown_workers(drain=False)
    assert connection._workers == {}


async def test_workers_are_cancelled_on_disconnect():
    ws = FakeWebSocket([protocol.hello_ack("c1", "hub", "0.1.0"), protocol.mcp("git", {"id": 1})])
    connection, _ = make_connection([ws])
    await connection.run()
    assert connection._workers == {} and connection._queues == {}


@pytest.mark.parametrize(
    "url,warns",
    [
        ("ws://hub.example.com", True),
        ("ws://192.168.1.5:8080", True),
        ("ws://localhost:8080", False),
        ("ws://127.0.0.2:8080", False),
        ("ws://[::1]:8080", False),
        ("wss://hub.example.com", False),
    ],
)
def test_plaintext_remote_ws_warns(url, warns, caplog):
    with caplog.at_level(logging.WARNING, logger="mcp_switchboard_client.tunnel"):
        make_connection([], hub_url=url)
    assert any("plaintext" in r.message for r in caplog.records) is warns


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


# -- label-replacement damping (eviction-tennis guard) --------------------


def _replacement_ws():
    """A session that acks, learns it was replaced, and drops — like a second
    live client on the same label sees, over and over."""
    return FakeWebSocket([
        protocol.hello_ack("c1", "hub", "0.1"),
        protocol.error('label "legion5" is already connected from a newer connection; this one is replaced', ""),
    ])


async def test_replaced_after_ack_is_flagged_not_fatal():
    ws = FakeWebSocket([
        protocol.hello_ack("c1", "hub", "0.1"),
        protocol.error('label "legion5" is already connected from a newer connection', ""),
    ])
    connection, _ = make_connection([ws], max_retries=1)
    await connection.run()
    # The point: post-ack "already connected" never raises (the run loop ran
    # to its normal give-up), and it flags the session as churn instead.
    assert connection._replaced is True


async def test_backoff_keeps_growing_across_fast_replacements():
    """Three ack-then-replaced sessions in a row must grow the delay (attempt
    1,2,3), not restart at the base every time like plain success would."""
    delays = []
    connection, _ = make_connection([], max_retries=4)
    connection.settings.reconnect_delay = 0.5

    async def fake_sleep(delay):
        delays.append(delay)
        return False

    connection._sleep_or_stop = fake_sleep
    await connection.run()
    assert connection.failure is not None and "giving up" in connection.failure
    assert len(delays) == 3
    assert delays[1] > delays[0] and delays[2] > delays[1], f"backoff did not grow: {delays}"


class _HeldThenClose(FakeWebSocket):
    """Acks, holds the session open, then the stream ends (a clean drop)."""

    def __init__(self, hold):
        super().__init__([protocol.hello_ack("c1", "hub", "0.1")])
        self._hold = hold

    async def __anext__(self):
        if self.inbound:
            return self.inbound.pop(0)
        await asyncio.sleep(self._hold)
        raise StopAsyncIteration


def connection_stop(connection):
    connection.request_stop()


async def test_stable_session_re_bases_the_backoff(monkeypatch):
    monkeypatch.setattr(tunnel_mod, "STABLE_CONNECTION", 0.05)
    delays = []
    sessions = [
        _replacement_ws(),            # churn 1 -> attempt 1
        _HeldThenClose(0.3),          # holds past the stability window
        _replacement_ws(),            # churn 2 -> attempt was reset -> smallest delay
        _replacement_ws(),            # exhausts the retry budget
    ]
    connection, _ = make_connection(sessions, max_retries=3)
    connection.settings.reconnect_delay = 0.5

    async def fake_sleep(delay):
        delays.append(delay)
        return len(delays) >= 3

    connection._sleep_or_stop = fake_sleep
    await connection.run()
    assert len(delays) == 3
    # Without the stable-session rule these would be ~1x, 2x, 4x of the base.
    # With it: the churn before the stable session is 1x, the drop after the
    # stable session is re-based to ~1x again, and plain churn grows from there.
    assert delays[1] < delays[0] * 1.5, f"no re-base after the stable session: {delays}"
    assert delays[2] > delays[1], f"growth must simply start over: {delays}"
