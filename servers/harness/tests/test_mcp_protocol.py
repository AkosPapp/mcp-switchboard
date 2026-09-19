"""Talks to the server the way a real client does: JSON-RPC over stdio."""

import json
import subprocess
import sys
import time

import pytest

SERVER = [sys.executable, "-m", "mcp_switchboard_server_harness"]


def converse(root, calls):
    """Send initialize + the given (id, method, params) requests; return {id: reply}.

    stdin stays open until every reply has arrived: closing it early makes the
    server shut down and abandon calls still in flight.
    """
    frames = [
        {"jsonrpc": "2.0", "id": 0, "method": "initialize",
         "params": {"protocolVersion": "2025-06-18", "capabilities": {}, "clientInfo": {"name": "t", "version": "0"}}},
        {"jsonrpc": "2.0", "method": "notifications/initialized"},
        *({"jsonrpc": "2.0", "id": i, "method": m, "params": p} for i, m, p in calls),
    ]
    wanted = {0, *(i for i, _, _ in calls)}
    proc = subprocess.Popen(
        SERVER + ["--root", str(root)],
        stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True,
    )
    replies = {}
    try:
        proc.stdin.write("\n".join(json.dumps(f) for f in frames) + "\n")
        proc.stdin.flush()
        deadline = time.time() + 30
        while wanted - replies.keys() and time.time() < deadline:
            line = proc.stdout.readline()
            if not line:
                break
            msg = json.loads(line)
            if "id" in msg:
                replies[msg["id"]] = msg
    finally:
        proc.kill()
        proc.wait()
    return replies


@pytest.fixture
def replies(tmp_path):
    return converse(
        tmp_path,
        [
            (1, "tools/list", {}),
            (2, "tools/call", {"name": "file_write", "arguments": {"path": "a.txt", "content": "hi"}}),
            (3, "tools/call", {"name": "run_command", "arguments": {"command": "cat a.txt"}}),
            (4, "tools/call", {"name": "file_read", "arguments": {"path": "../nope"}}),
        ],
    )


def test_every_tool_has_an_output_schema_and_annotations(replies):
    tools = {t["name"]: t for t in replies[1]["result"]["tools"]}
    assert len(tools) == 24
    for name, tool in tools.items():
        assert tool["annotations"]["readOnlyHint"] in (True, False), name
    assert tools["file_read"]["annotations"]["readOnlyHint"] is True
    assert tools["file_delete"]["annotations"]["destructiveHint"] is True
    assert tools["git_push"]["annotations"]["destructiveHint"] is True
    assert set(tools["run_command"]["outputSchema"]["properties"]) >= {"stdout", "stderr", "exit_code"}
    assert "run_bash" not in tools and "list_dir" not in tools


def test_tools_work_over_the_protocol(replies):
    assert replies[2]["result"]["structuredContent"]["success"] is True
    result = replies[3]["result"]["structuredContent"]
    assert (result["stdout"], result["exit_code"]) == ("hi", 0)


def test_confinement_error_reaches_the_client_as_a_tool_error(replies):
    assert replies[4]["result"]["isError"] is True
    # the reason must reach the model, not just a generic "Error executing tool"
    assert "outside the allowed root" in json.dumps(replies[4])
