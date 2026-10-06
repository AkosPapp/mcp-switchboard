"""The documentation promises are part of the contract — pin them down.

W6 (confinement honesty) and W7 (the wake contract) shipped as text; a future
edit that quietly reintroduces "it's a sandbox" or stops telling the agent
that it can never be woken would otherwise go unnoticed. Docstrings reflow
when edited, so compare with whitespace collapsed.
"""

import inspect

from mcp_switchboard_server_harness import server as h


def norm(text: str) -> str:
    return " ".join(text.replace("*", "").split()).lower()


def doc_of(fn) -> str:
    return norm(inspect.getdoc(fn) or "")


def test_module_docstring_is_honest_about_the_boundary():
    doc = norm(h.__doc__ or "")
    assert "not a security boundary" in doc, "the not-a-security-boundary statement vanished"
    assert "not blocked" in doc, "the readable-anyway honesty vanished"
    for path in ("scratch", "temp"):
        assert path in doc, f"the writable-set statement no longer names {path}"
    assert "sandbox_proposal" in doc, "the pointer to the real-sandbox proposal vanished"


def test_process_tools_state_the_wake_contract():
    for fn in (h.process_start, h.process_read, h.process_kill):
        doc = doc_of(fn)
        assert "harness exits" in doc or "with the harness" in doc, (
            f"{fn.__name__}: death-with-harness not stated"
        )
    for fn in (h.process_start, h.process_read, h.wait_for_poll):
        doc = doc_of(fn)
        assert "push" in doc, f"{fn.__name__}: the never-pushed promise not stated"


def test_watches_state_they_cannot_wake_and_die_with_the_harness():
    for fn in (h.wait_for_start, h.wait_for_poll):
        doc = doc_of(fn)
        assert "cannot wake" in doc or "pull only" in doc or "nothing blocks" in doc, (
            f"{fn.__name__}: wake contract missing"
        )
    assert "harness exits" in doc_of(h.wait_for_poll) or "with the harness" in doc_of(h.wait_for_poll)


def test_notification_path_names_the_hubs_own_tool():
    # W7's whole point: the docstrings must redirect "notify the user" to the
    # one mechanism that actually exists.
    doc = doc_of(h.process_start) + doc_of(h.wait_for_poll)
    assert "switchboard_user_ask" in doc


def test_wait_for_start_doc_points_at_the_deadline_it_avoids():
    doc = inspect.getdoc(h.wait_for_start) or ""
    assert "deadline" in doc.lower(), "the hub-deadline motivation must stay in the description"


def test_run_description_still_flags_the_timeout_shape():
    # W3: a timed-out run_command is a result, not an error, and says so.
    doc = doc_of(h.run_command)
    assert "timed_out" in doc or "partial" in doc, "the partial-output guarantee vanished"
