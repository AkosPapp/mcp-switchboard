import asyncio
import base64
import os
import shutil
import subprocess
import time

import pytest

from mcp_switchboard_server_harness import server as h


def run(coro):
    return asyncio.run(coro)


# --- confinement -----------------------------------------------------------


def test_paths_outside_the_root_are_refused(tmp_path):
    inner = tmp_path / "root"
    inner.mkdir()
    h.configure(str(inner))
    (tmp_path / "secret.txt").write_text("s")
    for bad in ["../secret.txt", str(tmp_path / "secret.txt"), "a/../../secret.txt"]:
        with pytest.raises(PermissionError):
            h.read(bad)
    with pytest.raises(PermissionError):
        h.write("../x", "y")


def test_symlink_escaping_the_root_is_refused(tmp_path):
    inner = tmp_path / "root"
    inner.mkdir()
    (tmp_path / "secret.txt").write_text("s")
    (inner / "link").symlink_to(tmp_path / "secret.txt")
    h.configure(str(inner))
    with pytest.raises(PermissionError):
        h.read("link")


def test_deleting_a_symlink_removes_the_link_not_the_target(tmp_path):
    inner = tmp_path / "root"
    inner.mkdir()
    target = tmp_path / "keep.txt"
    target.write_text("k")
    (inner / "link").symlink_to(target)
    h.configure(str(inner))
    assert h.file_delete("link").deleted
    assert target.exists() and not (inner / "link").exists()


def test_root_cannot_be_deleted(tmp_path):
    for p in [".", str(tmp_path)]:
        r = h.file_delete(p)
        assert not r.deleted and "root" in r.message
    assert tmp_path.exists()


def test_relative_paths_resolve_against_the_root(tmp_path):
    h.write("sub/f.txt", "hi")
    assert (tmp_path / "sub" / "f.txt").read_text() == "hi"
    assert h.read(".").splitlines() == ["sub/"]


# --- shell -----------------------------------------------------------------


def test_bash(tmp_path):
    r = run(h.bash("echo $FOO; pwd; echo e >&2; exit 2", env={"FOO": "bar"}))
    assert r.exit_code == 2
    assert r.stdout.split() == ["bar", str(tmp_path)]
    assert r.stderr == "e\n" and r.return_value is None and not r.truncated


def test_bash_cwd_is_confined(tmp_path):
    (tmp_path / "d").mkdir()
    assert run(h.bash("pwd", cwd="d")).stdout.strip() == str(tmp_path / "d")
    with pytest.raises(PermissionError):
        run(h.bash("pwd", cwd="/"))


def test_output_is_truncated_with_totals():
    h.configure(None, 10)
    r = run(h.bash("printf 'x%.0s' $(seq 1 50)"))
    assert len(r.stdout) == 10 and r.truncated and r.stdout_total == 50


def test_timeout_kills_the_whole_process_group(tmp_path):
    marker = tmp_path / "child_alive"
    cmd = f"(sleep 2; touch {marker}) & sleep 30"
    start = time.time()
    r = run(h.bash(cmd, timeout=0.5))
    assert time.time() - start < 10
    assert r.timed_out and r.exit_code == h.TIMEOUT_EXIT_CODE
    time.sleep(2.5)
    assert not marker.exists(), "background child survived the timeout"


def test_timeout_returns_partial_output(tmp_path):
    r = run(h.bash("echo first; echo oops >&2; sleep 30", timeout=1))
    assert r.timed_out and r.exit_code == h.TIMEOUT_EXIT_CODE and r.truncated
    assert r.stdout == "first\n" and r.stderr == "oops\n"
    assert r.stdout_total == 6 and r.stderr_total == 5
    assert r.applied_timeout_s == 1 and r.elapsed_s is not None and r.elapsed_s < 15


def test_missing_binary_is_a_clear_error():
    with pytest.raises(RuntimeError, match="not found"):
        run(h._run(["definitely-not-a-binary"]))


def test_a_slow_command_does_not_block_others():
    async def both():
        t0 = time.time()
        await asyncio.gather(*(h.bash("sleep 1") for _ in range(3)))
        return time.time() - t0

    assert run(both()) < 2.5  # concurrent, not 3s serial


# --- files -----------------------------------------------------------------


def test_write_read_text_and_binary(tmp_path):
    assert h.write("sub/f.txt", "héllo").success
    assert h.write("sub/f.txt", " world", append=True).success
    assert h.read("sub/f.txt") == "1: héllo world"

    payload = bytes(range(256))
    assert h.write("b.bin", base64.b64encode(payload).decode(), binary=True).success
    assert (tmp_path / "b.bin").read_bytes() == payload

    bad = h.write("b.bin", "***", binary=True)
    assert not bad.success and "base64" in bad.message


def test_overwrite_is_atomic_and_keeps_the_mode(tmp_path):
    p = tmp_path / "x.sh"
    p.write_text("old")
    p.chmod(0o755)
    h.write("x.sh", "new")
    assert p.read_text() == "new" and p.stat().st_mode & 0o777 == 0o755
    assert [f.name for f in tmp_path.iterdir()] == ["x.sh"]  # no temp files left behind


