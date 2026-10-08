"""apply_patch, edit diffs, run_tests, big-output paging."""

import asyncio
import json
import os
import stat
import subprocess
import textwrap

import pytest

from mcp_switchboard_server_harness import server as h


def run(coro):
    return asyncio.run(coro)


# --- edit ----------------------------------------------------------------------


def test_edit_returns_a_diff_and_dry_run_does_not_write(tmp_path):
    (tmp_path / "f.txt").write_text("one\ntwo\nthree\n")
    out = h.edit("f.txt", old_string="two", new_string="2", dry_run=True)
    assert out.startswith("dry run: would replace 1 occurrence")
    assert "-two" in out and "+2" in out
    assert (tmp_path / "f.txt").read_text() == "one\ntwo\nthree\n"
    out = h.edit("f.txt", old_string="two", new_string="2")
    assert "+2" in out and (tmp_path / "f.txt").read_text() == "one\n2\nthree\n"


def test_edit_diff_is_capped(tmp_path):
    (tmp_path / "f.txt").write_text("".join(f"line {i}\n" for i in range(2000)))
    out = h.edit("f.txt", old_string="line ", new_string="LINE ", replace_all=True, dry_run=True)
    assert "[diff truncated]" in out and len(out) < h.DIFF_CAP + 200


# --- apply_patch -----------------------------------------------------------------

MODIFY = """\
diff --git a/f.txt b/f.txt
--- a/f.txt
+++ b/f.txt
@@ -1,3 +1,3 @@
 one
-two
+TWO
 three
"""


def test_apply_patch_modify_with_dry_run(tmp_path):
    (tmp_path / "f.txt").write_text("one\ntwo\nthree\n")
    r = h.apply_patch(MODIFY, dry_run=True)
    assert not r.applied and r.files[0].action == "modify" and (r.files[0].added, r.files[0].removed) == (1, 1)
    assert (tmp_path / "f.txt").read_text() == "one\ntwo\nthree\n"
    assert h.apply_patch(MODIFY).applied
    assert (tmp_path / "f.txt").read_text() == "one\nTWO\nthree\n"


def test_apply_patch_tolerates_line_offsets(tmp_path):
    (tmp_path / "f.txt").write_text("x\ny\nz\none\ntwo\nthree\n")
    h.apply_patch(MODIFY)
    assert (tmp_path / "f.txt").read_text() == "x\ny\nz\none\nTWO\nthree\n"


def test_apply_patch_multi_file_create_delete_rename(tmp_path):
    (tmp_path / "old.txt").write_text("bye\n")
    (tmp_path / "keep.txt").write_text("a\nb\n")
    (tmp_path / "mv.txt").write_text("m\n")
    patch = textwrap.dedent("""\
        diff --git a/new.sh b/new.sh
        new file mode 100755
        --- /dev/null
        +++ b/new.sh
        @@ -0,0 +1,2 @@
        +#!/bin/sh
        +echo hi
        diff --git a/old.txt b/old.txt
        deleted file mode 100644
        --- a/old.txt
        +++ /dev/null
        @@ -1 +0,0 @@
        -bye
        diff --git a/keep.txt b/keep.txt
        --- a/keep.txt
        +++ b/keep.txt
        @@ -1,2 +1,3 @@
         a
        +inserted
         b
        diff --git a/mv.txt b/moved/mv2.txt
        similarity index 100%
        rename from mv.txt
        rename to moved/mv2.txt
        """)
    r = h.apply_patch(patch)
    assert [(f.path, f.action) for f in r.files] == [
        ("new.sh", "create"), ("old.txt", "delete"), ("keep.txt", "modify"), ("moved/mv2.txt", "rename")]
    assert (tmp_path / "new.sh").read_text() == "#!/bin/sh\necho hi\n"
    assert os.stat(tmp_path / "new.sh").st_mode & stat.S_IXUSR
    assert not (tmp_path / "old.txt").exists() and not (tmp_path / "mv.txt").exists()
    assert (tmp_path / "keep.txt").read_text() == "a\ninserted\nb\n"
    assert (tmp_path / "moved" / "mv2.txt").read_text() == "m\n"


