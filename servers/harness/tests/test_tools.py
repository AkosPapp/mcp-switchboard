import asyncio
import base64
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
            h.file_read(bad)
    with pytest.raises(PermissionError):
        h.file_write("../x", "y")


def test_symlink_escaping_the_root_is_refused(tmp_path):
    inner = tmp_path / "root"
    inner.mkdir()
    (tmp_path / "secret.txt").write_text("s")
    (inner / "link").symlink_to(tmp_path / "secret.txt")
    h.configure(str(inner))
    with pytest.raises(PermissionError):
        h.file_read("link")


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
    h.file_write("sub/f.txt", "hi")
    assert (tmp_path / "sub" / "f.txt").read_text() == "hi"
    assert h.dir_list(".").entries[0].path == "sub"


# --- shell -----------------------------------------------------------------


def test_run_command(tmp_path):
    r = run(h.run_command("echo $FOO; pwd; echo e >&2; exit 2", env={"FOO": "bar"}))
    assert r.exit_code == 2
    assert r.stdout.split() == ["bar", str(tmp_path)]
    assert r.stderr == "e\n" and r.return_value is None and not r.truncated


def test_run_command_cwd_is_confined(tmp_path):
    (tmp_path / "d").mkdir()
    assert run(h.run_command("pwd", cwd="d")).stdout.strip() == str(tmp_path / "d")
    with pytest.raises(PermissionError):
        run(h.run_command("pwd", cwd="/"))


def test_output_is_truncated_with_totals():
    h.configure(None, 10)
    r = run(h.run_command("printf 'x%.0s' $(seq 1 50)"))
    assert len(r.stdout) == 10 and r.truncated and r.stdout_total == 50


def test_timeout_kills_the_whole_process_group(tmp_path):
    marker = tmp_path / "child_alive"
    cmd = f"(sleep 2; touch {marker}) & sleep 30"
    start = time.time()
    with pytest.raises(RuntimeError, match="timed out"):
        run(h.run_command(cmd, timeout=0.5))
    assert time.time() - start < 5
    time.sleep(2.5)
    assert not marker.exists(), "background child survived the timeout"


def test_missing_binary_is_a_clear_error():
    with pytest.raises(RuntimeError, match="not found"):
        run(h._run(["definitely-not-a-binary"]))


def test_run_python():
    assert run(h.run_python("print(1 + 1)"))["stdout"] == "2\n"


def test_a_slow_command_does_not_block_others():
    async def both():
        t0 = time.time()
        await asyncio.gather(*(h.run_command("sleep 1") for _ in range(3)))
        return time.time() - t0

    assert run(both()) < 2.5  # concurrent, not 3s serial


# --- files -----------------------------------------------------------------


def test_write_read_text_and_binary():
    assert h.file_write("sub/f.txt", "héllo").success
    assert h.file_write("sub/f.txt", " world", append=True).success
    assert h.file_read("sub/f.txt").raw_text == "héllo world"

    payload = bytes(range(256))
    assert h.file_write("b.bin", base64.b64encode(payload).decode(), binary=True).success
    read = h.file_read("b.bin", binary=True)
    assert base64.b64decode(read.content) == payload and read.raw_text is None and read.size == 256

    bad = h.file_write("b.bin", "***", binary=True)
    assert not bad.success and "base64" in bad.message


def test_file_read_paging():
    h.file_write("f.txt", "0123456789")
    first = h.file_read("f.txt", limit=4)
    assert (first.raw_text, first.truncated) == ("0123", True)
    last = h.file_read("f.txt", offset=8, limit=4)
    assert (last.raw_text, last.truncated) == ("89", False)
    h.configure(None, 3)
    capped = h.file_read("f.txt")
    assert capped.raw_text == "012" and capped.truncated
    assert base64.b64decode(h.file_read("f.txt", binary=True, offset=3, limit=2).content) == b"34"


def test_overwrite_is_atomic_and_keeps_the_mode(tmp_path):
    p = tmp_path / "x.sh"
    p.write_text("old")
    p.chmod(0o755)
    h.file_write("x.sh", "new")
    assert p.read_text() == "new" and p.stat().st_mode & 0o777 == 0o755
    assert [f.name for f in tmp_path.iterdir()] == ["x.sh"]  # no temp files left behind


def test_file_delete(tmp_path):
    (tmp_path / "f").write_text("x")
    assert h.file_delete("f").deleted
    (tmp_path / "d" / "in").mkdir(parents=True)
    assert not h.file_delete("d", recursive=False).deleted and (tmp_path / "d").exists()
    assert h.file_delete("d").deleted and not (tmp_path / "d").exists()
    missing = h.file_delete("nope")
    assert not missing.deleted and "does not exist" in missing.message


