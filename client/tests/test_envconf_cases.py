"""envconf is checked against docs/envconf-cases.json, the fixture the Go hub also runs.

envconf.py stays Python-only; the hub reimplements the same two rules in Go
(spec.md P3). This shared table is what stops the two implementations from
disagreeing about, say, whether an empty file substitutes to an empty string
(spec.md V4).
"""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import pytest

from mcp_switchboard_client import envconf


def fixture() -> dict:
    here = Path(__file__).resolve()
    for candidate in here.parents:
        path = candidate / "docs" / "envconf-cases.json"
        if path.is_file():
            return json.loads(path.read_text(encoding="utf-8"))
    pytest.skip("not running from a source checkout")


FIXTURE = fixture()


@pytest.fixture(scope="module")
def sandbox(tmp_path_factory: pytest.TempPathFactory) -> Path:
    root = tmp_path_factory.mktemp("envconf")
    for name, content in FIXTURE["files"].items():
        (root / name).write_text(content, encoding="utf-8")
    for name in FIXTURE["dirs"]:
        (root / name).mkdir()
    return root


def expand(value: Any, root: Path) -> Any:
    return value.replace("{{dir}}", str(root)) if isinstance(value, str) else value


def test_prefix_matches_the_fixture() -> None:
    assert envconf.PREFIX == FIXTURE["prefix"]


@pytest.mark.parametrize("case", FIXTURE["cases"], ids=lambda c: c["name"])
def test_case(case: dict, sandbox: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    for key in list(__import__("os").environ):
        if key.startswith(FIXTURE["prefix"]):
            monkeypatch.delenv(key, raising=False)
    for key, value in case["env"].items():
        monkeypatch.setenv(key, expand(value, sandbox))

    spec = case["get"]
    getter = {
        "str": lambda: envconf.get(
            spec["name"], spec.get("default"), secret=spec.get("secret", False)
        ),
        "int": lambda: envconf.get_int(spec["name"], spec["default"]),
        "float": lambda: envconf.get_float(spec["name"], spec["default"]),
        "bool": lambda: envconf.get_bool(spec["name"], spec["default"]),
    }[spec["kind"]]

    expected = case["expect"]
    if isinstance(expected, dict) and expected.get("error"):
        with pytest.raises(envconf.ConfigError):
            getter()
    else:
        assert getter() == expand(expected, sandbox)
