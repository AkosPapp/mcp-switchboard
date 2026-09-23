package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Orchestrator series (spec.md 11.2). The cardinality rule of 11.1 applies in
// full: labels here are project, status, finish_reason, provider, model, kind,
// outcome and table - all bounded by configuration. Never agent_id or run_id.

// AgentStatuses, RunStatuses and friends are the fixed domains that are
// pre-created at zero (O2).
var (
	AgentStatuses = [...]string{"idle", "running", "waiting", "blocked", "done", "error"}
	// RunOutcomes are the (status, finish_reason) pairs a run can end with.
	RunOutcomes = [...][2]string{
		{"done", "stop"}, {"done", "max_tokens"}, {"done", "budget"},
		{"cancelled", "cancelled"}, {"error", "error"}, {"interrupted", "interrupted"},
	}
	ApprovalOutcomes = [...]string{"approved", "denied", "timeout"}
	StreamKinds      = [...]string{"global", "chat"}
	TokenKinds       = [...]string{"input", "output", "cache_read", "cache_write"}
)

// RunDurationBuckets: a run is from a second (one answer) to the 30 minute
// default wall clock.
var RunDurationBuckets = []float64{0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600, 1800}

// LLMDurationBuckets: time to stream one completion.
var LLMDurationBuckets = []float64{0.25, 0.5, 1, 2.5, 5, 10, 20, 40, 80, 160}

// DeliveryBuckets: send returning -> recipient's run starting (B3).
var DeliveryBuckets = []float64{0.01, 0.05, 0.1, 0.5, 1, 2.5, 5, 15, 60, 300}

type orchMetrics struct {
	agents        *prometheus.GaugeVec
	spawns        *prometheus.CounterVec
	runsActive    *prometheus.GaugeVec
	runsTotal     *prometheus.CounterVec
	runDuration   *prometheus.HistogramVec
	turns         *prometheus.CounterVec
	tokens        *prometheus.CounterVec
	cost          *prometheus.CounterVec
	llmDuration   *prometheus.HistogramVec
	llmErrors     *prometheus.CounterVec
	messages      *prometheus.CounterVec
	delivery      *prometheus.HistogramVec
	approvals     *prometheus.CounterVec
	streamClients *prometheus.GaugeVec
	storeRows     *prometheus.GaugeVec
}

func newOrchMetrics(reg *prometheus.Registry) orchMetrics {
	o := orchMetrics{
		agents: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "mcpsb_agents_total",
			Help: "Live agents by derived status and project."}, []string{"status", "project"}),
		spawns: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "mcpsb_agent_spawns_total",
			Help: "Agents created (spawned or created from the console)."}, []string{"project"}),
		runsActive: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "mcpsb_runs_active",
			Help: "Runs currently in status running (holding a concurrency slot)."}, []string{"project"}),
		runsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "mcpsb_runs_total",
			Help: "Runs finished."}, []string{"project", "status", "finish_reason"}),
		runDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "mcpsb_run_duration_seconds",
			Help: "Wall-clock duration of a run, waiting included.", Buckets: RunDurationBuckets}, []string{"project"}),
		turns: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "mcpsb_agent_turns_total",
			Help: "Model round-trips made by agents."}, []string{"project", "provider", "model"}),
		tokens: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "mcpsb_llm_tokens_total",
			Help: "Tokens used, by kind."}, []string{"provider", "model", "kind"}),
		cost: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "mcpsb_llm_cost_micros_total",
			Help: "Model spend in integer micros."}, []string{"provider", "model"}),
		llmDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "mcpsb_llm_request_duration_seconds",
			Help: "Duration of one streamed completion.", Buckets: LLMDurationBuckets}, []string{"provider", "model"}),
		llmErrors: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "mcpsb_llm_errors_total",
			Help: "Failed completions, by kind (request: could not start; stream: failed mid-stream)."}, []string{"provider", "model", "kind"}),
		messages: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "mcpsb_agent_messages_total",
			Help: "Agent-to-agent messages sent."}, []string{"project"}),
		delivery: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "mcpsb_agent_message_delivery_seconds",
			Help: "Latency from a send returning to the recipient's run starting.", Buckets: DeliveryBuckets}, []string{"project"}),
		approvals: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "mcpsb_approvals_total",
			Help: "Human approvals resolved."}, []string{"outcome"}),
		streamClients: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "mcpsb_stream_clients",
			Help: "Open SSE stream clients."}, []string{"kind"}),
		storeRows: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "mcpsb_store_rows",
			Help: "Rows per store table."}, []string{"table"}),
	}
	reg.MustRegister(o.agents, o.spawns, o.runsActive, o.runsTotal, o.runDuration, o.turns, o.tokens,
		o.cost, o.llmDuration, o.llmErrors, o.messages, o.delivery, o.approvals, o.streamClients, o.storeRows)

	// O2: pre-create every fixed domain. Provider and model come from
	// configuration and are unknown here, so those series appear on first use;
	// "project" is pre-created for the no-project value only.
	for _, st := range AgentStatuses {
		o.agents.WithLabelValues(st, "").Set(0)
	}
	o.spawns.WithLabelValues("").Add(0)
	o.runsActive.WithLabelValues("").Set(0)
	for _, r := range RunOutcomes {
		o.runsTotal.WithLabelValues("", r[0], r[1]).Add(0)
	}
	o.runDuration.WithLabelValues("")
	o.messages.WithLabelValues("").Add(0)
	o.delivery.WithLabelValues("")
	for _, out := range ApprovalOutcomes {
		o.approvals.WithLabelValues(out).Add(0)
	}
	for _, k := range StreamKinds {
		o.streamClients.WithLabelValues(k).Set(0)
	}
	return o
}

