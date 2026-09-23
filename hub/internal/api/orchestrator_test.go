package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/agents"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/chatstream"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/llm"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

// fakeAgents is an agents.Service that records what the handlers passed it.
type fakeAgents struct {
	allPending []agents.PendingApprovalDetail
	mu         sync.Mutex

	created   []agents.CreateAgentInput
	patches   []store.AgentPatch
	deleted   []string
	hard      []bool
	grants    []store.Grant
	revoked   []store.Grant
	edges     []store.Edge
	posts     []agents.PostInput
	branches  []agents.BranchInput
	cancelled []string
	approvals []string
	pending   []agents.PendingApproval

	err     error // returned by every method when set
	dedupe  bool
	active  int
	models  []llm.ModelSpec
	graph   *agents.GraphView
	postRun string

	profiles   []store.Profile
	profileIn  []agents.ProfileInput
	profileUp  []agents.ProfileUpdate
	profileDel []string
	chatIn     []agents.CreateChatInput
	chatUp     []agents.ChatUpdate
	chatDel    []string
	db         *store.SQLiteStore // when set, DeleteChat really deletes
	hubTools   []agents.HubTool
	chatTools  []agents.ChatTool
}

func (f *fakeAgents) lock() func() { f.mu.Lock(); return f.mu.Unlock }

func (f *fakeAgents) CreateAgent(_ context.Context, in agents.CreateAgentInput) (*store.Agent, error) {
	defer f.lock()()
	if f.err != nil {
		return nil, f.err
	}
	f.created = append(f.created, in)
	return &store.Agent{ID: "a-new", Name: in.Name, Status: "idle", Model: in.Model, Approval: "destructive",
		CreatedAt: time.Unix(0, 0), UpdatedAt: time.Unix(0, 0), LastActivityAt: time.Unix(0, 0)}, nil
}
func (f *fakeAgents) UpdateAgent(_ context.Context, id string, p store.AgentPatch) (*store.Agent, error) {
	defer f.lock()()
	if f.err != nil {
		return nil, f.err
	}
	f.patches = append(f.patches, p)
	return &store.Agent{ID: id, CreatedAt: time.Unix(0, 0), UpdatedAt: time.Unix(0, 0), LastActivityAt: time.Unix(0, 0)}, nil
}
func (f *fakeAgents) DeleteAgent(_ context.Context, id string, hard bool) error {
	defer f.lock()()
	if f.err != nil {
		return f.err
	}
	f.deleted, f.hard = append(f.deleted, id), append(f.hard, hard)
	return nil
}
func (f *fakeAgents) SetGrant(_ context.Context, g store.Grant) ([]store.Grant, error) {
	defer f.lock()()
	if f.err != nil {
		return nil, f.err
	}
	f.grants = append(f.grants, g)
	if !g.Allowed {
		return f.revoked, nil
	}
	return nil, nil
}
func (f *fakeAgents) SetEdge(_ context.Context, from, to string, allowed bool) (*store.Edge, error) {
	defer f.lock()()
	if f.err != nil {
		return nil, f.err
	}
	e := store.Edge{From: from, To: to, Allowed: allowed}
	f.edges = append(f.edges, e)
	return &e, nil
}
func (f *fakeAgents) Graph(context.Context) (*agents.GraphView, error) {
	if f.graph != nil {
		return f.graph, nil
	}
	return &agents.GraphView{Agents: []agents.GraphAgent{}, Edges: []store.Edge{}, Grants: []agents.GraphGrant{}, Servers: []agents.GraphServer{}}, nil
}
func (f *fakeAgents) Post(_ context.Context, chatID string, in agents.PostInput) (*agents.PostResult, error) {
	defer f.lock()()
	if f.err != nil {
		return nil, f.err
	}
	f.posts = append(f.posts, in)
	return &agents.PostResult{MessageID: "m-1", RunID: "run-1", Deduplicated: f.dedupe}, nil
}
func (f *fakeAgents) Branch(_ context.Context, chatID string, in agents.BranchInput) (*agents.PostResult, error) {
	defer f.lock()()
	if f.err != nil {
		return nil, f.err
	}
	f.branches = append(f.branches, in)
	return &agents.PostResult{MessageID: "m-2", RunID: "run-2"}, nil
}
func (f *fakeAgents) CancelRun(_ context.Context, id string) error {
	defer f.lock()()
	if f.err != nil {
		return f.err
	}
	f.cancelled = append(f.cancelled, id)
	return nil
}
func (f *fakeAgents) Approve(_ context.Context, runID, callID string, approved bool, reason string) error {
	defer f.lock()()
	if f.err != nil {
		return f.err
	}
	f.approvals = append(f.approvals, fmt.Sprintf("%s/%s/%v/%s", runID, callID, approved, reason))
	return nil
}
func (f *fakeAgents) PendingApprovals(string) []agents.PendingApproval { return f.pending }
func (f *fakeAgents) AllPendingApprovals(context.Context) ([]agents.PendingApprovalDetail, error) {
	return f.allPending, nil
}
func (f *fakeAgents) Models() []llm.ModelSpec { return f.models }
func (f *fakeAgents) ActiveRuns() int         { return f.active }

