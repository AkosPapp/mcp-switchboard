package mcpserver

import (
	"context"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/calls"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/events"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/protocol"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/registry"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

// recordingStore keeps the rows a dispatch writes, so a test can assert that
// the one dispatch path really was taken (spec.md R1).
type recordingStore struct {
	mu      sync.Mutex
	records []store.CallRecord
}

func (s *recordingStore) RecordCall(_ context.Context, rec store.CallRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, rec)
	return nil
}

func (s *recordingStore) ListCalls(context.Context, store.CallFilter) ([]store.CallRecord, error) {
	return nil, nil
}
func (s *recordingStore) GetCall(context.Context, string) (*store.CallRecord, error) {
	return nil, nil
}
func (s *recordingStore) PurgeCalls(context.Context) (int64, error) { return 0, nil }
func (s *recordingStore) CallStats(context.Context) (store.CallStats, error) {
	return store.CallStats{}, nil
}

func (s *recordingStore) rows() []store.CallRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]store.CallRecord(nil), s.records...)
}

// upstream stands in for one tunnelled MCP server: a real SDK server over an
// in-memory transport, so the hub's own session handling is exercised rather
// than mocked.
func upstream(t *testing.T, tools ...string) *mcp.ClientSession {
	t.Helper()

	server := mcp.NewServer(&mcp.Implementation{Name: "upstream", Version: "1"}, nil)
	for _, name := range tools {
		toolName := name
		server.AddTool(
			// The SDK insists on a schema; an empty object is what a tool with
			// no arguments declares.
			&mcp.Tool{
				Name:        toolName,
				Description: "does " + toolName,
				InputSchema: map[string]any{"type": "object"},
			},
			func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				if strings.HasPrefix(toolName, "boom") {
					return &mcp.CallToolResult{
						IsError: true,
						Content: []mcp.Content{&mcp.TextContent{Text: "it exploded"}},
					}, nil
				}
				return &mcp.CallToolResult{
					Content: []mcp.Content{&mcp.TextContent{Text: toolName + " ran"}},
				}, nil
			},
		)
	}

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = server.Run(ctx, serverTransport) }()

	session, err := mcp.NewClient(&mcp.Implementation{Name: "hub", Version: "test"}, nil).
		Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("could not connect the upstream session: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

type harness struct {
	registry *registry.Registry
	store    *recordingStore
	endpoint *Endpoint
	server   *httptest.Server
}

// addServer registers one tunnelled server with a live session behind it.
func (h *harness) addServer(t *testing.T, connID, label, project, name string, tools ...string) {
	t.Helper()

	connection := h.registry.Get(connID)
	if connection == nil {
		connection = registry.NewConnection(connID, label,
			protocol.ClientInfo{Name: "client", Version: "1", Label: label},
			time.Now().UTC(), nil)
		h.registry.AddConnection(connection)
	}

	channel := registry.NewServerChannel(connID, label,
		protocol.ServerDecl{Name: name, Project: project})
	infos := make([]registry.ToolInfo, 0, len(tools))
	for _, tool := range tools {
		infos = append(infos, registry.ToolInfo{
			Name:        tool,
			Description: "does " + tool,
			InputSchema: map[string]any{"type": "object"},
		})
	}
	channel.SetTools(infos)
	channel.SetSession(upstream(t, tools...))
	channel.SetState(protocol.StateRunning, "", nil)
	connection.AddServer(channel)
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	recorder := &recordingStore{}
	reg := registry.New(events.NewBus())
	endpoint := New(reg, calls.NewDispatcher(calls.Options{Store: recorder}), Options{Version: "test"})

	srv := httptest.NewServer(endpoint)
	t.Cleanup(srv.Close)

	return &harness{registry: reg, store: recorder, endpoint: endpoint, server: srv}
}

// consumer connects the way n8n does: an MCP client over Streamable HTTP.
func (h *harness) consumer(t *testing.T, path string) *mcp.ClientSession {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	session, err := mcp.NewClient(&mcp.Implementation{Name: "consumer", Version: "1"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: h.server.URL + path}, nil)
	if err != nil {
		t.Fatalf("could not connect a consumer to %s: %v", path, err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func listNames(t *testing.T, session *mcp.ClientSession) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	names := make([]string, 0, len(result.Tools))
	for _, tool := range result.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	return names
}

// The table the whole design rests on: what a consumer sees at each scope
// (spec.md H6), with and without a project.
func TestScopesComposeNames(t *testing.T) {
	h := newHarness(t)
	h.addServer(t, "c1", "box", "", "demo", "echo")
	h.addServer(t, "c1", "box", "web", "fs", "read")
	h.addServer(t, "c2", "laptop", "", "demo", "echo")

	cases := []struct {
		name string
		path string
		want []string
	}{
		{"all", "/mcp", []string{
			"box__demo__echo", "box__web__fs__read", "laptop__demo__echo",
		}},
		{"host", "/mcp/host/box", []string{"demo__echo", "web__fs__read"}},
		{"project", "/mcp/host/box/project/web", []string{"fs__read"}},
		{"server", "/mcp/host/box/server/demo", []string{"echo"}},
		{"project and server", "/mcp/host/box/project/web/server/fs", []string{"read"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := listNames(t, h.consumer(t, tc.path))
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestCallReachesTheRightToolAtEveryScope(t *testing.T) {
	h := newHarness(t)
	h.addServer(t, "c1", "box", "", "demo", "echo")
	h.addServer(t, "c1", "box", "web", "fs", "read")

	cases := map[string]struct{ path, tool string }{
		"all":                {"/mcp", "box__demo__echo"},
		"host":               {"/mcp/host/box", "demo__echo"},
		"project":            {"/mcp/host/box/project/web", "fs__read"},
		"server":             {"/mcp/host/box/server/demo", "echo"},
		"project and server": {"/mcp/host/box/project/web/server/fs", "read"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			result, err := h.consumer(t, tc.path).CallTool(ctx, &mcp.CallToolParams{
				Name: tc.tool, Arguments: map[string]any{},
			})
			if err != nil {
				t.Fatalf("tools/call: %v", err)
			}
			if result.IsError {
				t.Fatalf("call reported an error: %+v", result.Content)
			}
			text, ok := result.Content[0].(*mcp.TextContent)
			if !ok || !strings.HasSuffix(text.Text, " ran") {
				t.Errorf("content = %+v", result.Content)
			}
		})
	}
}

// R1: an MCP consumer's call goes through the same dispatch as everything else,
// so it lands in the call log for free.
func TestCallIsRecordedWithItsSource(t *testing.T) {
	h := newHarness(t)
	h.addServer(t, "c1", "box", "", "demo", "echo")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := h.consumer(t, "/mcp").CallTool(ctx, &mcp.CallToolParams{
		Name: "box__demo__echo", Arguments: map[string]any{},
	}); err != nil {
		t.Fatal(err)
	}

	rows := h.store.rows()
	if len(rows) != 1 {
		t.Fatalf("recorded %d rows, want 1", len(rows))
	}
	row := rows[0]
	if row.Source != store.SourceMCP {
		t.Errorf("source = %q, want %q", row.Source, store.SourceMCP)
	}
	if row.Tool != "echo" || row.ExposedName != "box__demo__echo" {
		t.Errorf("row names the wrong tool: %q / %q", row.Tool, row.ExposedName)
	}
	if row.Status != store.StatusOK {
		t.Errorf("status = %q", row.Status)
	}
}

// N1 again, from the consumer's side: a tool that fails answers with an error
// result, not a transport error, and the row says "error".
func TestToolErrorIsAResultNotATransportFailure(t *testing.T) {
	h := newHarness(t)
	h.addServer(t, "c1", "box", "", "demo", "boom")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := h.consumer(t, "/mcp").CallTool(ctx, &mcp.CallToolParams{
		Name: "box__demo__boom", Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("a failing tool must not surface as a transport error: %v", err)
	}
	if !result.IsError {
		t.Error("the result should be flagged as an error")
	}
	rows := h.store.rows()
	if len(rows) != 1 || rows[0].Status != store.StatusError {
		t.Errorf("rows = %+v, want one row with status error", rows)
	}
}

func TestUnknownToolIsAnError(t *testing.T) {
	h := newHarness(t)
	h.addServer(t, "c1", "box", "", "demo", "echo")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := h.consumer(t, "/mcp").CallTool(ctx, &mcp.CallToolParams{
		Name: "box__demo__nosuch", Arguments: map[string]any{},
	})
	if err == nil && (result == nil || !result.IsError) {
		t.Fatal("an unknown tool name must not look like a successful call")
	}
}

// I4: one malformed upstream name must not take out the whole catalog.
func TestAToolWithAnIllegalNameIsDroppedNotFatal(t *testing.T) {
	h := newHarness(t)
	h.addServer(t, "c1", "box", "", "demo", "echo")

	channel := h.registry.FindServer("c1", "demo")
	channel.SetTools(append(channel.Tools(), registry.ToolInfo{
		Name:        "bad name with spaces",
		InputSchema: map[string]any{"type": "object"},
	}))

	names := listNames(t, h.consumer(t, "/mcp"))
	if len(names) != 1 || names[0] != "box__demo__echo" {
		t.Errorf("got %v, want just the valid tool", names)
	}
}

// A server declared but never started has no session, and must not appear.
func TestServersWithoutASessionAreNotListed(t *testing.T) {
	h := newHarness(t)
	h.addServer(t, "c1", "box", "", "demo", "echo")

	connection := h.registry.Get("c1")
	dead := registry.NewServerChannel("c1", "box", protocol.ServerDecl{Name: "dead"})
	dead.SetTools([]registry.ToolInfo{{Name: "ghost"}})
	connection.AddServer(dead)

	for _, name := range listNames(t, h.consumer(t, "/mcp")) {
		if strings.Contains(name, "dead") {
			t.Errorf("a server with no live session was listed: %q", name)
		}
	}
}

func TestUnknownScopePathIs404(t *testing.T) {
	h := newHarness(t)
	resp, err := h.server.Client().Get(h.server.URL + "/mcp/nonsense/deeper")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404 for a path that names no scope", resp.StatusCode)
	}
}

// Listing while the registry churns is the common case, not an edge one: every
// tunnel that reconnects mutates it under a consumer's tools/list.
func TestListingIsSafeWhileTheRegistryChurns(t *testing.T) {
	h := newHarness(t)
	h.addServer(t, "c1", "box", "", "demo", "echo")
	session := h.consumer(t, "/mcp")

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			channel := h.registry.FindServer("c1", "demo")
			channel.SetTools([]registry.ToolInfo{{Name: "echo"}, {Name: "extra"}})
			channel.SetTools([]registry.ToolInfo{{Name: "echo"}})
		}
	}()
	for i := 0; i < 20; i++ {
		listNames(t, session)
	}
	<-done
}
