package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/calls"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/config"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/events"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/llm"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/protocol"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/registry"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

type frameRec struct {
	Chat, Run, Type string
	Data            any
}

type recStreamer struct {
	mu     sync.Mutex
	frames []frameRec
}

func (s *recStreamer) Publish(chat, run, typ string, data any) {
	s.mu.Lock()
	s.frames = append(s.frames, frameRec{chat, run, typ, data})
	s.mu.Unlock()
}
func (s *recStreamer) EndRun(string, string) {}
func (s *recStreamer) types(run string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, f := range s.frames {
		if f.Run == run {
			out = append(out, f.Type)
		}
	}
	return out
}

type env struct {
	t    *testing.T
	st   *store.SQLiteStore
	reg  *registry.Registry
	llm  *llm.Registry
	m    *Manager
	strm *recStreamer
	set  config.Settings
}

func newEnv(t *testing.T, tune ...func(*config.Settings)) *env {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "hub.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	set := config.Settings{
		AgentsEnabled: true, AgentMaxDepth: 4, AgentMaxChildren: 8, AgentMaxConcurrentRuns: 16,
		AgentMaxParallelToolCalls: 8, AgentReplyTimeout: 10, ApprovalTimeout: 3600,
		AgentDefaultBudget: config.DefaultAgentBudget(),
		AgentDefaultGrants: []config.GrantSpec{{Label: "*", Project: "*", Server: "*"}},
	}
	for _, f := range tune {
		f(&set)
	}
	bus := events.NewBus()
	reg := registry.New(bus)
	disp := calls.NewDispatcher(calls.Options{Store: st, Bus: bus})
	e := &env{t: t, st: st, reg: reg, llm: llm.NewRegistry(), strm: &recStreamer{}, set: set}
	e.m = New(Options{Store: st, Registry: reg, Dispatcher: disp, LLM: e.llm, Bus: bus, Streamer: e.strm, Settings: set})
	if err := e.m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = e.m.Shutdown(c)
	})
	return e
}

// provider registers a priced (1 micro per token) provider with one model.
func (e *env) provider(p llm.Provider) {
	e.llm.Register(p, llm.ModelSpec{Model: "fake", Priced: true,
		Prices: llm.Price{InputMicrosPerMTok: 1_000_000, OutputMicrosPerMTok: 1_000_000}})
}

func (e *env) scripted(name string, turns ...llm.Turn) *llm.Scripted {
	p := llm.NewScripted(name, turns...)
	e.provider(p)
	return p
}

func bptr(b bool) *bool { return &b }

// addServer registers a live server with an in-memory MCP upstream.
func (e *env) addServer(label, project, name string, tools ...registry.ToolInfo) {
	e.t.Helper()
	conn := e.reg.Get("conn-" + label)
	if conn == nil {
		conn = registry.NewConnection("conn-"+label, label, protocol.ClientInfo{Name: "c", Version: "1", Label: label}, time.Now().UTC(), nil)
		e.reg.AddConnection(conn)
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "up", Version: "1"}, nil)
	for i := range tools {
		tools[i].InputSchema = map[string]any{"type": "object"}
		tn := tools[i].Name
		srv.AddTool(&mcp.Tool{Name: tn, InputSchema: map[string]any{"type": "object"}},
			func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: tn + " ran"}}}, nil
			})
	}
	st, ct := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	e.t.Cleanup(cancel)
	go func() { _ = srv.Run(ctx, st) }()
	sess, err := mcp.NewClient(&mcp.Implementation{Name: "hub", Version: "t"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { sess.Close() })
	ch := registry.NewServerChannel(conn.ID, label, protocol.ServerDecl{Name: name, Project: project})
	ch.SetTools(tools)
	ch.SetSession(sess)
	ch.SetState(protocol.StateRunning, "", nil)
	conn.AddServer(ch)
}