def test_dir_list(tmp_path):
    (tmp_path / "a").mkdir()
    (tmp_path / "a" / "x.txt").write_text("123")
    (tmp_path / "b.txt").write_text("")
    flat = {e.name: e for e in h.dir_list(".").entries}
    assert set(flat) == {"a", "b.txt"} and flat["a"].is_dir and not flat["b.txt"].is_dir
    deep = {e.name: e for e in h.dir_list(".", recursive=True).entries}
    assert deep["x.txt"].size == 3 and deep["x.txt"].mtime and deep["x.txt"].path == "a/x.txt"
    with pytest.raises(NotADirectoryError):
        h.dir_list("b.txt")


def test_file_move(tmp_path):
    (tmp_path / "a").write_text("1")
    (tmp_path / "taken").write_text("2")
    assert h.file_move("a", "new/b").moved
    assert (tmp_path / "new" / "b").read_text() == "1"
    assert not h.file_move("gone", "z").moved
    assert not h.file_move("new/b", "taken").moved and (tmp_path / "taken").read_text() == "2"


def test_read_lines(tmp_path):
    (tmp_path / "b.txt").write_text("1\n2\n3\n")
    assert h.read_lines("b.txt", 2, 3) == ["2", "3"]
    assert h.read_lines("b.txt") == ["1", "2", "3"]


def test_edit_file(tmp_path):
    p = tmp_path / "f.txt"
    p.write_text("a a b")
    with pytest.raises(ValueError, match="2 times"):  # ambiguous
        h.edit_file("f.txt", old_str="a", new_str="X")
    assert p.read_text() == "a a b"
    assert h.edit_file("f.txt", old_str="b", new_str="c") == "replaced 1 occurrence"
    assert h.edit_file("f.txt", old_str="a", new_str="z", replace_all=True) == "replaced 2 occurrences"
    assert p.read_text() == "z z c"
    h.edit_file("f.txt", new_content="q")
    assert p.read_text() == "q"
    with pytest.raises(ValueError):
        h.edit_file("f.txt", old_str="missing", new_str="x")
    with pytest.raises(ValueError):
        h.edit_file("f.txt", old_str="q")
    with pytest.raises(ValueError):
        h.edit_file("f.txt", old_str="", new_str="x")
    with pytest.raises(FileNotFoundError):
        h.edit_file("nope", new_content="x")


# --- search ----------------------------------------------------------------


def test_tree_skips_vcs_dirs(tmp_path):
    (tmp_path / "src").mkdir()
    (tmp_path / "src" / "m.py").write_text("")
    (tmp_path / ".git").mkdir()
    tree = h.tree_of_files(".")
    assert tree["src"]["__files__"] == ["m.py"] and ".git" not in tree


def test_find_files_without_git_skips_vendor_dirs(tmp_path):
    (tmp_path / "pkg").mkdir()
    (tmp_path / "pkg" / "m.py").write_text("")
    (tmp_path / "node_modules" / "x").mkdir(parents=True)
    (tmp_path / "node_modules" / "x" / "n.py").write_text("")
    (tmp_path / "n.txt").write_text("")
    assert run(h.find_files("**/*.py")) == ["pkg/m.py"]
    assert run(h.find_files("*.txt")) == ["n.txt"]
    assert run(h.find_files("*.py", limit=0)) == []


def init_repo(path):
    for cmd in (["init", "-q"], ["config", "user.email", "t@example.com"], ["config", "user.name", "T"]):
        subprocess.run(["git", *cmd], cwd=path, check=True)


def test_find_files_honours_gitignore(tmp_path):
    init_repo(tmp_path)
    (tmp_path / ".gitignore").write_text("ignored.py\n")
    (tmp_path / "ignored.py").write_text("")
    (tmp_path / "kept.py").write_text("")
    assert run(h.find_files("*.py")) == ["kept.py"]


@pytest.mark.skipif(not shutil.which("rg"), reason="ripgrep not installed")
def test_ripgrep(tmp_path):
    (tmp_path / "f.txt").write_text("alpha\nBeta\ngamma\n")
    (tmp_path / "g.py").write_text("beta\n")
    hit = run(h.ripgrep("Beta"))
    assert [(m.file, m.line_no, m.text, m.is_match) for m in hit.matches] == [("f.txt", 2, "Beta", True)]
    assert len(run(h.ripgrep("beta", ignore_case=True)).matches) == 2
    assert [m.file for m in run(h.ripgrep("beta", ignore_case=True, glob="*.py")).matches] == ["g.py"]
    ctx = run(h.ripgrep("Beta", context=1)).matches
    assert [(m.text, m.is_match) for m in ctx] == [("alpha", False), ("Beta", True), ("gamma", False)]
    capped = run(h.ripgrep("a", ignore_case=True, max_results=1))
    assert len(capped.matches) == 1 and capped.truncated
    assert run(h.ripgrep("zzz")).matches == []


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
