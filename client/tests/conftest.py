import os
import sys
from pathlib import Path

import pytest

# Importable without installing the package (and regardless of pytest's rootdir).
SRC = Path(__file__).resolve().parents[1] / "src"
if str(SRC) not in sys.path:
    sys.path.insert(0, str(SRC))


@pytest.fixture(autouse=True)
def isolated_env():
    """Undo every os.environ change a test makes.

    envconf.load_env_file() writes straight into os.environ, which monkeypatch
    cannot roll back, and the repo's own .env would otherwise leak into tests.
    """
    saved = os.environ.copy()
    for key in list(os.environ):
        if key.startswith("MCP_SWITCHBOARD_"):
            del os.environ[key]
    try:
        yield
    finally:
        os.environ.clear()
        os.environ.update(saved)
