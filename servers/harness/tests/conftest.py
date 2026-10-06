import tempfile

import pytest

from mcp_switchboard_server_harness import server as h


@pytest.fixture(autouse=True)
def root(tmp_path):
    """Every test runs confined to its own tmp_path."""
    h.configure(str(tmp_path), h.DEFAULT_MAX_OUTPUT)
    yield tmp_path
    h.configure(str(tmp_path), h.DEFAULT_MAX_OUTPUT)


@pytest.fixture(autouse=True)
def temp_inside_root(tmp_path, monkeypatch):
    """Keep the file tools' temp-dir allowance away from the real /tmp.

    pytest's tmp_path lives under /tmp itself, so without this the confinement
    tests that point at sibling tmp_path files would be allowed through the
    temp-dir rule. Tests for that rule set a temp dir of their own.
    """
    monkeypatch.setattr(tempfile, "tempdir", str(tmp_path / "_temp"))  # need not exist
