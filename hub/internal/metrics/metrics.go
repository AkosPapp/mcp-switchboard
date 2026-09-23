// Package metrics is the hub's Prometheus instrumentation, all of it prefixed
// mcpsb_.
//
// Everything lives on a registry owned by the Metrics value, never on
// prometheus.DefaultRegisterer. Two hubs (or a hub and its test suite) can
// therefore coexist in one process without duplicate-collector panics, and a
// test can assert on exactly the series it produced.
//
// CARDINALITY (spec.md 11.1). Label values must be bounded and come from
// configuration: label, project, server, tool, status, source, provider and
// model qualify. agent_id, chat_id, run_id, connection_id and error text do
// not, and must never become labels here. Once agents spawn agents those are
// unbounded by construction, and the series explosion would take /metrics down
// before it took anything else down. Per-agent numbers belong in the store,
// served by /api/stats and the Graph view; Prometheus gets aggregates.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ServerStates mirrors protocol.State*, duplicated rather than imported so this
// package stays free of hub internals.
var ServerStates = [...]string{"starting", "running", "exited", "failed"}

// FrameDirections is relative to the hub: a frame read off the tunnel is "in".
var FrameDirections = [...]string{"in", "out"}

// DurationBuckets: tool calls run from a few milliseconds (a local git status)
// to tens of seconds (a slow network fetch); the last finite bucket is the
// timeout range.
var DurationBuckets = []float64{
	0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0, 10.0, 20.0, 30.0, 60.0,
}

// Metrics holds the hub's collectors and the registry they live on.
type Metrics struct {
	registry *prometheus.Registry

	connectionsActive prometheus.Gauge
	servers           *prometheus.GaugeVec
	tools             *prometheus.GaugeVec
	toolCalls         *prometheus.CounterVec
	toolCallDuration  *prometheus.HistogramVec
	tunnelFrames      *prometheus.CounterVec
	lokiDropped       prometheus.Counter

	orch orchMetrics
}

// New builds a Metrics on its own registry.
func New() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		registry: reg,
		connectionsActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "mcpsb_connections_active",
			Help: "Tunnel connections currently established.",
		}),
		servers: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "mcpsb_servers",
			Help: "Known MCP servers across all connections, by lifecycle state.",
		}, []string{"state"}),
		tools: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "mcpsb_tools_total",
			Help: "Tools currently exposed by a server on a connection.",
		}, []string{"label", "server"}),
		// status takes "denied" and source takes "agent" (spec.md O1). They are
		// ordinary label values, not separate series: a ..._denied_total would
		// carry the same numbers under a second name and the two would
		// eventually disagree.
		toolCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mcpsb_tool_calls_total",
			Help: "Tool calls completed.",
		}, []string{"label", "server", "tool", "status", "source"}),
		toolCallDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "mcpsb_tool_call_duration_seconds",
			Help:    "Wall-clock duration of a tool call, hub-side.",
			Buckets: DurationBuckets,
		}, []string{"label", "server", "tool"}),
		tunnelFrames: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mcpsb_tunnel_frames_total",
			Help: "Tunnel protocol frames, by direction relative to the hub.",
		}, []string{"direction"}),
		lokiDropped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "mcpsb_loki_dropped_total",
			Help: "Log events dropped instead of being shipped to Loki.",
		}),
	}

	reg.MustRegister(
		m.connectionsActive,
		m.servers,
		m.tools,
		m.toolCalls,
		m.toolCallDuration,
		m.tunnelFrames,
		m.lokiDropped,
	)

	// Pre-create the fixed-domain series (spec.md O2) so a fresh hub scrapes as
	// zeroes rather than as absent series, which would make rate() and alerts
	// silently no-op until the first event. Only domains known up front qualify;
	// label/server/tool come from whoever connects.
	for _, state := range ServerStates {
		m.servers.WithLabelValues(state).Set(0)
	}
	for _, direction := range FrameDirections {
		m.tunnelFrames.WithLabelValues(direction).Add(0)
	}
	m.connectionsActive.Set(0)
	m.lokiDropped.Add(0)
	m.orch = newOrchMetrics(reg)

	return m
}

// Registry is the hub-owned registry. Exposed for tests and for any caller that
// needs to register a collector of its own alongside these.
func (m *Metrics) Registry() *prometheus.Registry { return m.registry }

// Handler serves the /metrics route off this hub's registry alone, so a second
// hub in the same process is invisible here.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{Registry: m.registry})
}

// ObserveCall records one completed tool call and how long it took.
func (m *Metrics) ObserveCall(label, server, tool, status, source string, durationSeconds float64) {
	m.toolCalls.WithLabelValues(label, server, tool, status, source).Inc()
	// A negative duration means a clock went backwards mid-call; clamping keeps
	// the histogram's _sum monotonic.
	if durationSeconds < 0 {
		durationSeconds = 0
	}
	m.toolCallDuration.WithLabelValues(label, server, tool).Observe(durationSeconds)
}

// SetConnections records the number of established tunnel connections.
func (m *Metrics) SetConnections(n int) { m.connectionsActive.Set(float64(n)) }

// SetServers sets every known state, so a state that emptied drops back to zero
// instead of keeping its last value forever.
func (m *Metrics) SetServers(byState map[string]int) {
	for _, state := range ServerStates {
		m.servers.WithLabelValues(state).Set(float64(byState[state]))
	}
	for state, count := range byState {
		if !knownState(state) {
			m.servers.WithLabelValues(state).Set(float64(count))
		}
	}
}

// SetTools records how many tools a server currently exposes.
func (m *Metrics) SetTools(label, server string, n int) {
	m.tools.WithLabelValues(label, server).Set(float64(n))
}

// ClearTools drops the series for a server that went away. Without this, a
// disconnected machine keeps reporting its last tool count forever and
// dashboards overcount.
func (m *Metrics) ClearTools(label, server string) {
	m.tools.DeleteLabelValues(label, server)
}

// CountFrame counts one tunnel frame. An unknown direction is ignored rather
// than minting a series: frame counting sits on the hot path and must not be
// able to fail a connection.
func (m *Metrics) CountFrame(direction string) {
	for _, d := range FrameDirections {
		if d == direction {
			m.tunnelFrames.WithLabelValues(direction).Inc()
			return
		}
	}
}

// CountLokiDropped implements the drop counter the loki package expects.
func (m *Metrics) CountLokiDropped(n int) {
	if n > 0 {
		m.lokiDropped.Add(float64(n))
	}
}

func knownState(state string) bool {
	for _, s := range ServerStates {
		if s == state {
			return true
		}
	}
	return false
}
