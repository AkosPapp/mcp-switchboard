package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/calls"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/config"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/protocol"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

func TestConnectionsSnapshot(t *testing.T) {
	f := newFixture(t, nil)
	f.connect("c1", "box", "demo", "", "echo")
	f.connect("c2", "laptop", "lsp", "nix", "hover")

	rec := f.do(t, http.MethodGet, "/api/connections", "")
	requireStatus(t, rec, http.StatusOK)

	var body struct {
		Connections []struct {
			ID      string `json:"id"`
			Label   string `json:"label"`
			Servers []struct {
				Name    string  `json:"name"`
				Project *string `json:"project"`
				Tools   []struct {
					Name        string  `json:"name"`
					ExposedName *string `json:"exposedName"`
				} `json:"tools"`
			} `json:"servers"`
		} `json:"connections"`
	}
	decode(t, rec, &body)

	if len(body.Connections) != 2 {
		t.Fatalf("got %d connections, want 2", len(body.Connections))
	}
	byLabel := map[string]int{}
	for i, connection := range body.Connections {
		byLabel[connection.Label] = i
	}

	box := body.Connections[byLabel["box"]].Servers[0]
	if box.Project != nil {
		t.Errorf("a projectless server must report null, got %#v", box.Project)
	}
	if got := *box.Tools[0].ExposedName; got != "box__demo__echo" {
		t.Errorf("exposed name = %q", got)
	}

	laptop := body.Connections[byLabel["laptop"]].Servers[0]
	if laptop.Project == nil || *laptop.Project != "nix" {
		t.Errorf("project = %#v", laptop.Project)
	}
	if got := *laptop.Tools[0].ExposedName; got != "laptop__nix__lsp__hover" {
		t.Errorf("exposed name = %q", got)
	}
}

// N1, the rule this endpoint exists to get right: a tool that answers with an
// MCP error is a SUCCESSFUL call that returned an error. Only an unknown
// connection, server or tool is a 404. Conflating the two would make the
// console report transport failures as tool bugs.
func TestToolErrorIsNotATransportError(t *testing.T) {
	f := newFixture(t, nil)
	connection := f.connect("c1", "box", "demo", "", "boom")
	f.dispatch.result = &calls.Result{CallID: "call-1", IsError: true, Error: "it exploded"}
	f.store.add(store.CallRecord{
		ID: "call-1", Status: store.StatusError, Error: "it exploded", Source: SourceConsole,
	})

	rec := f.do(t, http.MethodPost,
		"/api/connections/"+connection.ID+"/servers/demo/tools/boom/call", `{"arguments":{}}`)
	requireStatus(t, rec, http.StatusOK)

	var body map[string]any
	decode(t, rec, &body)
	if body["status"] != store.StatusError {
		t.Errorf("status = %v, want %q", body["status"], store.StatusError)
	}
	if body["error"] != "it exploded" {
		t.Errorf("error = %v", body["error"])
	}
}

// A server whose session is gone is also a 200: the call happened, it failed,
// and the row says so (spec.md A15 draws the same distinction for agents).
func TestCallToADisconnectedServerStillAnswersWithARow(t *testing.T) {
	f := newFixture(t, nil)
	connection := f.connect("c1", "box", "demo", "", "echo")
	f.dispatch.result = &calls.Result{CallID: "call-2", Error: calls.ErrNotConnected.Error()}
	f.dispatch.err = calls.ErrNotConnected

	rec := f.do(t, http.MethodPost,
		"/api/connections/"+connection.ID+"/servers/demo/tools/echo/call", `{"arguments":{}}`)
	requireStatus(t, rec, http.StatusOK)
}

func TestCallToolNotFoundCases(t *testing.T) {
	f := newFixture(t, nil)
	connection := f.connect("c1", "box", "demo", "", "echo")

	cases := map[string]string{
		"unknown connection": "/api/connections/nope/servers/demo/tools/echo/call",
		"unknown server":     "/api/connections/" + connection.ID + "/servers/nope/tools/echo/call",
		"unknown tool":       "/api/connections/" + connection.ID + "/servers/demo/tools/nope/call",
	}
	for name, target := range cases {
		t.Run(name, func(t *testing.T) {
			rec := f.do(t, http.MethodPost, target, `{"arguments":{}}`)
			requireStatus(t, rec, http.StatusNotFound)
		})
	}
}

