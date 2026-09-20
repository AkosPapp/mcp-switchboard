"""Metrics render the series the dashboards and alerts are written against."""

from __future__ import annotations

import prometheus_client
import pytest
from prometheus_client import CollectorRegistry

from mcp_switchboard_hub.metrics import SERVER_STATES, Metrics


@pytest.fixture
def metrics() -> Metrics:
    return Metrics()


def render_text(m: Metrics) -> str:
    payload, content_type = m.render()
    assert isinstance(payload, bytes)
    assert "text/plain" in content_type
    return payload.decode("utf-8")


def test_render_returns_prometheus_exposition(metrics):
    text = render_text(metrics)
    assert "# HELP mcpsb_connections_active" in text
    assert "# TYPE mcpsb_connections_active gauge" in text


def test_fresh_metrics_expose_zeroed_fixed_series(metrics):
    text = render_text(metrics)
    assert "mcpsb_connections_active 0.0" in text
    assert "mcpsb_loki_dropped_total 0.0" in text
    for state in SERVER_STATES:
        assert f'mcpsb_servers{{state="{state}"}} 0.0' in text
    assert 'mcpsb_tunnel_frames_total{direction="in"} 0.0' in text
    assert 'mcpsb_tunnel_frames_total{direction="out"} 0.0' in text


def test_observe_call_counts_and_times(metrics):
    metrics.observe_call(
        label="legion5",
        server="git",
        tool="git_status",
        status="ok",
        source="mcp",
        duration_seconds=0.3,
    )
    metrics.observe_call(
        label="legion5",
        server="git",
        tool="git_status",
        status="ok",
        source="mcp",
        duration_seconds=1.7,
    )
    metrics.observe_call(
        label="legion5",
        server="git",
        tool="git_status",
        status="error",
        source="console",
        duration_seconds=0.05,
    )

    call_labels = {
        "label": "legion5",
        "server": "git",
        "tool": "git_status",
        "status": "ok",
        "source": "mcp",
    }
    assert metrics.registry.get_sample_value("mcpsb_tool_calls_total", call_labels) == 2.0
    assert (
        metrics.registry.get_sample_value(
            "mcpsb_tool_calls_total", {**call_labels, "status": "error", "source": "console"}
        )
        == 1.0
    )

    hist_labels = {"label": "legion5", "server": "git", "tool": "git_status"}
    assert (
        metrics.registry.get_sample_value("mcpsb_tool_call_duration_seconds_count", hist_labels)
        == 3.0
    )
    assert metrics.registry.get_sample_value(
        "mcpsb_tool_call_duration_seconds_sum", hist_labels
    ) == pytest.approx(2.05)
    # 0.3 and 0.05 land at or below 0.5s; 1.7 does not.
    assert (
        metrics.registry.get_sample_value(
            "mcpsb_tool_call_duration_seconds_bucket", {**hist_labels, "le": "0.5"}
        )
        == 2.0
    )
    assert (
        metrics.registry.get_sample_value(
            "mcpsb_tool_call_duration_seconds_bucket", {**hist_labels, "le": "+Inf"}
        )
        == 3.0
    )

    text = render_text(metrics)
    assert (
        'mcpsb_tool_calls_total{label="legion5",server="git",source="mcp",'
        'status="ok",tool="git_status"} 2.0' in text
    )


def test_negative_duration_is_clamped(metrics):
    metrics.observe_call(
        label="l", server="s", tool="t", status="ok", source="api", duration_seconds=-5.0
    )
    assert metrics.registry.get_sample_value(
        "mcpsb_tool_call_duration_seconds_sum", {"label": "l", "server": "s", "tool": "t"}
    ) == pytest.approx(0.0)


def test_set_connections(metrics):
    metrics.set_connections(3)
    assert metrics.registry.get_sample_value("mcpsb_connections_active") == 3.0
    metrics.set_connections(0)
    assert metrics.registry.get_sample_value("mcpsb_connections_active") == 0.0