def test_apply_patch_failure_names_the_hunk_and_writes_nothing(tmp_path):
    (tmp_path / "a.txt").write_text("1\n2\n")
    (tmp_path / "b.txt").write_text("nothing like it\n")
    patch = ("--- a/a.txt\n+++ b/a.txt\n@@ -1,2 +1,2 @@\n 1\n-2\n+two\n"
             "--- a/b.txt\n+++ b/b.txt\n@@ -1,2 +1,2 @@\n one\n-two\n+2\n")
    with pytest.raises(ValueError, match=r"b\.txt: hunk 1 .*does not apply") as e:
        h.apply_patch(patch)
    assert "| two" in str(e.value) and "nothing like it" in str(e.value)
    assert (tmp_path / "a.txt").read_text() == "1\n2\n"  # all-or-nothing


def test_apply_patch_no_newline_marker(tmp_path):
    (tmp_path / "f").write_text("a\nb")
    h.apply_patch("--- a/f\n+++ b/f\n@@ -1,2 +1,2 @@\n a\n-b\n\\ No newline at end of file\n+c\n\\ No newline at end of file\n")
    assert (tmp_path / "f").read_text() == "a\nc"


def test_apply_patch_refuses_paths_outside_the_root(tmp_path):
    inner = tmp_path / "root"
    inner.mkdir()
    (tmp_path / "outside.txt").write_text("x\n")
    (inner / "link").symlink_to(tmp_path / "outside.txt")
    h.configure(str(inner))
    for name in ["../outside.txt", str(tmp_path / "outside.txt"), "a/../../outside.txt", "link"]:
        patch = f"--- a/{name}\n+++ b/{name}\n@@ -1 +1 @@\n-x\n+y\n"
        if name.startswith("/"):
            patch = f"--- {name}\n+++ {name}\n@@ -1 +1 @@\n-x\n+y\n"
        with pytest.raises(PermissionError):
            h.apply_patch(patch)
    assert (tmp_path / "outside.txt").read_text() == "x\n"


def test_apply_patch_rejects_garbage_and_existing_creates(tmp_path):
    with pytest.raises(ValueError):
        h.apply_patch("just some text")
    (tmp_path / "e.txt").write_text("hi\n")
    with pytest.raises(ValueError, match="already exists"):
        h.apply_patch("--- /dev/null\n+++ b/e.txt\n@@ -0,0 +1 @@\n+x\n")
    with pytest.raises(ValueError, match="shorter than its header"):
        h.apply_patch("--- a/e.txt\n+++ b/e.txt\n@@ -1,5 +1,5 @@\n hi\n")


# --- run_tests -----------------------------------------------------------------------


def _python_has_pytest():
    """Whether the plain `python` on PATH can import pytest.

    These tests hand that exact interpreter to the tool under test (rather
    than an auto-detected one), so where the ambient venv keeps pytest on a
    different binary they skip - like the go end-to-end test below skips
    without go. Failing here would test the shell, not the tool.
    """
    return subprocess.run(["python", "-c", "import pytest"], capture_output=True).returncode == 0


def test_run_tests_pytest(tmp_path):
    (tmp_path / "pytest.ini").write_text("[pytest]\n")
    (tmp_path / "test_x.py").write_text(
        "import pytest\n\ndef test_ok():\n    assert True\n\ndef test_bad():\n    assert 1 == 2, 'nope'\n\n"
        "@pytest.mark.skip\ndef test_skip():\n    pass\n"
    )
    r = run(h.run_tests())
    assert (r.framework, r.ok, r.passed, r.failed, r.skipped) == ("pytest", False, 1, 1, 1)
    assert r.exit_code == 1 and len(r.failures) == 1
    f = r.failures[0]
    assert f.name == "test_x.py::test_bad" and f.file == "test_x.py" and f.line == 7 and "nope" in f.message
    assert "1 failed" in r.output_tail
    (tmp_path / "test_x.py").write_text("def test_ok():\n    assert True\n")
    r = run(h.run_tests(command="pytest -q"))
    assert r.ok and r.passed == 1 and r.failures == []


def test_run_tests_pytest_default_tb_fallback(tmp_path):
    if not _python_has_pytest():
        pytest.skip("plain `python` on PATH has no pytest")
    (tmp_path / "test_y.py").write_text("def test_bad():\n    x = 1\n    assert x == 2\n")
    r = run(h.run_tests(command="python -m pytest -q --tb=short -rN"))
    assert r.framework == "pytest" and r.failed == 1
    assert r.failures[0].name == "test_bad" and r.failures[0].line == 3 and "assert 1 == 2" in r.failures[0].message


