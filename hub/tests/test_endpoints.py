"""The console's Endpoints panel: URL reference rows and the install command."""

from __future__ import annotations

from mcp_switchboard_hub.config import Settings
from mcp_switchboard_hub.endpoints import build_endpoints_info, build_install_command


def make_settings(**overrides):
    return Settings(tunnel_token="t0k3n", **overrides)


EMPTY_SNAPSHOT = {"connections": []}

ONE_CONNECTION_SNAPSHOT = {
    "connections": [
        {
            "id": "c1",
            "label": "legion5",
            "servers": [
                {
                    "name": "git",
                    "project": None,
                    "tools": [{"name": "git_status", "exposedName": "legion5__git__git_status"}],
                },
                {
                    "name": "lsp",
                    "project": "nix",
                    "tools": [{"name": "hover", "exposedName": "legion5__nix__lsp__hover"}],
                },
            ],
        }
    ]
}


def test_install_command_is_none_without_public_url():
    assert build_install_command(make_settings()) is None


def test_install_command_includes_hub_url_and_token():
    settings = make_settings(public_url="https://hp.example.ts.net:8443")
    command = build_install_command(settings)
    assert "--hub-url https://hp.example.ts.net:8443" in command
    assert "--token t0k3n" in command


def test_local_base_url_defaults_to_loopback_and_private_port():
    settings = make_settings(private_port=9001)
    info = build_endpoints_info(settings, EMPTY_SNAPSHOT)
    assert info["localBaseUrl"] == "http://127.0.0.1:9001"


def test_local_base_url_is_overridable():
    settings = make_settings(local_base_url="http://switchboard.local:8099")
    info = build_endpoints_info(settings, EMPTY_SNAPSHOT)
    assert info["localBaseUrl"] == "http://switchboard.local:8099"


def test_empty_snapshot_still_yields_reference_rows():
    info = build_endpoints_info(make_settings(), EMPTY_SNAPSHOT)
    paths = [row["path"] for row in info["rows"]]
    assert "/mcp" in paths
    assert any(p.startswith("/mcp/host/") for p in paths)


def test_rows_reflect_real_connections_and_projects():
    info = build_endpoints_info(make_settings(), ONE_CONNECTION_SNAPSHOT)
    by_path = {row["path"]: row for row in info["rows"]}

    assert by_path["/mcp"]["example"] == "legion5__git__git_status"
    assert by_path["/mcp/host/legion5"]["example"] in ("git__git_status", "nix__lsp__hover")
    assert by_path["/mcp/host/legion5/project/nix"]["example"] == "lsp__hover"
    assert by_path["/mcp/host/legion5/project/nix/server/lsp"]["example"] == "hover"
    assert by_path["/mcp/host/legion5/server/git"]["example"] == "git_status"
    # A projected server must not also get a project-less server row.
    assert "/mcp/host/legion5/server/lsp" not in by_path


def test_rows_deduplicate_across_multiple_servers_in_one_project():
    snapshot = {
        "connections": [
            {
                "id": "c1",
                "label": "legion5",
                "servers": [
                    {"name": "lsp", "project": "nix", "tools": [{"name": "hover", "exposedName": "x"}]},
                    {"name": "fmt", "project": "nix", "tools": [{"name": "run", "exposedName": "y"}]},
                ],
            }
        ]
    }
    info = build_endpoints_info(make_settings(), snapshot)
    project_rows = [r for r in info["rows"] if r["path"] == "/mcp/host/legion5/project/nix"]
    assert len(project_rows) == 1
