"""W2 (P0-A): read_lines must be newline-faithful and agree with file_read.

The original bug joined lines with "" so a list of source lines reached the
model as one glued line. These tests pin the envelope semantics and, most
importantly, the cross-reader property: read_lines(f, 1, big).text equals
file_read(f).raw_text for every text file we can think of.
"""

import pytest

from mcp_switchboard_server_harness import server as h


def test_read_lines_preserves_newlines(root):
    p = root / "probe_nl.txt"
    p.write_text("ALPHA\nBRAVO\nCHARLIE\nDELTA\n")
    assert h.read_lines("probe_nl.txt").text == "ALPHA\nBRAVO\nCHARLIE\nDELTA\n"
    assert h.read_lines("probe_nl.txt", start=2, end=3).text == "BRAVO\nCHARLIE"


def test_read_lines_trims_nothing(root):
    p = root / "ws.txt"
    p.write_text("a   \n\tb  \n\n   \n")
    r = h.read_lines("ws.txt")
    assert r.text == "a   \n\tb  \n\n   \n"
    assert r.total_lines == 4


def test_envelope_fields(root):
    (root / "e.txt").write_text("1\n2\n3\n4\n5\n")
    r = h.read_lines("e.txt", 2, 4)
    assert (r.text, r.start, r.end, r.total_lines, r.truncated) == ("2\n3\n4", 2, 4, 5, True)
    r = h.read_lines("e.txt", 4)
    assert (r.text, r.start, r.end, r.truncated) == ("4\n5\n", 4, 5, False)
    r = h.read_lines("e.txt", 9)
    assert (r.text, r.start, r.end, r.truncated) == ("", 9, 8, False)
    with pytest.raises(ValueError):
        h.read_lines("e.txt", 3, 2)


def _write_bits(root, name, data: bytes):
    (root / name).write_bytes(data)


def test_cross_reader_agrees_with_file_read(root):
    fixtures = {
        "plain.txt": b"ALPHA\nBRAVO\nCHARLIE\nDELTA\n",
        "crlf.txt": b"one\r\ntwo\r\nthree\r\n",
        "crlf_no_final.txt": b"one\r\ntwo",
        "lone_cr.txt": b"one\rtwo\r",
        "no_trailing_newline.txt": b"x\ny",
        "blank_heavy.md": b"\n# Title\n\n\n- item\n\n\n",
        "empty.txt": b"",
        "just_newline.txt": b"\n",
        "just_newlines.txt": b"\n\n\n",
        "utf8.txt": "héllo wörld\n日本語のテキスト\nemoji 🎉\n".encode("utf-8"),
        "trailing_ws.txt": b"a  \n\tb \n",
        "mixed_eol.txt": b"a\nb\r\nc\nd\n",
        "no_trailing_ws_eof.txt": b"a\nb   ",
        "long_line.txt": b"x" * 5000 + b"\nshort\n",
    }
    for name, data in fixtures.items():
        _write_bits(root, name, data)
    for name in fixtures:
        lines = h.read_lines(name, 1, 10**9)
        whole = h.file_read(name)
        assert lines.total_lines > 0 or name in {"empty.txt"}
        assert lines.text == whole.raw_text, f"{name}: read_lines disagrees with file_read"
        assert not lines.truncated


def test_paging_pages_concatenate_verbatim(root):
    (root / "p.txt").write_text("alpha\nbeta\ngamma\ndelta\nepsilon\n")
    h.configure(str(root), 8)  # tiny cap forces several pages
    try:
        pages, start = [], 1
        while True:
            r = h.read_lines("p.txt", start=start)
            assert r.text != "" or not pages, "cap must always make progress once it starts"
            pages.append(r.text)
            if not r.truncated:
                break
            assert r.end >= start  # progress is guaranteed page to page
            start = r.end + 1
        assert "".join(pages) == "alpha\nbeta\ngamma\ndelta\nepsilon\n"
    finally:
        h.configure(str(root), h.DEFAULT_MAX_OUTPUT)


def test_cap_cut_page_ends_with_newline(root):
    (root / "c.txt").write_text("aaaa\nbbbb\ncccc\n")
    h.configure(str(root), 10)
    try:
        r = h.read_lines("c.txt")
        assert r.text == "aaaa\nbbbb\n"
        assert (r.start, r.end, r.truncated) == (1, 2, True)
    finally:
        h.configure(str(root), h.DEFAULT_MAX_OUTPUT)
