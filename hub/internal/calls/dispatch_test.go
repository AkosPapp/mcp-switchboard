package calls

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/protocol"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/registry"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

type fakeStore struct {
	mu      sync.Mutex
	records []store.CallRecord
	err     error
}

func (f *fakeStore) RecordCall(_ context.Context, rec store.CallRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, rec)
	return f.err
}

func (f *fakeStore) ListCalls(context.Context, store.CallFilter) ([]store.CallRecord, error) {
	return nil, nil
}
func (f *fakeStore) GetCall(context.Context, string) (*store.CallRecord, error) { return nil, nil }
func (f *fakeStore) PurgeCalls(context.Context) (int64, error)                  { return 0, nil }
func (f *fakeStore) CallStats(context.Context) (store.CallStats, error) {
	return store.CallStats{}, nil
}

func (f *fakeStore) rows() []store.CallRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.CallRecord(nil), f.records...)
}

type fakeMetrics struct {
	mu       sync.Mutex
	observed []string
}

func (m *fakeMetrics) ObserveCall(label, server, tool, status, source string, _ float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.observed = append(m.observed, strings.Join([]string{label, server, tool, status, source}, "/"))
}

func (m *fakeMetrics) calls() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.observed...)
}

// upstream is a real SDK server over an in-memory transport: dispatch's job is
// to drive a live session, so faking the session would test nothing.
func upstream(t *testing.T) *mcp.ClientSession {
	t.Helper()

	server := mcp.NewServer(&mcp.Implementation{Name: "upstream", Version: "1"}, nil)
	schema := map[string]any{"type": "object"}

	server.AddTool(&mcp.Tool{Name: "echo", InputSchema: schema},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "echoed"}},
			}, nil
		})
	server.AddTool(&mcp.Tool{Name: "boom", InputSchema: schema},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: "it exploded"}},
			}, nil
		})
	server.AddTool(&mcp.Tool{Name: "slow", InputSchema: schema},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = server.Run(ctx, serverTransport) }()

	session, err := mcp.NewClient(&mcp.Implementation{Name: "hub", Version: "test"}, nil).
		Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

// request builds a dispatch request against a channel, optionally live.
func request(t *testing.T, tool string, live bool) Request {
	t.Helper()

	connection := registry.NewConnection("c1", "box",
		protocol.ClientInfo{Name: "client", Version: "1", Label: "box"},
		time.Now().UTC(), nil)
	channel := registry.NewServerChannel("c1", "box", protocol.ServerDecl{Name: "demo"})
	if live {
		channel.SetSession(upstream(t))
	}
	connection.AddServer(channel)

	return Request{
		Connection:  connection,
		Channel:     channel,
		Tool:        tool,
		ExposedName: "box__demo__" + tool,
		Arguments:   map[string]any{"message": "hi"},
		Source:      store.SourceConsole,
	}
}

func TestSuccessfulCallIsRecorded(t *testing.T) {
	recorder, meter := &fakeStore{}, &fakeMetrics{}
	d := NewDispatcher(Options{Store: recorder, Metrics: meter})

	result, err := d.Call(context.Background(), request(t, "echo", true))
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Error("a successful call must not be flagged as an error")
	}

	rows := recorder.rows()
	if len(rows) != 1 {
		t.Fatalf("recorded %d rows, want 1", len(rows))
	}
	row := rows[0]
	if row.Status != store.StatusOK || row.Source != store.SourceConsole {
		t.Errorf("row = %q/%q", row.Status, row.Source)
	}
	if row.Tool != "echo" || row.ExposedName != "box__demo__echo" {
		t.Errorf("row names = %q/%q", row.Tool, row.ExposedName)
	}
	if row.Arguments["message"] != "hi" {
		t.Errorf("arguments were not recorded: %#v", row.Arguments)
	}
	if row.Result == nil {
		t.Error("the result must be recorded verbatim, it is what the console shows")
	}
	if row.StartedAt.IsZero() {
		t.Error("startedAt must be set")
	}
	if got := meter.calls(); len(got) != 1 || got[0] != "box/demo/echo/ok/console" {
		t.Errorf("metrics = %v", got)
	}
}

// N1: a tool that answers with an error is a successful call that returned an
// error. It must not come back as a Go error, or every caller will report a
// tool bug as a transport failure.
func TestToolErrorIsNotAGoError(t *testing.T) {
	recorder := &fakeStore{}
	d := NewDispatcher(Options{Store: recorder})

	result, err := d.Call(context.Background(), request(t, "boom", true))
	if err != nil {
		t.Fatalf("a failing tool must not produce a dispatch error: %v", err)
	}
	if !result.IsError {
		t.Error("the result must be flagged as an error")
	}
	if !strings.Contains(result.Error, "exploded") {
		t.Errorf("the tool's message should survive, got %q", result.Error)
	}

	rows := recorder.rows()
	if len(rows) != 1 || rows[0].Status != store.StatusError {
		t.Fatalf("rows = %+v, want one with status error", rows)
	}
	if rows[0].Result == nil {
		t.Error("an error result is still a result worth keeping")
	}
}