// SetAgents replaces the agent gauge: status -> project -> count. Every status
// keeps a series (zero when empty).
func (m *Metrics) SetAgents(counts map[string]map[string]int) {
	m.orch.agents.Reset()
	for _, st := range AgentStatuses {
		m.orch.agents.WithLabelValues(st, "").Set(0)
	}
	for st, byProject := range counts {
		for project, n := range byProject {
			m.orch.agents.WithLabelValues(st, project).Set(float64(n))
		}
	}
}

// CountSpawn counts one created agent.
func (m *Metrics) CountSpawn(project string) { m.orch.spawns.WithLabelValues(project).Inc() }

// AddRunsActive adjusts the running-runs gauge (delta +1 / -1).
func (m *Metrics) AddRunsActive(project string, delta int) {
	m.orch.runsActive.WithLabelValues(project).Add(float64(delta))
}

// ObserveRun records a finished run.
func (m *Metrics) ObserveRun(project, status, finishReason string, seconds float64) {
	m.orch.runsTotal.WithLabelValues(project, status, finishReason).Inc()
	if seconds < 0 {
		seconds = 0
	}
	m.orch.runDuration.WithLabelValues(project).Observe(seconds)
}

// ObserveTurn records one model round-trip and what it cost.
func (m *Metrics) ObserveTurn(project, provider, model string, in, out, cacheRead, cacheWrite int, costMicros int64, seconds float64) {
	m.orch.turns.WithLabelValues(project, provider, model).Inc()
	for kind, n := range map[string]int{"input": in, "output": out, "cache_read": cacheRead, "cache_write": cacheWrite} {
		if n > 0 {
			m.orch.tokens.WithLabelValues(provider, model, kind).Add(float64(n))
		}
	}
	if costMicros > 0 {
		m.orch.cost.WithLabelValues(provider, model).Add(float64(costMicros))
	}
	if seconds < 0 {
		seconds = 0
	}
	m.orch.llmDuration.WithLabelValues(provider, model).Observe(seconds)
}

// CountLLMError counts a failed completion; kind is "request" or "stream".
func (m *Metrics) CountLLMError(provider, model, kind string) {
	m.orch.llmErrors.WithLabelValues(provider, model, kind).Inc()
}

// CountAgentMessage counts one agent-to-agent message.
func (m *Metrics) CountAgentMessage(project string) { m.orch.messages.WithLabelValues(project).Inc() }

// ObserveDelivery records B3 latency.
func (m *Metrics) ObserveDelivery(project string, seconds float64) {
	if seconds < 0 {
		seconds = 0
	}
	m.orch.delivery.WithLabelValues(project).Observe(seconds)
}

// CountApproval counts a resolved approval; outcome approved|denied|timeout.
func (m *Metrics) CountApproval(outcome string) {
	for _, o := range ApprovalOutcomes {
		if o == outcome {
			m.orch.approvals.WithLabelValues(outcome).Inc()
			return
		}
	}
}

// SetStreamClients sets the open-stream gauge; kind is global or chat.
func (m *Metrics) SetStreamClients(kind string, n int) {
	for _, k := range StreamKinds {
		if k == kind {
			m.orch.streamClients.WithLabelValues(kind).Set(float64(n))
			return
		}
	}
}

// SetStoreRows records row counts per table (bounded: the schema's tables).
func (m *Metrics) SetStoreRows(tables map[string]int64) {
	for t, n := range tables {
		m.orch.storeRows.WithLabelValues(t).Set(float64(n))
	}
}