func (f *fakeAgents) ListProfiles(context.Context) ([]store.Profile, error) {
	defer f.lock()()
	if f.err != nil {
		return nil, f.err
	}
	return f.profiles, nil
}
func (f *fakeAgents) GetProfile(_ context.Context, id string) (*store.Profile, error) {
	defer f.lock()()
	if f.err != nil {
		return nil, f.err
	}
	return &store.Profile{ID: id, Name: "P", CreatedAt: time.Unix(0, 0), UpdatedAt: time.Unix(0, 0)}, nil
}
func (f *fakeAgents) CreateProfile(_ context.Context, in agents.ProfileInput) (*store.Profile, error) {
	defer f.lock()()
	if f.err != nil {
		return nil, f.err
	}
	f.profileIn = append(f.profileIn, in)
	return &store.Profile{ID: "p-new", Name: in.Name, SystemPrompt: in.SystemPrompt, Capabilities: in.Capabilities,
		Approval: "destructive", CreatedAt: time.Unix(0, 0), UpdatedAt: time.Unix(0, 0)}, nil
}
func (f *fakeAgents) UpdateProfile(_ context.Context, id string, in agents.ProfileUpdate) (*store.Profile, error) {
	defer f.lock()()
	if f.err != nil {
		return nil, f.err
	}
	f.profileUp = append(f.profileUp, in)
	return &store.Profile{ID: id, Name: "P", CreatedAt: time.Unix(0, 0), UpdatedAt: time.Unix(0, 0)}, nil
}
func (f *fakeAgents) DeleteProfile(_ context.Context, id string) error {
	defer f.lock()()
	if f.err != nil {
		return f.err
	}
	f.profileDel = append(f.profileDel, id)
	return nil
}
func (f *fakeAgents) CreateChat(_ context.Context, in agents.CreateChatInput) (*store.Chat, error) {
	defer f.lock()()
	if f.err != nil {
		return nil, f.err
	}
	if in.ClientLabel == "*" {
		return nil, fmt.Errorf("%w: clientLabel", agents.ErrInvalid)
	}
	f.chatIn = append(f.chatIn, in)
	var label *string
	if in.ClientLabel != "" {
		label = &in.ClientLabel
	}
	return &store.Chat{ID: "c-new", AgentID: "a-new", Kind: "human", ClientLabel: label,
		CreatedAt: time.Unix(0, 0), UpdatedAt: time.Unix(0, 0)}, nil
}
func (f *fakeAgents) UpdateChat(ctx context.Context, id string, in agents.ChatUpdate) (*store.Chat, error) {
	defer f.lock()()
	if f.err != nil {
		return nil, f.err
	}
	f.chatUp = append(f.chatUp, in)
	return &store.Chat{ID: id}, nil
}
func (f *fakeAgents) DeleteChat(ctx context.Context, id string) (int, error) {
	defer f.lock()()
	if f.err != nil {
		return 0, f.err
	}
	f.chatDel = append(f.chatDel, id)
	if f.db == nil {
		return 1, nil
	}
	del, err := f.db.DeleteChatCascade(ctx, id)
	return len(del.ChatIDs), err
}
func (f *fakeAgents) HubTools() []agents.HubTool { return f.hubTools }
func (f *fakeAgents) ChatTools(_ context.Context, id string) (*agents.ChatToolsView, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &agents.ChatToolsView{Tools: f.chatTools}, nil
}

func (f *fakeAgents) ChatSystemPrompt(_ context.Context, id string) (*agents.SystemPromptView, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &agents.SystemPromptView{SystemPrompt: "sp " + id, Source: "agent", ToolCount: 2}, nil
}

func (f *fakeAgents) Answer(context.Context, string, string, []agents.QuestionAnswer) error {
	return f.err
}
func (f *fakeAgents) ListSkills(context.Context) ([]store.Skill, error) { return nil, f.err }
func (f *fakeAgents) CreateSkill(context.Context, agents.SkillInput) (*store.Skill, error) {
	return nil, f.err
}
func (f *fakeAgents) UpdateSkill(context.Context, string, agents.SkillUpdate) (*store.Skill, error) {
	return nil, f.err
}
func (f *fakeAgents) DeleteSkill(context.Context, string) error { return f.err }

var _ agents.Service = (*fakeAgents)(nil)

type orchFixture struct {
	*fixture
	db     *store.SQLiteStore
	agents *fakeAgents
	hub    *chatstream.Hub
}