def test_run_tests_go_json_parsing():
    events = [
        {"Action": "run", "Package": "p", "Test": "TestA"},
        {"Action": "output", "Package": "p", "Test": "TestA", "Output": "    a_test.go:12: got 1, want 2\n"},
        {"Action": "fail", "Package": "p", "Test": "TestA"},
        {"Action": "pass", "Package": "p", "Test": "TestB"},
        {"Action": "skip", "Package": "p", "Test": "TestC"},
        {"Action": "fail", "Package": "p"},
        {"Action": "output", "Package": "q", "Output": "q/x.go:3:1: undefined: foo\n"},
        {"Action": "fail", "Package": "q"},
    ]
    passed, failed, skipped, failures, _ = h._parse_go_json("\n".join(json.dumps(e) for e in events))
    assert (passed, failed, skipped) == (1, 2, 1)
    assert (failures[0].name, failures[0].file, failures[0].line, failures[0].message) == ("p.TestA", "a_test.go", 12, "got 1, want 2")
    assert (failures[1].name, failures[1].file, failures[1].line) == ("q", "q/x.go", 3)


def test_run_tests_go_end_to_end(tmp_path):
    import shutil

    if not shutil.which("go"):
        pytest.skip("go not installed")
    (tmp_path / "go.mod").write_text("module example.com/t\n\ngo 1.20\n")
    (tmp_path / "a_test.go").write_text('package t\nimport "testing"\nfunc TestOK(t *testing.T) {}\nfunc TestBad(t *testing.T) { t.Errorf("boom") }\n')
    r = run(h.run_tests(timeout=120))
    assert r.framework == "go" and not r.ok and r.passed == 1 and r.failed == 1
    assert r.failures[0].file == "a_test.go" and r.failures[0].line == 4 and r.failures[0].message == "boom"


def test_run_tests_cargo_parsing_and_generic(tmp_path):
    text = ("running 2 tests\ntest a ... ok\ntest b ... FAILED\n\nfailures:\n\n---- b stdout ----\n"
            "thread 'b' panicked at src/lib.rs:9:5:\nassertion failed\n\nfailures:\n    b\n\n"
            "test result: FAILED. 1 passed; 1 failed; 0 ignored; 0 measured\n")
    passed, failed, skipped, failures = h._parse_cargo(text)
    assert (passed, failed, skipped) == (1, 1, 0)
    assert (failures[0].name, failures[0].file, failures[0].line, failures[0].message) == ("b", "src/lib.rs", 9, "assertion failed")
    assert h._parse_generic("Tests:       1 failed, 2 passed, 3 total") == (2, 1, 0)
    (tmp_path / "package.json").write_text('{"scripts": {"test": "echo 3 passing; echo 1 failing; exit 1"}}')
    r = run(h.run_tests())
    assert r.framework == "npm" and not r.ok and (r.passed, r.failed) == (3, 1)


def test_run_tests_needs_something_to_run(tmp_path):
    with pytest.raises(ValueError, match="no test framework detected"):
        run(h.run_tests())
    r = run(h.run_tests(command="echo hi; exit 3"))
    assert r.framework == "custom" and r.exit_code == 3 and not r.ok and "hi" in r.output_tail


# --- big output ------------------------------------------------------------------------


def test_bash_modes(tmp_path):
    seq = "seq 1 300"
    head = run(h.bash(seq, mode="head"))
    assert head.stdout.splitlines() == [str(i) for i in range(1, 101)] and head.truncated and head.output_id
    tail = run(h.bash(seq, mode="tail"))
    assert tail.stdout.splitlines()[0] == "201" and tail.stdout.splitlines()[-1] == "300"
    g = run(h.bash(seq, mode="grep", pattern="^15$"))
    assert g.stdout.splitlines() == ["13-13", "14-14", "15:15", "16-16", "17-17"]
    full = run(h.bash("echo hi", mode="full"))
    assert full.stdout == "hi\n" and full.output_id is None and not full.truncated
    with pytest.raises(ValueError):
        run(h.bash("echo", mode="grep"))
    with pytest.raises(ValueError):
        run(h.bash("echo", mode="bogus"))
    with pytest.raises(ValueError, match="invalid regex"):
        run(h.bash("echo", mode="grep", pattern="("))