def test_file_delete(tmp_path):
    (tmp_path / "f").write_text("x")
    assert h.file_delete("f").deleted
    (tmp_path / "d" / "in").mkdir(parents=True)
    assert not h.file_delete("d", recursive=False).deleted and (tmp_path / "d").exists()
    assert h.file_delete("d", recursive=True).deleted and not (tmp_path / "d").exists()
    missing = h.file_delete("nope")
    assert not missing.deleted and "does not exist" in missing.message


def test_read_lists_directories(tmp_path):
    (tmp_path / "a").mkdir()
    (tmp_path / "a" / "x.txt").write_text("123")
    (tmp_path / "b.txt").write_text("")
    assert h.read(".").splitlines() == ["a/", "b.txt"]
    assert h.read("a").splitlines() == ["x.txt"]
    (tmp_path / "empty").mkdir()
    assert h.read("empty") == "(empty directory)"


def test_read_pageing_and_numbering(tmp_path):
    (tmp_path / "t").write_text("a\nb\nc\nd\n")
    r = h.read("t", offset=2, limit=2)
    assert r.startswith("2: b\n3: c") and "continue with offset=4" in r
    assert h.read("t", offset=4).startswith("4: d")
    assert h.read("t") == "1: a\n2: b\n3: c\n4: d"
    with pytest.raises(ValueError):
        h.read("t", offset=5)
    (tmp_path / "empty").write_text("")
    assert h.read("empty") == "(empty file)"
    with pytest.raises(ValueError, match="limit"):
        h.read("t", limit=0)


def test_read_respects_the_output_cap(tmp_path):
    (tmp_path / "big").write_text("x" * 50 + "\n")
    h.configure(None, 8)
    r = h.read("big")
    assert r.startswith("1: xxxxx")


def test_read_truncates_long_lines(tmp_path):
    (tmp_path / "long").write_text("y" * 3000 + "\nlast\n")
    r = h.read("long")
    assert "[... truncated]" in r and "2: last" in r


def test_file_move(tmp_path):
    (tmp_path / "a").write_text("1")
    (tmp_path / "taken").write_text("2")
    assert h.file_move("a", "new/b").moved
    assert (tmp_path / "new" / "b").read_text() == "1"
    assert not h.file_move("gone", "z").moved
    assert not h.file_move("new/b", "taken").moved and (tmp_path / "taken").read_text() == "2"


def test_edit(tmp_path):
    p = tmp_path / "f.txt"
    p.write_text("a a b")
    with pytest.raises(ValueError, match="2 times"):  # ambiguous
        h.edit("f.txt", old_string="a", new_string="X")
    assert p.read_text() == "a a b"
    assert h.edit("f.txt", old_string="b", new_string="c").startswith("replaced 1 occurrence\n")
    assert h.edit("f.txt", old_string="a", new_string="z", replace_all=True).startswith("replaced 2 occurrences")
    assert p.read_text() == "z z c"
    with pytest.raises(ValueError):
        h.edit("f.txt", old_string="missing", new_string="x")
    with pytest.raises(ValueError):
        h.edit("f.txt", old_string="", new_string="x")


def test_edit_dry_run(tmp_path):
    (tmp_path / "f.txt").write_text("one\n")
    out = h.edit("f.txt", old_string="one", new_string="two", dry_run=True)
    assert out.startswith("dry run: would replace") and "+two" in out and "-one" in out
    assert (tmp_path / "f.txt").read_text() == "one\n"


# --- search ----------------------------------------------------------------


def test_glob_without_git_skips_vendor_dirs(tmp_path):
    (tmp_path / "pkg").mkdir()
    (tmp_path / "pkg" / "m.py").write_text("")
    (tmp_path / "node_modules" / "x").mkdir(parents=True)
    (tmp_path / "node_modules" / "x" / "n.py").write_text("")
    (tmp_path / "n.txt").write_text("")
    assert run(h.glob("**/*.py")) == ["pkg/m.py"]
    assert run(h.glob("*.txt")) == ["n.txt"]
    assert run(h.glob("*.py", limit=0)) == []


def test_glob_is_sorted_newest_first(tmp_path):
    (tmp_path / "old.py").write_text("")
    os_time = 1_600_000_000
    os.utime(tmp_path / "old.py", (os_time, os_time))
    (tmp_path / "new.py").write_text("")
    assert run(h.glob("*.py")) == ["new.py", "old.py"]


def init_repo(path):
    for cmd in (["init", "-q"], ["config", "user.email", "t@example.com"], ["config", "user.name", "T"]):
        subprocess.run(["git", *cmd], cwd=path, check=True)


def test_glob_honours_gitignore(tmp_path):
    init_repo(tmp_path)
    (tmp_path / ".gitignore").write_text("ignored.py\n")
    (tmp_path / "ignored.py").write_text("")
    (tmp_path / "kept.py").write_text("")
    assert run(h.glob("*.py")) == ["kept.py"]


