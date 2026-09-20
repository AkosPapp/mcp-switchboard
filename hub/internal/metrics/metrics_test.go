package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// Two hubs in one process must not collide: the reason the collectors live on
// an owned registry rather than the default one (spec.md G4).
func TestTwoInstancesAreIndependent(t *testing.T) {
	a, b := New(), New()

	a.SetConnections(3)
	a.CountFrame("in")
	b.CountFrame("in")
	b.CountFrame("in")

	if got := testutil.ToFloat64(a.connectionsActive); got != 3 {
		t.Errorf("a connections = %v, want 3", got)
	}
	if got := testutil.ToFloat64(b.connectionsActive); got != 0 {
		t.Errorf("b connections = %v, want 0", got)
	}
	if got := testutil.ToFloat64(a.tunnelFrames.WithLabelValues("in")); got != 1 {
		t.Errorf("a frames in = %v, want 1", got)
	}
	if got := testutil.ToFloat64(b.tunnelFrames.WithLabelValues("in")); got != 2 {
		t.Errorf("b frames in = %v, want 2", got)
	}
}

// O2: a fresh hub scrapes the fixed-domain series as zeroes, not as absences.
func TestFixedDomainSeriesStartAtZero(t *testing.T) {
	m := New()
	body := scrape(t, m)

	want := []string{
		`mcpsb_connections_active 0`,
		`mcpsb_servers{state="starting"} 0`,
		`mcpsb_servers{state="running"} 0`,
		`mcpsb_servers{state="exited"} 0`,
		`mcpsb_servers{state="failed"} 0`,
		`mcpsb_tunnel_frames_total{direction="in"} 0`,
		`mcpsb_tunnel_frames_total{direction="out"} 0`,
		`mcpsb_loki_dropped_total 0`,
	}
	for _, line := range want {
		if !strings.Contains(body, line) {
			t.Errorf("scrape is missing %q\n%s", line, body)
		}
	}
}

func TestObserveCallLabels(t *testing.T) {
	m := New()
	m.ObserveCall("dev", "git", "status", "ok", "consumer", 0.2)
	m.ObserveCall("dev", "git", "status", "ok", "consumer", 0.3)
	// O1: denied and agent are ordinary values of the existing labels.
	m.ObserveCall("dev", "git", "commit", "denied", "agent", -1)

	const want = `
# HELP mcpsb_tool_calls_total Tool calls completed.
# TYPE mcpsb_tool_calls_total counter
mcpsb_tool_calls_total{label="dev",server="git",source="agent",status="denied",tool="commit"} 1
mcpsb_tool_calls_total{label="dev",server="git",source="consumer",status="ok",tool="status"} 2
`
	if err := testutil.CollectAndCompare(m.toolCalls, strings.NewReader(want), "mcpsb_tool_calls_total"); err != nil {
		t.Error(err)
	}

	body := scrape(t, m)
	if !strings.Contains(body, `mcpsb_tool_call_duration_seconds_sum{label="dev",server="git",tool="status"} 0.5`) {
		t.Errorf("duration sum missing from scrape:\n%s", body)
	}
	// The clock-went-backwards case is clamped, so _sum stays monotonic.
	if !strings.Contains(body, `mcpsb_tool_call_duration_seconds_sum{label="dev",server="git",tool="commit"} 0`) {
		t.Errorf("negative duration was not clamped to zero:\n%s", body)
	}
}

func TestSetServersZeroesEmptiedStates(t *testing.T) {
	m := New()
	m.SetServers(map[string]int{"running": 2, "failed": 1})
	m.SetServers(map[string]int{"running": 1})

	if got := testutil.ToFloat64(m.servers.WithLabelValues("running")); got != 1 {
		t.Errorf("running = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.servers.WithLabelValues("failed")); got != 0 {
		t.Errorf("failed = %v, want 0 after it emptied", got)
	}
}

func TestToolsGaugeIsClearedWithTheServer(t *testing.T) {
	m := New()
	m.SetTools("dev", "git", 7)
	if got := testutil.CollectAndCount(m.tools); got != 1 {
		t.Fatalf("tool series = %d, want 1", got)
	}
	m.ClearTools("dev", "git")
	if got := testutil.CollectAndCount(m.tools); got != 0 {
		t.Errorf("tool series after clear = %d, want 0", got)
	}
}

func TestCountFrameIgnoresUnknownDirection(t *testing.T) {
	m := New()
	m.CountFrame("sideways")
	if got := testutil.CollectAndCount(m.tunnelFrames); got != 2 {
		t.Errorf("frame series = %d, want the 2 pre-created ones", got)
	}
}

func TestCountLokiDropped(t *testing.T) {
	m := New()
	m.CountLokiDropped(4)
	m.CountLokiDropped(0)
	m.CountLokiDropped(-1)
	if got := testutil.ToFloat64(m.lokiDropped); got != 4 {
		t.Errorf("dropped = %v, want 4", got)
	}
}

func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape status = %d", rec.Code)
	}
	body, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatalf("read scrape: %v", err)
	}
	return string(body)
}
