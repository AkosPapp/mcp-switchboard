"""The hub and the client each carry their own copy of two shared modules.

They are duplicated on purpose: the client must stay dependency-light (no mcp,
no fastapi) and must be installable on its own, so it cannot import from the hub
package. The cost of that choice is drift, which this test exists to prevent.
"""

from pathlib import Path

import pytest

SHARED = ["protocol.py", "envconf.py"]


def repo_root() -> Path:
    here = Path(__file__).resolve()
    for candidate in here.parents:
        if (candidate / "hub").is_dir() and (candidate / "client").is_dir():
            return candidate
    pytest.skip("not running from a source checkout")


@pytest.mark.parametrize("filename", SHARED)
def test_shared_modules_are_identical(filename: str) -> None:
    root = repo_root()
    hub_copy = root / "hub" / "src" / "mcp_switchboard_hub" / filename
    client_copy = root / "client" / "src" / "mcp_switchboard_client" / filename

    assert hub_copy.read_text() == client_copy.read_text(), (
        f"{filename} has drifted between the hub and the client. These files are "
        f"deliberate copies; edit one and copy it to the other:\n"
        f"  cp {client_copy} {hub_copy}"
    )
