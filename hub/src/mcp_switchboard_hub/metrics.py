"""Prometheus instrumentation for the hub.

Everything lives on a registry owned by the Metrics instance, never on
prometheus_client's global default. Two hubs (or a hub and its test suite) can
therefore coexist in one process without duplicate-timeseries errors, and a
test can assert on exactly the series it produced.

Cardinality is the thing to watch: label values here are machine labels,
server names and tool names, all of which come from configuration and are
bounded. Never add call arguments, connection ids or error text as labels.
"""

from __future__ import annotations

from prometheus_client import (
    CONTENT_TYPE_LATEST,
    CollectorRegistry,
    Counter,
    Gauge,
    Histogram,
    generate_latest,
)

# Matches protocol.STATE_* - duplicated rather than imported so this module
# stays importable on its own.
SERVER_STATES = ("starting", "running", "exited", "failed")

FRAME_DIRECTIONS = ("in", "out")

# Tool calls run from a few milliseconds (a local git status) to tens of
# seconds (a slow network fetch); the last finite bucket is the timeout range.
DURATION_BUCKETS = (
    0.01,
    0.025,
    0.05,
    0.1,
    0.25,
    0.5,
    1.0,
    2.5,
    5.0,
    10.0,
    20.0,
    30.0,
    60.0,
    float("inf"),
)


class Metrics:
    """All hub metrics, prefixed mcpsb_."""

    def __init__(self, registry: CollectorRegistry | None = None) -> None:
        self.registry = registry if registry is not None else CollectorRegistry()

        self.connections_active = Gauge(
            "mcpsb_connections_active",
            "Tunnel connections currently established.",
            registry=self.registry,
        )
        self.servers = Gauge(
            "mcpsb_servers",
            "Known MCP servers across all connections, by lifecycle state.",
            ["state"],
            registry=self.registry,
        )
        self.tools = Gauge(
            "mcpsb_tools_total",
            "Tools currently exposed by a server on a connection.",
            ["label", "server"],
            registry=self.registry,
        )
        self.tool_calls = Counter(
            "mcpsb_tool_calls_total",
            "Tool calls completed.",
            ["label", "server", "tool", "status", "source"],
            registry=self.registry,
        )
        self.tool_call_duration = Histogram(
            "mcpsb_tool_call_duration_seconds",
            "Wall-clock duration of a tool call, hub-side.",
            ["label", "server", "tool"],
            buckets=DURATION_BUCKETS,
            registry=self.registry,
        )
        self.tunnel_frames = Counter(
            "mcpsb_tunnel_frames_total",
            "Tunnel protocol frames, by direction relative to the hub.",
            ["direction"],
            registry=self.registry,
        )
        self.loki_dropped = Counter(
            "mcpsb_loki_dropped_total",
            "Log events dropped instead of being shipped to Loki.",
            registry=self.registry,
        )

        # Pre-create the fixed-domain series so a fresh hub scrapes as zeroes
        # rather than as absent series, which would make rate() and alerts
        # silently no-op until the first event.
        for state in SERVER_STATES:
            self.servers.labels(state=state).set(0)
        for direction in FRAME_DIRECTIONS:
            self.tunnel_frames.labels(direction=direction).inc(0)
        self.connections_active.set(0)
        self.loki_dropped.inc(0)

    def render(self) -> tuple[bytes, str]:
        """Body and Content-Type for the /metrics route."""
        return generate_latest(self.registry), CONTENT_TYPE_LATEST

    def observe_call(
        self,
        *,
        label: str,
        server: str,
        tool: str,
        status: str,
        source: str,
        duration_seconds: float,
    ) -> None:
        self.tool_calls.labels(
            label=label, server=server, tool=tool, status=status, source=source
        ).inc()
        # A negative duration means a clock went backwards mid-call; clamping
        # keeps the histogram's _sum monotonic.
        self.tool_call_duration.labels(label=label, server=server, tool=tool).observe(
            max(0.0, float(duration_seconds))
        )

    def set_connections(self, n: int) -> None:
        self.connections_active.set(n)

    def set_servers(self, by_state: dict[str, int]) -> None:
        """Set every known state, so a state that emptied drops back to zero."""
        for state in SERVER_STATES:
            self.servers.labels(state=state).set(by_state.get(state, 0))
        for state, count in by_state.items():
            if state not in SERVER_STATES:
                self.servers.labels(state=state).set(count)

    def set_tools(self, label: str, server: str, n: int) -> None:
        self.tools.labels(label=label, server=server).set(n)

    def clear_tools(self, label: str, server: str) -> None:
        """Drop the series for a server that went away.

        Without this, a disconnected machine keeps reporting its last tool
        count forever and dashboards overcount.
        """
        try:
            self.tools.remove(label, server)
        except KeyError:
            pass

    def count_frame(self, direction: str) -> None:
        if direction not in FRAME_DIRECTIONS:
            raise ValueError(f"direction must be one of {FRAME_DIRECTIONS}, got {direction!r}")
        self.tunnel_frames.labels(direction=direction).inc()

    def count_loki_dropped(self, n: int = 1) -> None:
        if n > 0:
            self.loki_dropped.inc(n)