func TestCallToolPassesArgumentsAndSource(t *testing.T) {
	f := newFixture(t, nil)
	connection := f.connect("c1", "box", "demo", "", "echo")
	f.dispatch.result = &calls.Result{CallID: "call-3"}

	rec := f.do(t, http.MethodPost,
		"/api/connections/"+connection.ID+"/servers/demo/tools/echo/call",
		`{"arguments":{"message":"hello"}}`)
	requireStatus(t, rec, http.StatusOK)

	seen := f.dispatch.requests()
	if len(seen) != 1 {
		t.Fatalf("dispatched %d calls, want 1", len(seen))
	}
	if seen[0].Arguments["message"] != "hello" {
		t.Errorf("arguments = %#v", seen[0].Arguments)
	}
	if seen[0].Source != SourceConsole {
		t.Errorf("source = %q, want %q", seen[0].Source, SourceConsole)
	}
	// The call log shows the name a consumer at /mcp would use, so a row can be
	// matched to a catalog entry by eye.
	if seen[0].ExposedName != "box__demo__echo" {
		t.Errorf("exposed name = %q", seen[0].ExposedName)
	}
}

func TestRestartSendsAFrame(t *testing.T) {
	f := newFixture(t, nil)
	connection := f.connect("c1", "box", "demo", "", "echo")

	rec := f.do(t, http.MethodPost, "/api/connections/"+connection.ID+"/servers/demo/restart", "")
	requireStatus(t, rec, http.StatusNoContent)

	frames := f.frames()
	if len(frames) != 1 {
		t.Fatalf("sent %d frames, want 1", len(frames))
	}
	raw, err := json.Marshal(frames[0])
	if err != nil {
		t.Fatal(err)
	}
	var frame map[string]any
	if err := json.Unmarshal(raw, &frame); err != nil {
		t.Fatal(err)
	}
	if frame["type"] != protocol.TypeRestart || frame["server"] != "demo" {
		t.Errorf("frame = %v", frame)
	}
}

func TestRestartUnknownTargetIs404(t *testing.T) {
	f := newFixture(t, nil)
	f.connect("c1", "box", "demo", "", "echo")

	rec := f.do(t, http.MethodPost, "/api/connections/nope/servers/demo/restart", "")
	requireStatus(t, rec, http.StatusNotFound)
}

// A client that went away between the console's click and the frame is a
// gateway problem, not a request problem.
func TestRestartReportsAVanishedClient(t *testing.T) {
	f := newFixture(t, nil)
	connection := f.connect("c1", "box", "demo", "", "echo")
	f.mu.Lock()
	f.sendErr = errors.New("socket closed")
	f.mu.Unlock()

	rec := f.do(t, http.MethodPost, "/api/connections/"+connection.ID+"/servers/demo/restart", "")
	if rec.Code < 500 {
		t.Errorf("status = %d, want a 5xx for a client that vanished", rec.Code)
	}
}

func TestListCallsFilters(t *testing.T) {
	f := newFixture(t, nil)
	agent := "agent-1"
	f.store.add(store.CallRecord{ID: "1", Label: "box", Server: "demo", Tool: "echo",
		Status: store.StatusOK, Source: SourceConsole})
	f.store.add(store.CallRecord{ID: "2", Label: "box", Server: "demo", Tool: "boom",
		Status: store.StatusError, Source: SourceConsole})
	f.store.add(store.CallRecord{ID: "3", Label: "laptop", Server: "lsp", Tool: "hover",
		Status: store.StatusDenied, Source: "agent", AgentID: &agent})

	cases := map[string]struct {
		query string
		want  int
	}{
		"no filter":    {"", 3},
		"by label":     {"?label=box", 2},
		"by server":    {"?server=lsp", 1},
		"by tool":      {"?tool=echo", 1},
		"by status":    {"?status=denied", 1},
		"by source":    {"?source=agent", 1},
		"by agent":     {"?agentId=agent-1", 1},
		"no match":     {"?label=nosuch", 0},
		"combination":  {"?label=box&status=error", 1},
		"limit honors": {"?limit=1", 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rec := f.do(t, http.MethodGet, "/api/calls"+tc.query, "")
			requireStatus(t, rec, http.StatusOK)
			var body struct {
				Calls []json.RawMessage `json:"calls"`
			}
			decode(t, rec, &body)
			if len(body.Calls) != tc.want {
				t.Errorf("got %d calls, want %d", len(body.Calls), tc.want)
			}
		})
	}
}

