"""LocalServer against real subprocesses."""

import asyncio
import json
import sys

import pytest

from mcp_switchboard_client import protocol
from mcp_switchboard_client.config import ServerSpec
from mcp_switchboard_client.supervisor import LocalServer

pytestmark = pytest.mark.asyncio

TIMEOUT = 10

# Echoes each stdin line back on stdout, and prints its own noise on stderr.
ECHO = (
    "import os,sys\n"
    "sys.stderr.write('server log line\\n')\n"
    "for line in sys.stdin:\n"
    "    sys.stdout.write(line)\n"
    "    sys.stdout.flush()\n"
)


def echo_spec(name="echo", **kwargs):
    return ServerSpec(name=name, argv=[sys.executable, "-u", "-c", ECHO], **kwargs)


class Recorder:
    def __init__(self):
        self.lines = asyncio.Queue()
        self.states = []
        self.state_events = asyncio.Queue()

    async def on_stdout(self, name, line):
        await self.lines.put((name, line))

    async def on_state(self, name, state, exit_code, error):
        self.states.append((name, state, exit_code, error))
        await self.state_events.put((name, state, exit_code, error))

    async def next_line(self):
        return await asyncio.wait_for(self.lines.get(), TIMEOUT)

    async def wait_for_state(self, state):
        while True:
            event = await asyncio.wait_for(self.state_events.get(), TIMEOUT)
            if event[1] == state:
                return event


@pytest.fixture
def recorder():
    return Recorder()


async def test_start_send_receive_stop(recorder):
    server = LocalServer(echo_spec(), on_stdout=recorder.on_stdout, on_state=recorder.on_state)
    await server.start()
    try:
        assert server.running
        assert [s[1] for s in recorder.states] == [
            protocol.STATE_STARTING,
            protocol.STATE_RUNNING,
        ]

        payload = json.dumps({"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
        await server.send(payload)
        assert await recorder.next_line() == ("echo", payload)

        # A trailing newline on the way in must not become a blank line.
        await server.send(payload + "\n")
        assert await recorder.next_line() == ("echo", payload)
    finally:
        await server.stop()

    assert not server.running
    # A clean stop is not reported as an exit: the hub asked for it.
    assert [s[1] for s in recorder.states] == [
        protocol.STATE_STARTING,
        protocol.STATE_RUNNING,
    ]


async def test_start_is_idempotent(recorder):
    server = LocalServer(echo_spec(), on_state=recorder.on_state)
    await server.start()
    try:
        process = server._process
        await server.start()
        assert server._process is process
    finally:
        await server.stop()


async def test_stop_is_safe_to_call_twice_concurrently(recorder):
    server = LocalServer(echo_spec(), on_stdout=recorder.on_stdout, on_state=recorder.on_state)
    await server.start()
    process = server._process

    await asyncio.wait_for(asyncio.gather(server.stop(), server.stop()), TIMEOUT)

    assert process.returncode is not None
    assert not server.running
    await server.stop()  # and a third time, sequentially


async def test_restart_gives_a_new_process(recorder):
    server = LocalServer(echo_spec(), on_stdout=recorder.on_stdout, on_state=recorder.on_state)
    await server.start()
    try:
        first = server._process
        await server.restart()
        assert server._process is not first
        assert first.returncode is not None
        assert server.running

        payload = json.dumps({"jsonrpc": "2.0", "id": 2})
        await server.send(payload)
        assert await recorder.next_line() == ("echo", payload)
    finally:
        await server.stop()

    assert [s[1] for s in recorder.states] == [
        protocol.STATE_STARTING,
        protocol.STATE_RUNNING,
        protocol.STATE_STARTING,
        protocol.STATE_RUNNING,
    ]


async def test_a_server_that_exits_on_its_own_is_reported(recorder):
    spec = ServerSpec(name="quitter", argv=[sys.executable, "-c", "import sys; sys.exit(3)"])
    server = LocalServer(spec, on_stdout=recorder.on_stdout, on_state=recorder.on_state)
    await server.start()

    name, state, code, _error = await recorder.wait_for_state(protocol.STATE_EXITED)
    assert (name, code) == ("quitter", 3)
    # Reported, not respawned.
    assert [s[1] for s in recorder.states].count(protocol.STATE_STARTING) == 1
    await server.stop()


async def test_a_server_that_cannot_be_spawned_is_reported_failed(recorder, tmp_path):
    spec = ServerSpec(name="ghost", argv=[str(tmp_path / "no-such-binary")])
    server = LocalServer(spec, on_state=recorder.on_state)
    await server.start()

    name, state, code, error = await recorder.wait_for_state(protocol.STATE_FAILED)
    assert name == "ghost" and error
    assert not server.running
    await server.stop()


async def test_send_to_a_dead_server_is_dropped(recorder):
    server = LocalServer(echo_spec(), on_stdout=recorder.on_stdout, on_state=recorder.on_state)
    await server.start()
    await server.stop()
    await server.send('{"jsonrpc": "2.0"}')  # must not raise
    assert recorder.lines.empty()


async def test_ignores_non_json_stdout_noise(recorder):
    # The supervisor forwards lines verbatim; filtering is the tunnel's job.
    spec = ServerSpec(
        name="noisy",
        argv=[sys.executable, "-u", "-c", "print('hello, not json'); print('{\"a\": 1}')"],
    )
    server = LocalServer(spec, on_stdout=recorder.on_stdout, on_state=recorder.on_state)
    await server.start()
    try:
        assert await recorder.next_line() == ("noisy", "hello, not json")
        assert await recorder.next_line() == ("noisy", '{"a": 1}')
    finally:
        await server.stop()


async def test_env_and_cwd_are_applied(recorder, tmp_path):
    spec = ServerSpec(
        name="envtest",
        argv=[sys.executable, "-u", "-c", "import os; print(os.environ['MARKER'], os.getcwd())"],
        env={"MARKER": "set-by-spec"},
        cwd=str(tmp_path),
    )
    server = LocalServer(spec, on_stdout=recorder.on_stdout, on_state=recorder.on_state)
    await server.start()
    try:
        _name, line = await recorder.next_line()
        marker, cwd = line.split(" ", 1)
        assert marker == "set-by-spec"
        assert cwd.endswith(str(tmp_path).split("/")[-1])
    finally:
        await server.stop()


async def test_tempfile_survives_a_restart_and_goes_on_stop(recorder, tmp_path):
    tempfile = tmp_path / "fastmcp-generated.json"
    tempfile.write_text("{}")
    server = LocalServer(
        echo_spec(name="fastmcp", fastmcp_tempfile=tempfile),
        on_state=recorder.on_state,
    )
    await server.start()
    try:
        await server.restart()
        assert tempfile.exists()  # the respawn still needs it
    finally:
        await server.stop()
    assert not tempfile.exists()


async def test_long_lines_survive_the_stream_limit(recorder):
    # asyncio's default 64 KiB StreamReader limit would blow up on this.
    spec = ServerSpec(
        name="chatty",
        argv=[sys.executable, "-u", "-c", "print('x' * 200000)"],
    )
    server = LocalServer(spec, on_stdout=recorder.on_stdout, on_state=recorder.on_state)
    await server.start()
    try:
        _name, line = await recorder.next_line()
        assert len(line) == 200000
    finally:
        await server.stop()