def test_overflow_is_stored_and_pageable():
    h.configure(max_output=50)
    r = run(h.bash("seq 1 100; echo oops >&2"))
    assert r.truncated and r.output_id and len(r.stdout) == 50
    first = h.output_read(r.output_id, limit=20)
    assert first.text == r.stdout[:20] and first.more and first.next_offset == 20
    rest = h.output_read(r.output_id, offset=first.next_offset, limit=1000)
    assert (first.text + rest.text).startswith(r.stdout[:50]) and len(rest.text) == 50
    lines = h.output_read(r.output_id, mode="lines", offset=10, limit=5)
    assert lines.text == "11\n12\n13\n14\n15\n" and lines.next_offset == 15 and lines.total == 100
    assert h.output_read(r.output_id, stream="stderr").text == "oops\n"
    g = h.output_grep(r.output_id, r"^5\d$", context=1, max_results=3)
    assert [m.text for m in g.matches if m.is_match] == ["50", "51", "52"] and g.truncated and g.total_lines == 100
    assert [m.line_no for m in g.matches][:2] == [49, 50]
    with pytest.raises(ValueError, match="unknown output_id"):
        h.output_read("nope")
    with pytest.raises(ValueError):
        h.output_read(r.output_id, mode="bytes")
    with pytest.raises(ValueError):
        h.output_read(r.output_id, stream="both")


def test_git_overflow_gives_output_id(tmp_path):
    h.configure(max_output=30)
    (tmp_path / "g.txt").write_text("l\n" * 100)
    subprocess.run(["git", "init", "-q"], cwd=tmp_path, check=True)
    subprocess.run(["git", "add", "."], cwd=tmp_path, check=True)
    out = run(h.git_diff(staged=True))
    assert "output_read(output_id=" in out
    oid = out.split('output_id="')[1].split('"')[0]
    assert "diff --git" in h.output_read(oid).text


def test_output_store_is_an_lru(monkeypatch):
    ids = [h._store_output(f"out{i}") for i in range(h._OUTPUT_MAX_ENTRIES + 3)]
    assert len(h._outputs) == h._OUTPUT_MAX_ENTRIES
    assert ids[0] not in h._outputs and ids[-1] in h._outputs
    monkeypatch.setattr(h, "_OUTPUT_MAX_CHARS", 100)
    h._outputs.clear()
    a = h._store_output("a" * 40)
    b = h._store_output("b" * 40)
    h.output_read(a)  # touch a: b is now the oldest
    c = h._store_output("c" * 40)
    assert a in h._outputs and c in h._outputs and b not in h._outputs


# --- registration -----------------------------------------------------------------------------


def test_new_tools_are_registered_with_examples_and_annotations():
    tools = {fn.__name__: (fn, a) for fn, a in h.TOOLS}
    for name in ["apply_patch", "edit", "grep", "glob", "read", "write", "run_tests",
                 "output_read", "output_grep", "bash", "git_diff"]:
        assert "Example:" in tools[name][0].__doc__, name
        assert (tools[name][0].__doc__.strip().rsplit("Example:", 1)[1]).strip(), name
    for name in ["read", "glob", "grep", "output_read", "output_grep"]:
        assert tools[name][1].read_only_hint is True
    for name in ["apply_patch", "edit"]:
        a = tools[name][1]
        assert (a.read_only_hint, a.destructive_hint, a.idempotent_hint, a.open_world_hint) == (False, True, False, False)
    assert tools["run_tests"][1].open_world_hint is True


# --- W10: push guards --------------------------------------------------------


def _push_repo(tmp_path, branch="main"):
    origin = tmp_path / "origin"
    subprocess.run(["git", "init", "--bare", str(origin)], check=True, capture_output=True)
    work = tmp_path / "work"
    subprocess.run(["git", "clone", str(origin), str(work)], check=True, capture_output=True)
    subprocess.run(
        ["git", "-C", str(work), "checkout", "-B", branch], check=True, capture_output=True)
    (work / "f.txt").write_text("a\n")
    for args in (["config", "user.email", "t@t"], ["config", "user.name", "t"],
                 ["add", "f.txt"], ["commit", "-m", "x"], ["push", "-u", "origin", branch]):
        subprocess.run(["git", "-C", str(work), *args], check=True, capture_output=True)
    # local bare origin only: no network, no real remote (W10 tests never push anywhere live)
    (work / "g.txt").write_text("b\n")
    for args in (["add", "g.txt"], ["commit", "-m", "y"]):
        subprocess.run(["git", "-C", str(work), *args], check=True, capture_output=True)
    return work, origin


