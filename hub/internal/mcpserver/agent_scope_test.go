package mcpserver

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/calls"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/events"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/registry"
)

func TestParseTargetAgentScope(t *testing.T) {
	got, ok := parseTarget("/mcp/agent/0195abc")
	if !ok || got.Scope != registry.ScopeAgent || got.AgentID != "0195abc" {
		t.Fatalf("got %+v ok=%v", got, ok)
	}
	for _, bad := range []string{"/mcp/agent", "/mcp/agent/", "/mcp/agent/a/b", "/mcp/agent//x"} {
		if _, ok := parseTarget(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
	a, _ := parseTarget("/mcp/agent/1")
	b, _ := parseTarget("/mcp/agent/2")
	if a.key() == b.key() {
		t.Error("two agents share a server key")
	}
}

// With the orchestrator off there is no agent scope at all (E7).
func TestAgentScopeIs404WhenAgentsDisabled(t *testing.T) {
	h := newHarness(t)
	resp, err := h.server.Client().Get(h.server.URL + "/mcp/agent/anything")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

type fakeBackend struct {
	gotAgent, gotTool string
	gotArgs           map[string]any
}

func (f *fakeBackend) HasAgent(_ context.Context, id string) bool { return id == "known" }
func (f *fakeBackend) AgentTools(context.Context, string) ([]*mcp.Tool, error) {
	return []*mcp.Tool{{Name: "switchboard.chat.list", InputSchema: map[string]any{"type": "object"}}}, nil
}
func (f *fakeBackend) CallAgentTool(_ context.Context, agent, name string, args map[string]any) (*mcp.CallToolResult, error) {
	f.gotAgent, f.gotTool, f.gotArgs = agent, name, args
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
}

func TestAgentScopeServesOnlyItsBackendAndIdentityComesFromTheURL(t *testing.T) {
	backend := &fakeBackend{}
	reg := registry.New(events.NewBus())
	ep := New(reg, calls.NewDispatcher(calls.Options{}), Options{Version: "t", Agents: backend})
	srv := httptest.NewServer(ep)
	t.Cleanup(srv.Close)

	resp, err := srv.Client().Get(srv.URL + "/mcp/agent/unknown")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("unknown agent status = %d", resp.StatusCode)
	}

	connect := func(path string) *mcp.ClientSession {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		t.Cleanup(cancel)
		s, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "1"}, nil).
			Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL + path}, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	}
	agentSession := connect("/mcp/agent/known")
	if names := listNames(t, agentSession); len(names) != 1 || names[0] != "switchboard.chat.list" {
		t.Fatalf("agent scope lists %v", names)
	}
	// switchboard.* never appears at the other scopes (5.5).
	if names := listNames(t, connect("/mcp")); len(names) != 0 {
		t.Errorf("/mcp lists %v", names)
	}
	// A chat_id smuggled into the arguments changes nothing (X8).
	res, err := agentSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "switchboard.chat.list", Arguments: map[string]any{"chat_id": "someone-else"}})
	if err != nil || res.IsError {
		t.Fatalf("call: %v %+v", err, res)
	}
	if backend.gotAgent != "known" || backend.gotTool != "switchboard.chat.list" {
		t.Errorf("backend saw agent=%q tool=%q", backend.gotAgent, backend.gotTool)
	}
}