@pytest.mark.skipif(not shutil.which("rg"), reason="ripgrep not installed")
def test_grep(tmp_path):
    (tmp_path / "f.txt").write_text("alpha\nBeta\ngamma\n")
    (tmp_path / "g.py").write_text("beta\n")
    hit = run(h.grep("Beta"))
    assert [(m.file, m.line_no, m.text, m.is_match) for m in hit.matches] == [("f.txt", 2, "Beta", True)]
    assert len(run(h.grep("beta", ignore_case=True)).matches) == 2
    assert [m.file for m in run(h.grep("beta", ignore_case=True, include="*.py")).matches] == ["g.py"]
    ctx = run(h.grep("Beta", context=1)).matches
    assert [(m.text, m.is_match) for m in ctx] == [("alpha", False), ("Beta", True), ("gamma", False)]
    capped = run(h.grep("a", ignore_case=True, max_results=1))
    assert len(capped.matches) == 1 and capped.truncated
    assert run(h.grep("zzz")).matches == []


# --- git -------------------------------------------------------------------


def test_git_workflow(tmp_path):
    init_repo(tmp_path)
    (tmp_path / "f").write_text("one\n")
    assert "?? f" in run(h.git_status())
    run(h.git_add(["f"]))
    assert "+one" in run(h.git_diff(staged=True))
    run(h.git_commit("first"))
    assert run(h.git_log(1)).endswith("first")

    (tmp_path / "f").write_text("two\n")
    assert "+two" in run(h.git_diff())
    assert "+two" in run(h.git_diff(rev="HEAD", paths=["f"]))
    assert run(h.git_diff(paths=["f"]))
    assert "first" in run(h.git_show()) and "f" in run(h.git_show(stat_only=True))

    run(h.git_checkout("feature", create=True))
    assert "feature" in run(h.git_branch())
    with pytest.raises(RuntimeError):
        run(h.git_add(["does-not-exist"]))


def test_git_refuses_option_injection_and_escaping_paths(tmp_path):
    init_repo(tmp_path)
    with pytest.raises(ValueError):
        run(h.git_checkout("--orphan"))
    with pytest.raises(ValueError):
        run(h.git_show("--output=/tmp/x"))
    with pytest.raises(PermissionError):
        run(h.git_diff(paths=["../elsewhere"]))


# --- background processes --------------------------------------------------


def test_background_process_lifecycle():
    async def scenario():
        started = await h.process_start("echo hello; sleep 30")
        out = await h.process_read(started.id, wait=1)
        assert out.running and out.stdout == "hello\n"
        assert (await h.process_read(started.id)).stdout == ""  # already consumed
        killed = await h.process_kill(started.id)
        assert killed.killed
        done = await h.process_read(started.id)
        assert not done.running and done.exit_code is not None
        assert not (await h.process_kill(started.id)).killed
        with pytest.raises(ValueError):
            await h.process_read("nope")

    run(scenario())


def test_background_process_that_exits_reports_its_code():
    async def scenario():
        started = await h.process_start("echo done >&2; exit 4")
        out = await h.process_read(started.id, wait=5)
        assert (out.running, out.exit_code, out.stderr) == (False, 4, "done\n")

    run(scenario())


def test_too_many_background_processes():
    async def scenario():
        ids = [(await h.process_start("sleep 30")).id for _ in range(h.MAX_PROCESSES)]
        try:
            with pytest.raises(RuntimeError, match="too many"):
                await h.process_start("sleep 30")
        finally:
            for i in ids:
                await h.process_kill(i)

    run(scenario())


def test_read_lines_and_text_paging(tmp_path):
    (tmp_path / "t").write_text("a\nb\nc\nd\n")
    mid = h.read("t", offset=2, limit=2)
    assert mid.startswith("2: b\n3: c")
    tail = h.read("t", offset=3)
    assert tail.startswith("3: c\n4: d")


def test_grep_max_results_truncates(tmp_path):
    (tmp_path / "g").write_text("hit\n" * 50)
    r = run(h.grep("hit", max_results=5))
    assert len(r.matches) == 5 and r.truncated
    assert not run(h.grep("hit", max_results=500)).truncated


def test_cleanup_kills_background_process_groups():
    import os

    async def go():
        started = await h.process_start("sleep 60 & sleep 60")
        # The kill and the wait must happen while the loop runs: reaping the
        # direct child is the loop's job, and a zombie keeps the group id
        # reserved, so killpg(0) would answer "alive" forever after.
        os.killpg(started.pid, 0)  # group alive
        h._kill_all_processes()
        m = h._processes[started.id]
        await m.proc.wait()
        for _ in range(50):
            try:
                os.killpg(started.pid, 0)
            except ProcessLookupError:
                return started.pid
            await asyncio.sleep(0.1)
        raise AssertionError("process group still alive")

    run(go())


def test_read_directory_lists_all_entries(tmp_path):
    for i in range(12):
        (tmp_path / f"f{i:02}.py").write_text("x")
    (tmp_path / "d").mkdir()
    out = h.read(".").splitlines()
    assert out == ["d/"] + [f"f{i:02}.py" for i in range(12)]