def test_git_push_to_protected_branch_is_flagged(tmp_path):
    work, _origin = _push_repo(tmp_path, "main")
    h.configure(str(work))
    out = run(h.git_push())
    assert "protected branch 'main'" in out


def test_git_push_to_feature_branch_is_quiet(tmp_path):
    work, _origin = _push_repo(tmp_path, "feature")
    h.configure(str(work))
    out = run(h.git_push())
    assert "protected" not in out


def test_git_push_declares_itself_irreversible():
    tools = {t.name: t for t in run(h.build_server().list_tools())}
    meta = tools["git_push"].meta or {}
    sw = meta.get("switchboard", {})
    assert sw.get("irreversible") is True
    assert "cannot be undone" in sw.get("irreversibleReason", "")
    # other tools carry no such flag
    assert not (tools["write"].meta or {}).get("switchboard")

# ---------- grep fallback and run_tests changed_only (capability pass) ----


def test_grep_falls_back_to_python_scanner(tmp_path, monkeypatch):
    (tmp_path / "a.py").write_text("import os\n\ndef main():\n    pass\n")
    (tmp_path / "keep.txt").write_text("main here\n")
    monkeypatch.setattr(h.shutil, "which", lambda name: None)
    r = run(h.grep("def \\w+", include="*.py", context=1))
    texts = [(m.file, m.line_no) for m in r.matches]
    assert ("a.py", 3) in texts
    assert all(f.endswith(".py") for f, _ in texts)  # include honored by the scanner too
    ctx = [m for m in r.matches if not m.is_match]
    assert any(m.line_no == 2 or m.line_no == 4 for m in ctx)


def test_grep_fallback_reports_bad_regex(tmp_path, monkeypatch):
    monkeypatch.setattr(h.shutil, "which", lambda name: None)
    with pytest.raises(RuntimeError, match="regex"):
        run(h.grep("def("))


def _git(tmp_path, *args):
    subprocess.run(["git", *args], cwd=tmp_path, check=True, capture_output=True)


def test_run_tests_changed_only_scopes_to_dirty_packages(tmp_path, monkeypatch):
    if not _python_has_pytest():
        pytest.skip("plain `python` on PATH has no pytest")
    monkeypatch.setenv("GIT_CONFIG_GLOBAL", str(tmp_path / ".gitconfig"))
    (tmp_path / "pyproject.toml").write_text("[project]\nname='x'\nversion='0'\n")
    (tmp_path / "tests").mkdir()
    (tmp_path / "tests" / "test_a.py").write_text("def test_a():\n    assert True\n")
    (tmp_path / "pkg").mkdir()
    (tmp_path / "pkg" / "impl.py").write_text("X = 1\n")
    _git(tmp_path, "init", "-q", ".")
    _git(tmp_path, "config", "user.email", "t@t")
    _git(tmp_path, "config", "user.name", "t")
    _git(tmp_path, "add", "-A")
    _git(tmp_path, "commit", "-qm", "base")
    (tmp_path / "pkg" / "impl.py").write_text("X = 2\n")

    targets = h._changed_targets(tmp_path, run(h._dirty_files(tmp_path)))
    assert [fw for fw, _, _ in targets] == ["pytest"], targets
    r = run(h.run_tests(changed_only=True))
    assert r.framework == "changed" and r.ok and r.passed == 1 and r.exit_code == 0


def test_run_tests_changed_only_maps_go_packages(tmp_path):
    (tmp_path / "go.mod").write_text("module m\n\ngo 1.19\n")
    (tmp_path / "a").mkdir()
    (tmp_path / "a" / "a.go").write_text("package a\n\nfunc N() int { return 1 }\n")
    (tmp_path / "a" / "a_test.go").write_text("package a\n\nimport \"testing\"\n\nfunc TestN(t *testing.T) {\n\tif N() != 2 { t.Fatal(\"want 2\") }\n}\n")
    targets = h._changed_targets(tmp_path, [str(p.relative_to(h._root)) for p in [(tmp_path / "a" / "a.go")]])
    assert [fw for fw, _, _ in targets] == ["go"]
    assert targets[0][2][-1] == "./a"


def test_run_tests_changed_only_falls_back_when_nothing_maps(tmp_path):
    (tmp_path / "notes.txt").write_text("hello\n")
    targets = h._changed_targets(tmp_path, [str((tmp_path / "notes.txt").relative_to(h._root))])
    assert targets == []
