import pytest

from mcp_switchboard_server_harness import server as h


@pytest.fixture(autouse=True)
def root(tmp_path):
    """Every test runs confined to its own tmp_path."""
    h.configure(str(tmp_path), h.DEFAULT_MAX_OUTPUT)
    yield tmp_path
    h.configure(str(tmp_path), h.DEFAULT_MAX_OUTPUT)