// An API caller must not be able to ask for the whole table in one query.
func TestListCallsClampsTheLimit(t *testing.T) {
	f := newFixture(t, nil)
	rec := f.do(t, http.MethodGet, "/api/calls?limit=99999", "")
	requireStatus(t, rec, http.StatusOK)

	f.store.mu.Lock()
	got := f.store.lastFilter.Limit
	f.store.mu.Unlock()
	if got > store.MaxLimit {
		t.Errorf("limit reached the store as %d, want it clamped to %d", got, store.MaxLimit)
	}
}

func TestGetCall(t *testing.T) {
	f := newFixture(t, nil)
	f.store.add(store.CallRecord{ID: "abc", Status: store.StatusOK, Tool: "echo"})

	rec := f.do(t, http.MethodGet, "/api/calls/abc", "")
	requireStatus(t, rec, http.StatusOK)

	var body map[string]any
	decode(t, rec, &body)
	if body["id"] != "abc" {
		t.Errorf("id = %v", body["id"])
	}

	requireStatus(t, f.do(t, http.MethodGet, "/api/calls/nope", ""), http.StatusNotFound)
}

func TestStats(t *testing.T) {
	f := newFixture(t, nil)
	rec := f.do(t, http.MethodGet, "/api/stats", "")
	requireStatus(t, rec, http.StatusOK)

	var body map[string]any
	decode(t, rec, &body)
	// D11: growth is unbounded by design for chats, so the size has to be
	// visible before it is a problem.
	if _, ok := body["databaseBytes"]; !ok {
		t.Errorf("stats must report the database size, got %v", body)
	}
}

func TestTimestampsCarryAnExplicitOffset(t *testing.T) {
	f := newFixture(t, nil)
	f.store.add(store.CallRecord{
		ID: "abc", Status: store.StatusOK,
		StartedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	})

	rec := f.do(t, http.MethodGet, "/api/calls/abc", "")
	var body map[string]any
	decode(t, rec, &body)

	started, _ := body["startedAt"].(string)
	// JavaScript's Date parses this and Python's fromisoformat round-trips it,
	// which a trailing "Z" does not on 3.10.
	if !strings.HasSuffix(started, "+00:00") {
		t.Errorf("startedAt = %q, want an explicit +00:00 offset", started)
	}
}

func TestAuthIsOpenWhenNoTokenIsConfigured(t *testing.T) {
	f := newFixture(t, nil)
	requireStatus(t, f.do(t, http.MethodGet, "/api/connections", ""), http.StatusOK)
}

func TestAuthRequiresTheTokenWhenConfigured(t *testing.T) {
	f := newFixture(t, func(o *Options) {
		o.Settings = config.Settings{PrivatePort: 8099, PrivateToken: "letmein"}
	})
	guarded := RequireToken("letmein", f.handler)

	rec := httptest.NewRecorder()
	guarded.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/connections", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("without a token: status = %d, want 401", rec.Code)
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/connections", nil)
	req.Header.Set("Authorization", "Bearer letmein")
	guarded.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("with the token: status = %d, want 200", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/connections", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	guarded.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("with the wrong token: status = %d, want 401", rec.Code)
	}
}

func TestEventsStreamDeliversAndKeepsAlive(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.Keepalive = 20 * time.Millisecond })

	srv := httptest.NewServer(f.handler)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// N8: these are what make the stream survive nginx.
	if got := resp.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q, want \"no\"", got)
	}
	if got := resp.Header.Get("Cache-Control"); !strings.Contains(got, "no-cache") {
		t.Errorf("Cache-Control = %q", got)
	}

	// The subscriber has to be attached before an event can reach it, and
	// attaching happens on the handler's goroutine.
	waitFor(t, "the subscriber to attach", func() bool { return f.bus.Subscribers() > 0 })
	f.registry.PublishChange()

	buf := make([]byte, 512)
	deadline := time.Now().Add(3 * time.Second)
	var seen string
	for time.Now().Before(deadline) && !strings.Contains(seen, "connections") {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			seen += string(buf[:n])
		}
		if err != nil {
			break
		}
	}
	if !strings.Contains(seen, "connections") {
		t.Fatalf("never saw the change event; stream so far: %q", seen)
	}
	if !strings.Contains(seen, ":") {
		t.Errorf("expected a keepalive comment in %q", seen)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestEventsStreamEndsWhenTheClientGoesAway(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.Keepalive = 20 * time.Millisecond })
	srv := httptest.NewServer(f.handler)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the subscriber to attach", func() bool { return f.bus.Subscribers() > 0 })

	cancel()
	resp.Body.Close()

	// The handler must unsubscribe on its way out, or every reconnecting
	// console leaks a queue for the life of the process.
	waitFor(t, "the subscriber to detach", func() bool { return f.bus.Subscribers() == 0 })
}