def test_set_servers_zeroes_states_that_emptied(metrics):
    metrics.set_servers({"running": 4, "failed": 1})
    assert metrics.registry.get_sample_value("mcpsb_servers", {"state": "running"}) == 4.0
    assert metrics.registry.get_sample_value("mcpsb_servers", {"state": "failed"}) == 1.0
    assert metrics.registry.get_sample_value("mcpsb_servers", {"state": "starting"}) == 0.0
    assert metrics.registry.get_sample_value("mcpsb_servers", {"state": "exited"}) == 0.0

    metrics.set_servers({"starting": 2})
    assert metrics.registry.get_sample_value("mcpsb_servers", {"state": "running"}) == 0.0
    assert metrics.registry.get_sample_value("mcpsb_servers", {"state": "starting"}) == 2.0


def test_set_and_clear_tools(metrics):
    metrics.set_tools("legion5", "git", 7)
    metrics.set_tools("nas", "fs", 3)
    text = render_text(metrics)
    assert 'mcpsb_tools_total{label="legion5",server="git"} 7.0' in text
    assert 'mcpsb_tools_total{label="nas",server="fs"} 3.0' in text

    metrics.clear_tools("legion5", "git")
    text = render_text(metrics)
    assert 'label="legion5"' not in text
    assert 'mcpsb_tools_total{label="nas",server="fs"} 3.0' in text
    assert (
        metrics.registry.get_sample_value("mcpsb_tools_total", {"label": "legion5", "server": "git"})
        is None
    )


def test_clear_tools_is_idempotent(metrics):
    metrics.clear_tools("never", "seen")
    metrics.set_tools("legion5", "git", 1)
    metrics.clear_tools("legion5", "git")
    metrics.clear_tools("legion5", "git")


def test_count_frame(metrics):
    metrics.count_frame("in")
    metrics.count_frame("in")
    metrics.count_frame("out")
    assert metrics.registry.get_sample_value(
        "mcpsb_tunnel_frames_total", {"direction": "in"}
    ) == 2.0
    assert metrics.registry.get_sample_value(
        "mcpsb_tunnel_frames_total", {"direction": "out"}
    ) == 1.0


def test_count_frame_rejects_unbounded_directions(metrics):
    with pytest.raises(ValueError):
        metrics.count_frame("sideways")


def test_count_loki_dropped(metrics):
    metrics.count_loki_dropped()
    metrics.count_loki_dropped(9)
    metrics.count_loki_dropped(0)
    assert metrics.registry.get_sample_value("mcpsb_loki_dropped_total") == 10.0


def test_instances_do_not_collide_or_touch_the_global_registry():
    first = Metrics()
    second = Metrics()  # would raise Duplicated timeseries on a shared registry

    first.set_connections(1)
    second.set_connections(9)
    assert first.registry.get_sample_value("mcpsb_connections_active") == 1.0
    assert second.registry.get_sample_value("mcpsb_connections_active") == 9.0
    assert first.registry is not second.registry

    global_text = prometheus_client.generate_latest(prometheus_client.REGISTRY).decode()
    assert "mcpsb_" not in global_text


def test_explicit_registry_is_used():
    registry = CollectorRegistry()
    metrics = Metrics(registry)
    metrics.set_connections(2)
    assert metrics.registry is registry
    assert registry.get_sample_value("mcpsb_connections_active") == 2.0


def test_all_contracted_metric_names_are_present(metrics):
    metrics.observe_call(
        label="l", server="s", tool="t", status="ok", source="api", duration_seconds=0.1
    )
    metrics.set_tools("l", "s", 1)
    text = render_text(metrics)
    for name in (
        "mcpsb_connections_active",
        "mcpsb_servers",
        "mcpsb_tools_total",
        "mcpsb_tool_calls_total",
        "mcpsb_tool_call_duration_seconds_bucket",
        "mcpsb_tunnel_frames_total",
        "mcpsb_loki_dropped_total",
    ):
        assert name in text
