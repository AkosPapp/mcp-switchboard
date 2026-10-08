"""The honest confinement set: root + scratch + temp (W6)."""

import asyncio
import json
import os
import subprocess
import sys
import tempfile

import pytest

from mcp_switchboard_server_harness import server as h

SERVER = [sys.executable, "-m", "mcp_switchboard_server_harness"]


def run(coro):
    return asyncio.run(coro)


def test_scratch_is_created_on_demand_and_writable(tmp_path):
    assert h.scratch_dir() == tmp_path / ".harness" / "scratch"
    assert not h.scratch_dir().exists()
    assert h.write(".harness/scratch/x.txt", "hi").success
    assert h.scratch_dir().is_dir()
    assert h.read(".harness/scratch/x.txt") == "1: hi"


def test_scratch_outside_the_root_is_allowed(tmp_path):
    inner = tmp_path / "root"
    inner.mkdir()
    outside = tmp_path / "build" / "scratch"
    h.configure(str(inner), None, str(outside))
    assert h.write(str(outside / "o.txt"), "data").success
    assert (outside / "o.txt").read_text() == "data"
    assert h.read(str(outside / "o.txt")) == "1: data"
    # ... but the rest of the neighbourhood is still refused
    (tmp_path / "elsewhere.txt").write_text("no")
    with pytest.raises(PermissionError):
        h.read(str(tmp_path / "elsewhere.txt"))
    # patches stay inside the root even with a scratch outside it
    patch = f"--- a/{outside / 'p.txt'}\n+++ b/{outside / 'p.txt'}\n@@ -1 +1 @@\n-x\n+y\n"
    with pytest.raises(PermissionError):
        h.apply_patch(patch)


def test_refusal_message_names_all_three_locations(tmp_path):
    inner = tmp_path / "root"
    inner.mkdir()
    outside = tmp_path / "s"
    h.configure(str(inner), None, str(outside))
    with pytest.raises(PermissionError) as e:
        h.read("/etc/hostname")
    msg = str(e.value)
    assert "outside the allowed locations" in msg
    assert str(inner) in msg and str(outside) in msg and str(h.temp_dir()) in msg


def test_temp_dir_is_reachable_outside_the_root(monkeypatch, tmp_path):
    inner = tmp_path / "root"
    inner.mkdir()
    t = tmp_path / "mytmp"
    t.mkdir()
    monkeypatch.setattr(tempfile, "tempdir", str(t))
    h.configure(str(inner))
    assert h.write(str(t / "f.txt"), "data").success
    assert h.read(str(t / "f.txt")) == "1: data"


def test_shell_and_file_tools_agree_on_the_temp_dir(monkeypatch, tmp_path):
    """Whatever bash writes into $TMPDIR, read must see."""
    inner = tmp_path / "root"
    inner.mkdir()
    t = tmp_path / "mytmp"
    t.mkdir()
    monkeypatch.setattr(tempfile, "tempdir", str(t))
    monkeypatch.setenv("TMPDIR", str(t))
    h.configure(str(inner))
    run(h.bash('echo hi > "$TMPDIR/agreed.txt"'))
    assert h.read(str(t / "agreed.txt")) == "1: hi"


def test_configure_rejects_non_directory_scratch(tmp_path):
    f = tmp_path / "afile"
    f.write_text("x")
    with pytest.raises(ValueError):
        h.configure(None, None, str(f))


def test_cli_scratch_flag_reaches_the_server(tmp_path):
    inner = tmp_path / "root"
    inner.mkdir()
    outside = tmp_path / "sc"
    frames = [
        {"jsonrpc": "2.0", "id": 0, "method": "initialize",
         "params": {"protocolVersion": "2025-06-18", "capabilities": {}, "clientInfo": {"name": "t", "version": "0"}}},
        {"jsonrpc": "2.0", "method": "notifications/initialized"},
        {"jsonrpc": "2.0", "id": 1, "method": "tools/call",
         "params": {"name": "write", "arguments": {"path": str(outside / "x.txt"), "content": "hi"}}},
    ]
    proc = subprocess.Popen(
        SERVER + ["--root", str(inner), "--scratch", str(outside)],
        stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True,
        env={**os.environ, "TMPDIR": str(inner)},
    )
    try:
        proc.stdin.write("\n".join(json.dumps(f) for f in frames) + "\n")
        proc.stdin.flush()
        reply = {}
        while reply.get("id") != 1:
            line = proc.stdout.readline()
            if not line:
                break
            msg = json.loads(line)
            if "id" in msg:
                reply = msg
    finally:
        proc.kill()
        proc.wait()
    assert reply.get("result", {}).get("isError") is False
    assert (outside / "x.txt").read_text() == "hi"