func (e *env) agent(name string, mut func(*CreateAgentInput)) *store.Agent {
	e.t.Helper()
	in := CreateAgentInput{Name: name, Approval: store.ApprovalNever}
	if mut != nil {
		mut(&in)
	}
	a, err := e.m.CreateAgent(context.Background(), in)
	if err != nil {
		e.t.Fatalf("CreateAgent %s: %v", name, err)
	}
	return a
}

func withModel(provider string) func(*CreateAgentInput) {
	return func(in *CreateAgentInput) {
		in.Model = json.RawMessage(fmt.Sprintf(`{"provider":%q,"model":"fake"}`, provider))
	}
}

func (e *env) chatOf(agentID string) string {
	e.t.Helper()
	cs, err := e.st.ListChats(context.Background(), store.ChatFilter{AgentID: agentID})
	if err != nil || len(cs) == 0 {
		e.t.Fatalf("no chat for agent %s: %v", agentID, err)
	}
	for _, c := range cs {
		if c.Kind == store.ChatKindHuman {
			return c.ID
		}
	}
	return cs[len(cs)-1].ID
}

func say(s string) []llm.Block { return []llm.Block{{Type: llm.BlockText, Text: s}} }

func (e *env) post(agentID, text string) *PostResult {
	e.t.Helper()
	res, err := e.m.Post(context.Background(), e.chatOf(agentID), PostInput{Content: say(text)})
	if err != nil {
		e.t.Fatalf("Post: %v", err)
	}
	return res
}

func terminal(s string) bool {
	return s == store.RunDone || s == store.RunCancelled || s == store.RunError || s == store.RunInterrupted
}

func (e *env) waitRun(id string) *store.Run {
	e.t.Helper()
	return e.waitRunFor(id, 10*time.Second)
}

func (e *env) waitRunFor(id string, d time.Duration) *store.Run {
	e.t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		r, err := e.st.GetRun(context.Background(), id)
		if err == nil && r != nil && terminal(r.Status) {
			return r
		}
		time.Sleep(10 * time.Millisecond)
	}
	r, _ := e.st.GetRun(context.Background(), id)
	e.t.Fatalf("run %s did not finish: %+v", id, r)
	return nil
}

func (e *env) waitStatus(id, status string) {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if r, _ := e.st.GetRun(context.Background(), id); r != nil && r.Status == status {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	r, _ := e.st.GetRun(context.Background(), id)
	e.t.Fatalf("run %s never reached %s: %+v", id, status, r)
}

func (e *env) path(chatID string) []store.Message {
	e.t.Helper()
	p, err := e.st.ActivePath(context.Background(), chatID)
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *env) callRows(f store.CallFilter) []store.CallRecord {
	rows, err := e.st.ListCalls(context.Background(), f)
	if err != nil {
		e.t.Fatal(err)
	}
	return rows
}

func roles(p []store.Message) string {
	var out []string
	for _, m := range p {
		out = append(out, m.Role)
	}
	return strings.Join(out, ",")
}

func (e *env) agentNow(id string) *store.Agent {
	a, err := e.st.GetAgent(context.Background(), id)
	if err != nil || a == nil {
		e.t.Fatalf("agent %s: %v", id, err)
	}
	return a
}

func caps(spawn, msg bool) func(*CreateAgentInput) {
	return func(in *CreateAgentInput) { in.Capabilities = store.Capabilities{CanSpawn: spawn, CanMessage: msg} }
}

func chain(fs ...func(*CreateAgentInput)) func(*CreateAgentInput) {
	return func(in *CreateAgentInput) {
		for _, f := range fs {
			f(in)
		}
	}
}

// agentOfChat is the execution record behind a chat.
func (e *env) agentOfChat(chatID string) *store.Agent {
	e.t.Helper()
	c, err := e.st.GetChat(context.Background(), chatID)
	if err != nil || c == nil {
		e.t.Fatalf("chat %s: %v", chatID, err)
	}
	return e.agentNow(c.AgentID)
}