// A15: "not connected" is distinguishable from a tool error, because for an
// agent the difference decides whether retrying elsewhere makes sense.
func TestCallToASessionlessServer(t *testing.T) {
	recorder := &fakeStore{}
	d := NewDispatcher(Options{Store: recorder})

	result, err := d.Call(context.Background(), request(t, "echo", false))
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("err = %v, want ErrNotConnected", err)
	}
	if result == nil || result.CallID == "" {
		t.Fatal("even an unreachable server must produce a row to point at")
	}

	rows := recorder.rows()
	if len(rows) != 1 {
		t.Fatalf("recorded %d rows, want 1", len(rows))
	}
	if rows[0].Status != store.StatusError || !strings.Contains(rows[0].Error, "not connected") {
		t.Errorf("row = %q/%q", rows[0].Status, rows[0].Error)
	}
}

// R2: a denied call is a normal tool result the model can adapt to, recorded
// with its own status so the Calls view can show it.
func TestDenyWritesADeniedRow(t *testing.T) {
	recorder, meter := &fakeStore{}, &fakeMetrics{}
	d := NewDispatcher(Options{Store: recorder, Metrics: meter})

	req := request(t, "echo", true)
	agent := "agent-1"
	req.Source = "agent"
	req.AgentID = &agent

	result := d.Deny(context.Background(), req, "not permitted: box/demo")
	if !result.IsError || result.Error == "" {
		t.Error("a denial must read as an error to the model")
	}

	rows := recorder.rows()
	if len(rows) != 1 {
		t.Fatalf("recorded %d rows, want 1", len(rows))
	}
	if rows[0].Status != store.StatusDenied {
		t.Errorf("status = %q, want %q", rows[0].Status, store.StatusDenied)
	}
	if rows[0].AgentID == nil || *rows[0].AgentID != agent {
		t.Error("a denied call must record which agent was refused")
	}
	if got := meter.calls(); len(got) != 1 || !strings.Contains(got[0], "denied") {
		t.Errorf("metrics = %v", got)
	}
}

// The one call anyone wants to read about afterwards is the one that was
// cancelled, so the row is written on a context that the caller cannot cancel.
func TestACancelledCallIsStillRecorded(t *testing.T) {
	recorder := &fakeStore{}
	d := NewDispatcher(Options{Store: recorder, Timeout: 5 * time.Second})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	result, err := d.Call(ctx, request(t, "slow", true))
	if err != nil {
		t.Fatalf("a cancelled call reports through the result, not an error: %v", err)
	}
	if !result.IsError {
		t.Error("a cancelled call is an error to the caller")
	}

	rows := recorder.rows()
	if len(rows) != 1 {
		t.Fatalf("recorded %d rows, want 1 - a cancelled call must still be logged", len(rows))
	}
	if rows[0].Status != store.StatusError {
		t.Errorf("status = %q", rows[0].Status)
	}
}

// The dispatch timeout is the hub's own bound, independent of how long the
// caller is willing to wait.
func TestTheCallTimeoutIsEnforced(t *testing.T) {
	recorder := &fakeStore{}
	d := NewDispatcher(Options{Store: recorder, Timeout: 100 * time.Millisecond})

	started := time.Now()
	result, err := d.Call(context.Background(), request(t, "slow", true))
	if err != nil {
		t.Fatalf("a timeout reports through the result: %v", err)
	}
	if !result.IsError {
		t.Error("a timed-out call is an error")
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Errorf("took %s, the timeout was not enforced", elapsed)
	}
	if rows := recorder.rows(); len(rows) != 1 {
		t.Errorf("recorded %d rows, want 1", len(rows))
	}
}

// History must never fail a call that already happened.
func TestAFailingStoreDoesNotFailTheCall(t *testing.T) {
	recorder := &fakeStore{err: errors.New("disk on fire")}
	d := NewDispatcher(Options{Store: recorder})

	result, err := d.Call(context.Background(), request(t, "echo", true))
	if err != nil || result.IsError {
		t.Errorf("the call should still succeed: err=%v result=%+v", err, result)
	}
}

func TestDispatchWithNoStoreOrMetrics(t *testing.T) {
	// The zero configuration has to work: the orchestrator constructs a
	// dispatcher before anything else is wired up.
	d := NewDispatcher(Options{})
	if _, err := d.Call(context.Background(), request(t, "echo", true)); err != nil {
		t.Fatal(err)
	}
}
