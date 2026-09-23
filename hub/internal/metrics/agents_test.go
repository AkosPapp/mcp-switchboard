package metrics

import (
	"strconv"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestOrchestratorSeriesArePrecreatedAtZero(t *testing.T) {
	m := New()
	for _, name := range []string{
		"mcpsb_agents_total", "mcpsb_agent_spawns_total", "mcpsb_runs_active", "mcpsb_runs_total",
		"mcpsb_run_duration_seconds", "mcpsb_agent_messages_total", "mcpsb_agent_message_delivery_seconds",
		"mcpsb_approvals_total", "mcpsb_stream_clients",
	} {
		n, err := testutil.GatherAndCount(m.Registry(), name)
		if err != nil || n == 0 {
			t.Errorf("%s: %d series (%v)", name, n, err)
		}
	}
	m.CountApproval("bogus")
	m.CountApproval("timeout")
	m.ObserveTurn("p", "anthropic", "opus", 10, 5, 0, 0, 42, 0.5)
	out, err := testutil.GatherAndCount(m.Registry(), "mcpsb_llm_tokens_total")
	if err != nil || out != 2 {
		t.Errorf("token series = %d (%v)", out, err)
	}
	m.SetAgents(map[string]map[string]int{"running": {"proj": 2}})
	if !strings.Contains(gather(t, m), `mcpsb_agents_total{project="proj",status="running"} 2`) {
		t.Error("agent gauge not set")
	}
}

func gather(t *testing.T, m *Metrics) string {
	t.Helper()
	mfs, err := m.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	for _, mf := range mfs {
		for _, mm := range mf.Metric {
			sb.WriteString(mf.GetName() + "{")
			for i, l := range mm.Label {
				if i > 0 {
					sb.WriteString(",")
				}
				sb.WriteString(l.GetName() + `="` + l.GetValue() + `"`)
			}
			sb.WriteString("} ")
			if mm.Gauge != nil {
				sb.WriteString(strings.TrimRight(strings.TrimRight(formatF(mm.Gauge.GetValue()), "0"), "."))
			}
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

func formatF(f float64) string {
	return strings.TrimSpace(strings.Replace(strings.Replace(fmtFloat(f), "+", "", -1), " ", "", -1))
}

func fmtFloat(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