func newOrch(t *testing.T) *orchFixture {
	t.Helper()
	db, err := store.Open(context.Background(), store.Options{Path: filepath.Join(t.TempDir(), "hub.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	o := &orchFixture{db: db, agents: &fakeAgents{db: db}, hub: chatstream.NewHub()}
	t.Cleanup(o.hub.Close)
	o.fixture = newFixture(t, func(opts *Options) {
		opts.Store = db
		opts.Agents = o.agents
		opts.Streams = o.hub
	})
	return o
}

func (o *orchFixture) agent(t *testing.T, name string) store.Agent {
	t.Helper()
	a, err := o.db.CreateAgent(context.Background(), store.Agent{Name: name, Status: "idle", AutoWake: true, Approval: "never"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func (o *orchFixture) chat(t *testing.T, agentID, title string) store.Chat {
	t.Helper()
	c, err := o.db.CreateChat(context.Background(), store.Chat{AgentID: agentID, Title: title})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (o *orchFixture) msg(t *testing.T, chatID string, parent *string, role, text string) store.Message {
	t.Helper()
	body, _ := json.Marshal([]map[string]string{{"type": "text", "text": text}})
	m, err := o.db.AppendMessage(context.Background(), store.Message{ChatID: chatID, ParentID: parent, Role: role, Content: body})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func (o *orchFixture) doH(t *testing.T, method, target, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	o.handler.ServeHTTP(rec, req)
	return rec
}

func TestOrchestratorRoutes404WhenDisabled(t *testing.T) {
	f := newFixture(t, nil)
	for _, c := range [][2]string{
		{"GET", "/api/agents"}, {"POST", "/api/agents"}, {"GET", "/api/graph"}, {"GET", "/api/chats"},
		{"GET", "/api/chats/x/stream"}, {"GET", "/api/runs/x"}, {"GET", "/api/models"},
		{"POST", "/api/runs/x/cancel"},
	} {
		requireStatus(t, f.do(t, c[0], c[1], ""), http.StatusNotFound)
	}
	// Stats stays and carries no orchestrator counters.
	rec := f.do(t, "GET", "/api/stats", "")
	requireStatus(t, rec, 200)
	if strings.Contains(rec.Body.String(), "activeRuns") {
		t.Fatalf("stats leaked orchestrator fields: %s", rec.Body.String())
	}
}

func TestAgentCRUD(t *testing.T) {
	o := newOrch(t)
	rec := o.do(t, "POST", "/api/agents", `{"name":" bob ","systemPrompt":"hi","model":{"provider":"p","model":"m"},
		"capabilities":{"canSpawn":true},"grants":[{"label":"*","project":"","server":"*","allowed":true}]}`)
	requireStatus(t, rec, 201)
	in := o.agents.created[0]
	if in.Name != "bob" || in.SystemPrompt != "hi" || !in.Capabilities.CanSpawn || len(in.Grants) != 1 || in.Grants[0].Server != "*" {
		t.Fatalf("input %+v", in)
	}
	requireStatus(t, o.do(t, "POST", "/api/agents", `{"name":""}`), 400)
	requireStatus(t, o.do(t, "POST", "/api/agents", `{"name":"x","approval":"sometimes"}`), 400)
	requireStatus(t, o.do(t, "POST", "/api/agents", `{bad`), 400)

	o.agents.err = fmt.Errorf("create: %w", store.ErrNameTaken)
	requireStatus(t, o.do(t, "POST", "/api/agents", `{"name":"x"}`), 409)
	o.agents.err = fmt.Errorf("parent: %w", store.ErrNotFound)
	requireStatus(t, o.do(t, "POST", "/api/agents", `{"name":"x"}`), 404)
	o.agents.err = fmt.Errorf("depth: %w", store.ErrInvalid)
	requireStatus(t, o.do(t, "POST", "/api/agents", `{"name":"x"}`), 400)
	o.agents.err = errors.New("boom")
	rec = o.do(t, "POST", "/api/agents", `{"name":"x"}`)
	requireStatus(t, rec, 500)
	if strings.Contains(rec.Body.String(), "boom") {
		t.Fatal("internal error detail leaked")
	}
	o.agents.err = nil

	a := o.agent(t, "alice")
	rec = o.do(t, "GET", "/api/agents", "")
	requireStatus(t, rec, 200)
	var list struct{ Agents []map[string]any }
	decode(t, rec, &list)
	if len(list.Agents) != 1 || list.Agents[0]["name"] != "alice" {
		t.Fatalf("list %v", list)
	}
	requireStatus(t, o.do(t, "GET", "/api/agents/"+a.ID, ""), 200)
	requireStatus(t, o.do(t, "GET", "/api/agents/nope", ""), 404)

	requireStatus(t, o.do(t, "PATCH", "/api/agents/"+a.ID, `{"description":"d","project":null,"autoWake":false}`), 200)
	p := o.agents.patches[0]
	if *p.Description != "d" || p.Project == nil || *p.Project != "" || *p.AutoWake {
		t.Fatalf("patch %+v", p)
	}
	requireStatus(t, o.do(t, "PATCH", "/api/agents/"+a.ID, `{"project":"proj"}`), 200)
	if *o.agents.patches[1].Project != "proj" {
		t.Fatal("project")
	}
	requireStatus(t, o.do(t, "PATCH", "/api/agents/"+a.ID, `{"name":" "}`), 400)

	requireStatus(t, o.do(t, "DELETE", "/api/agents/"+a.ID, ""), 204)
	requireStatus(t, o.do(t, "DELETE", "/api/agents/"+a.ID+"?hard=1", ""), 204)
	if o.agents.hard[0] || !o.agents.hard[1] {
		t.Fatalf("hard = %v", o.agents.hard)
	}
}

func TestGrantsAndEdges(t *testing.T) {
	o := newOrch(t)
	a := o.agent(t, "a")
	if _, err := o.db.SetGrant(context.Background(), store.Grant{AgentID: a.ID, Label: "l", Server: "s", Allowed: true, Source: "explicit"}); err != nil {
		t.Fatal(err)
	}
	rec := o.do(t, "GET", "/api/agents/"+a.ID+"/grants", "")
	requireStatus(t, rec, 200)
	var g struct{ Grants []store.Grant }
	if err := json.Unmarshal(rec.Body.Bytes(), &g); err != nil || len(g.Grants) != 1 {
		t.Fatalf("%v %s", err, rec.Body.String())
	}
	requireStatus(t, o.do(t, "GET", "/api/agents/nope/grants", ""), 404)

	o.agents.revoked = []store.Grant{{AgentID: "child", Label: "l", Server: "s"}}
	rec = o.do(t, "PUT", "/api/agents/"+a.ID+"/grants", `{"label":"l","project":"","server":"s","allowed":false}`)
	requireStatus(t, rec, 200)
	if got := o.agents.grants[0]; got.AgentID != a.ID || got.Source != "human" || got.Allowed {
		t.Fatalf("grant %+v", got)
	}
	var out struct{ Revoked []store.Grant }
	decode(t, rec, &out)
	if len(out.Revoked) != 1 || out.Revoked[0].AgentID != "child" {
		t.Fatalf("revoked %s", rec.Body.String())
	}
	requireStatus(t, o.do(t, "PUT", "/api/agents/"+a.ID+"/grants",
		`{"grants":[{"label":"*","server":"*","allowed":true},{"label":"x","server":"y","allowed":true}]}`), 200)
	if len(o.agents.grants) != 3 {
		t.Fatalf("grants %d", len(o.agents.grants))
	}
	requireStatus(t, o.do(t, "PUT", "/api/agents/"+a.ID+"/grants", `{}`), 400)
	requireStatus(t, o.do(t, "PUT", "/api/agents/"+a.ID+"/grants", `{"grants":[{"label":"","server":"y"}]}`), 400)

	requireStatus(t, o.do(t, "PUT", "/api/graph/edges/a/b", `{"allowed":true}`), 200)
	if e := o.agents.edges[0]; e.From != "a" || e.To != "b" || !e.Allowed {
		t.Fatalf("edge %+v", e)
	}
	requireStatus(t, o.do(t, "PUT", "/api/graph/edges/a/b", `{}`), 400)
	rec = o.do(t, "GET", "/api/graph", "")
	requireStatus(t, rec, 200)
	if !strings.Contains(rec.Body.String(), `"servers"`) {
		t.Fatal(rec.Body.String())
	}
}

func TestChatsListCreatePatchDelete(t *testing.T) {
	o := newOrch(t)
	a, b := o.agent(t, "a"), o.agent(t, "b")
	c1 := o.chat(t, a.ID, "Deploy plan")
	c2 := o.chat(t, a.ID, "Other")
	c3 := o.chat(t, b.ID, "Bee chat")
	o.msg(t, c2.ID, nil, "user", "please discuss kubernetes rollout")
	if _, err := o.db.UpdateChat(context.Background(), c3.ID, store.ChatPatch{Tags: &[]string{"work"}}); err != nil {
		t.Fatal(err)
	}

	type page struct {
		Chats []store.Chat
		Hits  map[string][]store.SearchHit
		Limit int
	}
	get := func(q string) page {
		rec := o.do(t, "GET", "/api/chats"+q, "")
		requireStatus(t, rec, 200)
		var p page
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if n := len(get("").Chats); n != 3 {
		t.Fatalf("all = %d", n)
	}
	if n := len(get("?agentId=" + a.ID).Chats); n != 2 {
		t.Fatalf("agent = %d", n)
	}
	if p := get("?tag=work"); len(p.Chats) != 1 || p.Chats[0].ID != c3.ID {
		t.Fatalf("tag %v", p.Chats)
	}
	p := get("?q=deploy")
	if len(p.Chats) != 1 || p.Chats[0].ID != c1.ID {
		t.Fatalf("title q %v", p.Chats)
	}
	p = get("?q=kuber")
	if len(p.Chats) != 1 || p.Chats[0].ID != c2.ID || len(p.Hits[c2.ID]) != 1 {
		t.Fatalf("message q %+v", p)
	}
	if n := len(get("?q=kuber&agentId=" + b.ID).Chats); n != 0 {
		t.Fatalf("agent-scoped q = %d", n)
	}
	if n := len(get("?limit=1").Chats); n != 1 {
		t.Fatalf("limit = %d", n)
	}

	rec := o.do(t, "POST", "/api/chats", `{"agentId":"`+a.ID+`","title":"New"}`)
	requireStatus(t, rec, 201)
	var created store.Chat
	decode(t, rec, &created)
	if created.Kind != "human" || created.Title != "New" || created.AgentID != a.ID {
		t.Fatalf("%+v", created)
	}
	requireStatus(t, o.do(t, "POST", "/api/chats", `{"clientLabel":"*"}`), 400)
	requireStatus(t, o.do(t, "POST", "/api/chats", `{"agentId":"nope"}`), 404)
	requireStatus(t, o.do(t, "POST", "/api/chats", `{"agentId":"`+a.ID+`","kind":"agent"}`), 400)

	requireStatus(t, o.do(t, "GET", "/api/chats/"+c1.ID, ""), 200)
	requireStatus(t, o.do(t, "GET", "/api/chats/nope", ""), 404)
	rec = o.do(t, "PATCH", "/api/chats/"+c1.ID, `{"title":"Renamed","tags":["x","y"],"archived":true}`)
	requireStatus(t, rec, 200)
	var pc store.Chat
	decode(t, rec, &pc)
	if pc.Title != "Renamed" || len(pc.Tags) != 2 || pc.ArchivedAt == nil {
		t.Fatalf("%+v", pc)
	}
	if n := len(get("").Chats); n != 3 { // c1 archived, +created
		t.Fatalf("after archive = %d", n)
	}
	if n := len(get("?includeArchived=1").Chats); n != 4 {
		t.Fatalf("with archived = %d", n)
	}
	rec = o.do(t, "DELETE", "/api/chats/"+c1.ID, "")
	requireStatus(t, rec, 200)
	if strings.TrimSpace(rec.Body.String()) != `{"deletedChats":1}` {
		t.Fatalf("delete body = %s", rec.Body.String())
	}
	requireStatus(t, o.do(t, "GET", "/api/chats/"+c1.ID, ""), 404)
	requireStatus(t, o.do(t, "DELETE", "/api/chats/"+c1.ID, ""), 404)
}

type msgList struct {
	ActiveLeafID *string `json:"activeLeafId"`
	Tree         bool
	Messages     []struct {
		ID       string
		ParentID *string
		Siblings store.SiblingSet
		Role     string
	}
}

func TestMessagesPathLeafTreeAndLeafPatch(t *testing.T) {
	o := newOrch(t)
	a := o.agent(t, "a")
	c := o.chat(t, a.ID, "t")
	root := o.msg(t, c.ID, nil, "user", "hi")
	a1 := o.msg(t, c.ID, &root.ID, "assistant", "one")
	a2 := o.msg(t, c.ID, &root.ID, "assistant", "two") // sibling branch, now active
	other := o.chat(t, a.ID, "other")
	foreign := o.msg(t, other.ID, nil, "user", "x")

	get := func(q string) msgList {
		rec := o.do(t, "GET", "/api/chats/"+c.ID+"/messages"+q, "")
		requireStatus(t, rec, 200)
		var l msgList
		decode(t, rec, &l)
		return l
	}
	l := get("")
	if len(l.Messages) != 2 || l.Messages[1].ID != a2.ID || l.Tree {
		t.Fatalf("active path %+v", l)
	}
	if s := l.Messages[1].Siblings; len(s.IDs) != 2 || s.Index != 1 {
		t.Fatalf("siblings %+v", s)
	}
	if *l.ActiveLeafID != a2.ID {
		t.Fatal("activeLeafId")
	}
	l = get("?leaf=" + a1.ID)
	if len(l.Messages) != 2 || l.Messages[1].ID != a1.ID || l.Messages[1].Siblings.Index != 0 {
		t.Fatalf("leaf path %+v", l)
	}
	l = get("?tree=1")
	if !l.Tree || len(l.Messages) != 3 {
		t.Fatalf("tree %+v", l)
	}
	if s := l.Messages[0].Siblings; len(s.IDs) != 1 || s.Index != 0 {
		t.Fatalf("root siblings %+v", s)
	}
	if s := l.Messages[1].Siblings; len(s.IDs) != 2 || s.Index != 0 {
		t.Fatalf("tree siblings %+v", s)
	}
	requireStatus(t, o.do(t, "GET", "/api/chats/"+c.ID+"/messages?leaf="+foreign.ID, ""), 404)
	requireStatus(t, o.do(t, "GET", "/api/chats/nope/messages", ""), 404)

	rec := o.do(t, "PATCH", "/api/chats/"+c.ID, `{"activeLeafId":"`+a1.ID+`"}`)
	requireStatus(t, rec, 200)
	var pc store.Chat
	decode(t, rec, &pc)
	if *pc.ActiveLeafID != a1.ID {
		t.Fatalf("leaf %v", pc.ActiveLeafID)
	}
	if l = get(""); l.Messages[1].ID != a1.ID {
		t.Fatal("path did not follow the leaf")
	}
	if rec = o.do(t, "PATCH", "/api/chats/"+c.ID, `{"activeLeafId":"`+foreign.ID+`"}`); rec.Code < 400 {
		t.Fatalf("cross-chat leaf accepted: %d", rec.Code)
	}
	rec = o.do(t, "PATCH", "/api/chats/"+c.ID, `{"selectMessageId":"`+a2.ID+`"}`)
	requireStatus(t, rec, 200)
	decode(t, rec, &pc)
	if *pc.ActiveLeafID != a2.ID {
		t.Fatalf("select sibling leaf %v", pc.ActiveLeafID)
	}
}

func TestPostIdempotencyAndBranch(t *testing.T) {
	o := newOrch(t)
	a := o.agent(t, "a")
	c := o.chat(t, a.ID, "t")

	rec := o.doH(t, "POST", "/api/chats/"+c.ID+"/messages", `{"content":"hello"}`, map[string]string{"Idempotency-Key": "k-1"})
	requireStatus(t, rec, 202)
	var res agents.PostResult
	decode(t, rec, &res)
	if res.MessageID != "m-1" || res.RunID != "run-1" {
		t.Fatalf("%+v", res)
	}
	in := o.agents.posts[0]
	if in.IdempotencyKey != "k-1" || len(in.Content) != 1 || in.Content[0].Text != "hello" || in.Content[0].Type != "text" {
		t.Fatalf("post input %+v", in)
	}
	o.agents.dedupe = true
	rec = o.do(t, "POST", "/api/chats/"+c.ID+"/messages", `{"content":[{"type":"text","text":"b"}],"parentId":"p","model":{"model":"m"}}`)
	requireStatus(t, rec, 202)
	if !strings.Contains(rec.Body.String(), `"deduplicated":true`) {
		t.Fatal(rec.Body.String())
	}
	in = o.agents.posts[1]
	if in.IdempotencyKey != "" || in.ParentID != "p" || string(in.Model) != `{"model":"m"}` {
		t.Fatalf("post input %+v", in)
	}
	requireStatus(t, o.do(t, "POST", "/api/chats/"+c.ID+"/messages", `{"content":"  "}`), 400)
	requireStatus(t, o.do(t, "POST", "/api/chats/"+c.ID+"/messages", `{"content":5}`), 400)
	o.agents.err = store.ErrNotFound
	requireStatus(t, o.do(t, "POST", "/api/chats/nope/messages", `{"content":"x"}`), 404)
	o.agents.err = nil

	requireStatus(t, o.do(t, "POST", "/api/chats/"+c.ID+"/branch", `{}`), 400)
	rec = o.do(t, "POST", "/api/chats/"+c.ID+"/branch", `{"fromMessageId":"m9","content":"edited"}`)
	requireStatus(t, rec, 202)
	if b := o.agents.branches[0]; b.FromMessageID != "m9" || b.Content[0].Text != "edited" {
		t.Fatalf("%+v", b)
	}
	rec = o.do(t, "POST", "/api/chats/"+c.ID+"/branch", `{"fromMessageId":"m9"}`)
	requireStatus(t, rec, 202)
	if len(o.agents.branches[1].Content) != 0 {
		t.Fatal("regenerate carried content")
	}
}

func TestRunsApprovalsModelsStats(t *testing.T) {
	o := newOrch(t)
	a := o.agent(t, "a")
	c := o.chat(t, a.ID, "t")
	run, err := o.db.CreateRun(context.Background(), store.Run{AgentID: a.ID, ChatID: c.ID, Trigger: "human", Status: "running"})
	if err != nil {
		t.Fatal(err)
	}
	o.agents.pending = []agents.PendingApproval{{CallID: "c1", Tool: "fs.write", Arguments: map[string]any{"p": 1}, ExpiresAt: "2026-01-01T00:00:00+00:00"}}
	rec := o.do(t, "GET", "/api/runs/"+run.ID, "")
	requireStatus(t, rec, 200)
	var body map[string]any
	decode(t, rec, &body)
	pa := body["pendingApprovals"].([]any)
	if body["id"] != run.ID || body["status"] != "running" || len(pa) != 1 || pa[0].(map[string]any)["callId"] != "c1" {
		t.Fatalf("%v", body)
	}
	requireStatus(t, o.do(t, "GET", "/api/runs/nope", ""), 404)

	requireStatus(t, o.do(t, "POST", "/api/runs/"+run.ID+"/cancel", ""), 204)
	requireStatus(t, o.do(t, "POST", "/api/runs/nope/cancel", ""), 404)
	if o.agents.cancelled[0] != run.ID {
		t.Fatal("cancel")
	}

	requireStatus(t, o.do(t, "POST", "/api/runs/r/approvals/c1", `{"approved":false,"reason":"no"}`), 204)
	if o.agents.approvals[0] != "r/c1/false/no" {
		t.Fatalf("%v", o.agents.approvals)
	}
	requireStatus(t, o.do(t, "POST", "/api/runs/r/approvals/c1", `{}`), 400)
	o.agents.err = errors.New("agents: approval not pending")
	requireStatus(t, o.do(t, "POST", "/api/runs/r/approvals/c1", `{"approved":true}`), 409)
	o.agents.err = nil

	yes := true
	o.agents.models = []llm.ModelSpec{{Provider: "fake", Model: "m"},
		{Provider: "openai-compatible", Model: "q", ContextWindow: 32768, SupportsTools: &yes, Discovered: true, Priced: true}}
	rec = o.do(t, "GET", "/api/models", "")
	requireStatus(t, rec, 200)
	if !strings.Contains(rec.Body.String(), `"provider":"fake"`) ||
		!strings.Contains(rec.Body.String(), `"contextWindow":32768,"supportsTools":true,"discovered":true`) {
		t.Fatal(rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"model":"m","prices":{"input_micros_per_mtok":0,"output_micros_per_mtok":0,"cache_read_micros_per_mtok":0,"cache_write_micros_per_mtok":0},"contextWindow"`) {
		t.Fatal("contextWindow must be omitted when unknown")
	}

	o.agents.active = 3
	rec = o.do(t, "GET", "/api/stats", "")
	requireStatus(t, rec, 200)
	var st struct {
		ActiveRuns        *int             `json:"activeRuns"`
		ChatStreamClients *int             `json:"chatStreamClients"`
		Tables            map[string]int64 `json:"tables"`
	}
	decode(t, rec, &st)
	if st.ActiveRuns == nil || *st.ActiveRuns != 3 || st.ChatStreamClients == nil || st.Tables["agents"] != 1 {
		t.Fatalf("%s", rec.Body.String())
	}
}

func TestExport(t *testing.T) {
	o := newOrch(t)
	a := o.agent(t, "a")
	c := o.chat(t, a.ID, "My chat")
	root := o.msg(t, c.ID, nil, "user", "question?")
	o.msg(t, c.ID, &root.ID, "assistant", "dropped branch")
	calls := json.RawMessage(`[{"id":"t1","name":"fs.read","arguments":{"path":"/x"}}]`)
	m, err := o.db.AppendMessage(context.Background(), store.Message{ChatID: c.ID, ParentID: &root.ID, Role: "assistant",
		Content: json.RawMessage(`[{"type":"text","text":"answer"}]`), ToolCalls: calls})
	if err != nil {
		t.Fatal(err)
	}

	rec := o.do(t, "GET", "/api/chats/"+c.ID+"/export?format=json", "")
	requireStatus(t, rec, 200)
	var j struct {
		Chat         store.Chat
		Messages     []json.RawMessage
		SystemPrompt *agents.SystemPromptView
		Tools        *agents.ChatToolsView
	}
	decode(t, rec, &j)
	if j.Chat.ID != c.ID || len(j.Messages) != 3 {
		t.Fatalf("json export %s", rec.Body.String())
	}
	// The system prompt and tool catalog are resolved live, the same way the
	// System prompt and Tools panels do, so an export is a self-contained
	// snapshot of everything the model actually saw, not just the transcript.
	if j.SystemPrompt == nil || j.Tools == nil {
		t.Fatalf("export must carry the resolved system prompt and tool catalog: %s", rec.Body.String())
	}
	rec = o.do(t, "GET", "/api/chats/"+c.ID+"/export?format=markdown", "")
	requireStatus(t, rec, 200)
	md := rec.Body.String()
	if !strings.HasPrefix(md, "# My chat") || !strings.Contains(md, "question?") || !strings.Contains(md, "answer") ||
		!strings.Contains(md, "fs.read") || strings.Contains(md, "dropped branch") || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/markdown") {
		t.Fatalf("markdown:\n%s (leaf %s)", md, m.ID)
	}
	requireStatus(t, o.do(t, "GET", "/api/chats/"+c.ID+"/export?format=pdf", ""), 400)
	requireStatus(t, o.do(t, "GET", "/api/chats/nope/export", ""), 404)
}

// TestChatStreamResumeEndToEnd drives the real handler over a socket: the
// frames published before the client connects are replayed after Last-Event-ID.
func TestChatStreamResumeEndToEnd(t *testing.T) {
	o := newOrch(t)
	a := o.agent(t, "a")
	c := o.chat(t, a.ID, "t")
	srv := httptest.NewServer(o.handler)
	defer srv.Close()

	requireStatus(t, o.do(t, "GET", "/api/chats/nope/stream", ""), 404)

	o.hub.Publish(c.ID, "runA", "run_started", map[string]any{"agentId": a.ID})
	for i := 0; i < 3; i++ {
		o.hub.Publish(c.ID, "runA", "delta", map[string]any{"messageId": "m", "contentIndex": 0, "text": fmt.Sprint(i)})
	}

	connect := func(last string) (*bufio.Scanner, func()) {
		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/chats/"+c.ID+"/stream", nil)
		if last != "" {
			req.Header.Set("Last-Event-ID", last)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.Header.Get("X-Accel-Buffering") != "no" || resp.Header.Get("Cache-Control") != "no-cache, no-transform" {
			t.Fatalf("headers %v", resp.Header)
		}
		return bufio.NewScanner(resp.Body), func() { cancel(); resp.Body.Close() }
	}
	readEvent := func(sc *bufio.Scanner) (id, event, data string) {
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "" && event != "":
				return
			case strings.HasPrefix(line, "id: "):
				id = line[4:]
			case strings.HasPrefix(line, "event: "):
				event = line[7:]
			case strings.HasPrefix(line, "data: "):
				data = line[6:]
			}
		}
		t.Fatal("stream ended early")
		return
	}

	sc, closeConn := connect("runA:2")
	id, ev, data := readEvent(sc)
	if id != "runA:3" || ev != "delta" || !strings.Contains(data, `"text":"1"`) || !strings.Contains(data, `"seq":3`) {
		t.Fatalf("%s %s %s", id, ev, data)
	}
	id, _, _ = readEvent(sc)
	if id != "runA:4" {
		t.Fatalf("id %s", id)
	}
	// Live frame after the backlog, then the run ends.
	o.hub.Publish(c.ID, "runA", "run_done", map[string]any{"status": "done"})
	if id, ev, _ = readEvent(sc); id != "runA:5" || ev != "run_done" {
		t.Fatalf("%s %s", id, ev)
	}
	closeConn()

	// Unknown position: explicit overflow, no id.
	sc, closeConn = connect("gone:9")
	id, ev, _ = readEvent(sc)
	if ev != "overflow" || id != "" {
		t.Fatalf("%q %q", id, ev)
	}
	closeConn()

	deadline := time.Now().Add(2 * time.Second)
	for o.hub.Subscribers() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("subscriber leaked")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestListApprovals(t *testing.T) {
	o := newOrch(t)
	rec := o.do(t, "GET", "/api/approvals", "")
	requireStatus(t, rec, 200)
	if strings.TrimSpace(rec.Body.String()) != `{"approvals":[]}` {
		t.Fatalf("empty body = %s", rec.Body.String())
	}
	o.agents.allPending = []agents.PendingApprovalDetail{{
		RunID: "r1", ChatID: "c1", AgentID: "a1", AgentName: "bot", ChatTitle: "hello",
		PendingApproval: agents.PendingApproval{CallID: "k1", Tool: "fs__write", Arguments: map[string]any{"p": 1}, ExpiresAt: "2026-01-01T00:00:00+00:00"},
	}}
	rec = o.do(t, "GET", "/api/approvals", "")
	requireStatus(t, rec, 200)
	var body map[string][]map[string]any
	decode(t, rec, &body)
	got := body["approvals"]
	if len(got) != 1 {
		t.Fatalf("%v", body)
	}
	for k, want := range map[string]string{"runId": "r1", "chatId": "c1", "agentId": "a1", "agentName": "bot", "chatTitle": "hello", "callId": "k1", "tool": "fs__write", "expiresAt": "2026-01-01T00:00:00+00:00"} {
		if got[0][k] != want {
			t.Errorf("%s = %v, want %s", k, got[0][k], want)
		}
	}
	if got[0]["arguments"].(map[string]any)["p"] != float64(1) {
		t.Errorf("arguments = %v", got[0]["arguments"])
	}
}
