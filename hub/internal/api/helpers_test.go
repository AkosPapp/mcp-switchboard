package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/calls"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/config"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/events"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/protocol"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/registry"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

// fakeStore is the call log as the handlers see it. A real SQLite store would
// work too; a fake keeps each test's expectations on the same screen as the
// rows that produce them, and lets a failure be injected without a broken file.
type fakeStore struct {
	mu sync.Mutex

	records []store.CallRecord
	stats   store.CallStats
	dbStats store.Stats

	// lastFilter records what the handler asked for, which is how the clamp
	// and the filter tests observe the translation rather than its result.
	lastFilter store.CallFilter

	err error

	// Push subscriptions, keyed by endpoint - same upsert-by-endpoint
	// semantics as the real store, kept simple for handler-level tests.
	subs    []store.PushSubscription
	pushErr error
}

func (f *fakeStore) UpsertPushSubscription(_ context.Context, sub store.PushSubscription) (store.PushSubscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pushErr != nil {
		return store.PushSubscription{}, f.pushErr
	}
	for i, existing := range f.subs {
		if existing.Endpoint == sub.Endpoint {
			sub.ID = existing.ID
			f.subs[i] = sub
			return sub, nil
		}
	}
	f.subs = append(f.subs, sub)
	return sub, nil
}

func (f *fakeStore) ListPushSubscriptions(context.Context) ([]store.PushSubscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.PushSubscription(nil), f.subs...), f.pushErr
}

func (f *fakeStore) DeletePushSubscription(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pushErr != nil {
		return f.pushErr
	}
	out := f.subs[:0]
	for _, s := range f.subs {
		if s.ID != id {
			out = append(out, s)
		}
	}
	f.subs = out
	return nil
}

func (f *fakeStore) ListCalls(_ context.Context, filter store.CallFilter) ([]store.CallRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastFilter = filter
	if f.err != nil {
		return nil, f.err
	}

	var matched []store.CallRecord
	for _, record := range f.records {
		if filter.Server != "" && record.Server != filter.Server {
			continue
		}
		if filter.Tool != "" && record.Tool != filter.Tool {
			continue
		}
		if filter.Status != "" && record.Status != filter.Status {
			continue
		}
		if filter.Label != "" && record.Label != filter.Label {
			continue
		}
		if filter.Source != "" && record.Source != filter.Source {
			continue
		}
		if filter.AgentID != "" && deref(record.AgentID) != filter.AgentID {
			continue
		}
		if filter.ChatID != "" && deref(record.ChatID) != filter.ChatID {
			continue
		}
		matched = append(matched, record)
	}
	if filter.Offset >= len(matched) {
		return nil, nil
	}
	matched = matched[filter.Offset:]
	if filter.Limit > 0 && len(matched) > filter.Limit {
		matched = matched[:filter.Limit]
	}
	return matched, nil
}

func (f *fakeStore) GetCall(_ context.Context, id string) (*store.CallRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	for i := range f.records {
		if f.records[i].ID == id {
			record := f.records[i]
			return &record, nil
		}
	}
	return nil, nil
}

func (f *fakeStore) CallStats(context.Context) (store.CallStats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stats, f.err
}

func (f *fakeStore) Stats(context.Context) (store.Stats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dbStats, f.err
}

func (f *fakeStore) add(record store.CallRecord) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, record)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// fakeDispatcher stands in for calls.Dispatcher. The real one needs a live MCP
// client session behind the channel, which is a tunnel's worth of scaffolding
// for a test about HTTP status codes.
type fakeDispatcher struct {
	mu sync.Mutex

	result *calls.Result
	err    error
	seen   []calls.Request

	// onCall lets a test write the row the dispatcher would have written.
	onCall func(calls.Request) (*calls.Result, error)
}

func (d *fakeDispatcher) Call(_ context.Context, req calls.Request) (*calls.Result, error) {
	d.mu.Lock()
	d.seen = append(d.seen, req)
	onCall := d.onCall
	d.mu.Unlock()
	if onCall != nil {
		return onCall(req)
	}
	return d.result, d.err
}

func (d *fakeDispatcher) requests() []calls.Request {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]calls.Request(nil), d.seen...)
}

// sentFrame is one frame a test connection was asked to send.
type sentFrame struct {
	frame any
}

type fixture struct {
	handler  *Handler
	registry *registry.Registry
	bus      *events.Bus
	store    *fakeStore
	dispatch *fakeDispatcher
	settings config.Settings

	mu   sync.Mutex
	sent []sentFrame
	// sendErr makes the tunnel write fail, standing in for a client that went
	// away between the console's click and the frame.
	sendErr error
}

func newFixture(t *testing.T, mutate func(*Options)) *fixture {
	t.Helper()

	bus := events.NewBus()
	f := &fixture{
		registry: registry.New(bus),
		bus:      bus,
		store:    &fakeStore{},
		dispatch: &fakeDispatcher{},
		settings: config.Settings{PrivatePort: 8099, TunnelToken: "tunnel-secret"},
	}

	opts := Options{
		Settings:   f.settings,
		Registry:   f.registry,
		Store:      f.store,
		Dispatcher: f.dispatch,
		Bus:        bus,
		PushStore:  f.store,
	}
	if mutate != nil {
		mutate(&opts)
	}
	f.settings = opts.Settings
	f.handler = New(opts)
	return f
}

// connect registers a connection carrying one server with the given tools.
func (f *fixture) connect(id, label, serverName, project string, tools ...string) *registry.Connection {
	connection := registry.NewConnection(
		id, label,
		protocol.ClientInfo{Name: "mcp-switchboard-client", Version: "1", Label: label},
		time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		func(_ context.Context, frame any) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.sendErr != nil {
				return f.sendErr
			}
			f.sent = append(f.sent, sentFrame{frame: frame})
			return nil
		},
	)
	channel := registry.NewServerChannel(id, label, protocol.ServerDecl{
		Name: serverName, Project: project, Command: "uvx " + serverName,
	})
	infos := make([]registry.ToolInfo, 0, len(tools))
	for _, tool := range tools {
		infos = append(infos, registry.ToolInfo{Name: tool, Title: tool, Description: tool})
	}
	channel.SetTools(infos)
	connection.AddServer(channel)
	f.registry.AddConnection(connection)
	return connection
}

func (f *fixture) frames() []any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]any, 0, len(f.sent))
	for _, s := range f.sent {
		out = append(out, s.frame)
	}
	return out
}

// do runs one request against the handler.
func (f *fixture) do(t *testing.T, method, target string, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	f.handler.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder, into any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), into); err != nil {
		t.Fatalf("could not decode the response: %v\nbody: %s", err, rec.Body.String())
	}
}

func requireStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d\nbody: %s", rec.Code, want, rec.Body.String())
	}
}

var _ http.Handler = (*Handler)(nil)
