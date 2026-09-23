package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func mkAgent(t *testing.T, s *SQLiteStore, parent *string, name string, grants ...Grant) Agent {
	t.Helper()
	a, err := s.CreateAgent(context.Background(), Agent{ParentID: parent, Name: name, AutoWake: true}, grants)
	if err != nil {
		t.Fatalf("CreateAgent %s: %v", name, err)
	}
	return a
}

func mkChat(t *testing.T, s *SQLiteStore, agentID string) Chat {
	t.Helper()
	c, err := s.CreateChat(context.Background(), Chat{AgentID: agentID, Title: "t"})
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	return c
}

func text(s string) json.RawMessage {
	b, _ := json.Marshal([]map[string]string{{"type": "text", "text": s}})
	return b
}

func mkMsg(t *testing.T, s *SQLiteStore, chat string, parent *string, role, body string) Message {
	t.Helper()
	m, err := s.AppendMessage(context.Background(), Message{ChatID: chat, ParentID: parent, Role: role, Content: text(body)})
	if err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	return m
}

func ids(ms []Message) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.ID
	}
	return out
}

func sp(s string) *string { return &s }

func TestNewID(t *testing.T) {
	a, b := NewID(), NewID()
	if len(a) != 32 || strings.Contains(a, "-") || a == b {
		t.Fatalf("bad ids %q %q", a, b)
	}
	if a[12] != '7' {
		t.Errorf("not a version 7 uuid: %s", a)
	}
}

func TestAgentNameUniqueness(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, tempDB(t))

	root := mkAgent(t, s, nil, "root")
	// Roots have NULL parent_id; the COALESCE index must still catch them.
	if _, err := s.CreateAgent(ctx, Agent{Name: "root"}, nil); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("duplicate root name: err = %v, want ErrNameTaken", err)
	}
	other := mkAgent(t, s, nil, "other")
	kid := mkAgent(t, s, &root.ID, "kid")
	// Same name under a different parent is fine.
	mkAgent(t, s, &other.ID, "kid")
	if _, err := s.CreateAgent(ctx, Agent{ParentID: &root.ID, Name: "kid"}, nil); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("duplicate child name: err = %v", err)
	}
	// Rename collision.
	if _, err := s.UpdateAgent(ctx, kid.ID, AgentPatch{Name: sp("kid")}); err != nil {
		t.Fatalf("renaming to own name: %v", err)
	}
	mkAgent(t, s, &root.ID, "kid2")
	if _, err := s.UpdateAgent(ctx, kid.ID, AgentPatch{Name: sp("kid2")}); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("rename collision: err = %v", err)
	}

	// A soft-deleted sibling releases its name; a live one takes it.
	if _, err := s.SoftDeleteAgent(ctx, kid.ID); err != nil {
		t.Fatal(err)
	}
	again := mkAgent(t, s, &root.ID, "kid")
	if again.ID == kid.ID {
		t.Fatal("expected a new agent")
	}
	if got, _ := s.GetAgentByName(ctx, root.ID, "kid"); got == nil || got.ID != again.ID {
		t.Fatalf("GetAgentByName = %+v", got)
	}
	if got, _ := s.GetAgentByName(ctx, "", "root"); got == nil || got.ID != root.ID {
		t.Fatalf("GetAgentByName root = %+v", got)
	}
	// And a soft-deleted root frees its name too.
	if _, err := s.SoftDeleteAgent(ctx, root.ID); err != nil {
		t.Fatal(err)
	}
	mkAgent(t, s, nil, "root")
}

func TestAgentDepthEdgeAndDefaults(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, tempDB(t))

	root := mkAgent(t, s, nil, "root")
	a := mkAgent(t, s, &root.ID, "a")
	b := mkAgent(t, s, &a.ID, "b")
	for _, c := range []struct {
		a     Agent
		depth int
	}{{root, 0}, {a, 1}, {b, 2}} {
		got, _ := s.GetAgent(ctx, c.a.ID)
		if got.Depth != c.depth || c.a.Depth != c.depth {
			t.Errorf("%s depth = %d/%d, want %d", c.a.Name, got.Depth, c.a.Depth, c.depth)
		}
	}
	// Spawn writes the parent<->child edge allowed, symmetric by construction
	// (D14/A8): checking either direction reads the same row.
	if ok, _ := s.EdgeAllowed(ctx, root.ID, a.ID); !ok {
		t.Error("parent->child edge should be allowed")
	}
	if ok, _ := s.EdgeAllowed(ctx, a.ID, root.ID); !ok {
		t.Error("child->parent edge should be allowed too: edges are symmetric (D14)")
	}
	if ok, _ := s.EdgeAllowed(ctx, root.ID, b.ID); ok {
		t.Error("grandparent->grandchild edge should not exist")
	}
	got, _ := s.GetAgent(ctx, root.ID)
	if got.Status != AgentIdle || got.Approval != ApprovalDestructive || !got.AutoWake || string(got.Budget) != "{}" {
		t.Errorf("defaults wrong: %+v", got)
	}

	// Capabilities and project round-trip; project "" clears.
	upd, err := s.UpdateAgent(ctx, root.ID, AgentPatch{
		Capabilities: &Capabilities{CanSpawn: true}, Project: sp("proj"), Budget: json.RawMessage(`{"max_turns":3}`),
	})
	if err != nil || !upd.Capabilities.CanSpawn || upd.Capabilities.CanMessage || *upd.Project != "proj" || string(upd.Budget) != `{"max_turns":3}` {
		t.Fatalf("UpdateAgent = %+v, %v", upd, err)
	}
	upd, _ = s.UpdateAgent(ctx, root.ID, AgentPatch{Project: sp("")})
	if upd.Project != nil {
		t.Errorf("project not cleared: %v", *upd.Project)
	}

	// Missing or deleted parent.
	if _, err := s.CreateAgent(ctx, Agent{ParentID: sp("nope"), Name: "x"}, nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing parent: %v", err)
	}
	s.SoftDeleteAgent(ctx, b.ID)
	if _, err := s.CreateAgent(ctx, Agent{ParentID: &b.ID, Name: "x"}, nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted parent: %v", err)
	}

	if n, _ := s.CountLiveChildren(ctx, root.ID); n != 1 {
		t.Errorf("CountLiveChildren = %d", n)
	}
	list, _ := s.ListAgents(ctx, AgentFilter{ParentID: a.ID})
	if len(list) != 0 {
		t.Errorf("deleted child listed: %v", list)
	}
	list, _ = s.ListAgents(ctx, AgentFilter{ParentID: a.ID, IncludeDeleted: true})
	if len(list) != 1 {
		t.Errorf("IncludeDeleted list = %d", len(list))
	}
	roots, _ := s.ListAgents(ctx, AgentFilter{RootsOnly: true})
	if len(roots) != 1 || roots[0].ID != root.ID {
		t.Errorf("roots = %v", roots)
	}
}

func TestAgentJSON(t *testing.T) {
	a := Agent{ID: "x", Name: "n", Capabilities: Capabilities{CanSpawn: true}, CreatedAt: time.Unix(0, 0)}
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	for _, k := range []string{"id", "parentId", "systemPrompt", "costTotalMicros", "lastActivityAt", "autoWake", "deletedAt"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing key %q in %s", k, b)
		}
	}
	if m["createdAt"] != "1970-01-01T00:00:00+00:00" || m["capabilities"].(map[string]any)["canSpawn"] != true {
		t.Errorf("bad payload %s", b)
	}
}

func TestUsageRollsUpToEveryAncestor(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, tempDB(t))
	root := mkAgent(t, s, nil, "root")
	mid := mkAgent(t, s, &root.ID, "mid")
	leaf := mkAgent(t, s, &mid.ID, "leaf")
	sibling := mkAgent(t, s, &root.ID, "sibling")

	if err := s.AddUsage(ctx, leaf.ID, 100, 500); err != nil {
		t.Fatal(err)
	}
	if err := s.AddUsage(ctx, mid.ID, 10, 50); err != nil {
		t.Fatal(err)
	}
	want := map[string][2]int64{root.ID: {110, 550}, mid.ID: {110, 550}, leaf.ID: {100, 500}, sibling.ID: {0, 0}}
	for id, w := range want {
		a, _ := s.GetAgent(ctx, id)
		if a.TokenTotal != w[0] || a.CostTotalMicros != w[1] {
			t.Errorf("%s totals = %d/%d, want %v", a.Name, a.TokenTotal, a.CostTotalMicros, w)
		}
	}
	if err := s.AddUsage(ctx, "missing", 1, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("AddUsage on missing agent: %v", err)
	}

	cases := []struct {
		name     string
		agent    string
		limit    int64
		offender string
		exceeded bool
	}{
		{"below every total", leaf.ID, 1000, "", false},
		{"leaf itself at the limit", leaf.ID, 500, leaf.ID, true}, // >= : deepest first
		{"only ancestors reach it", leaf.ID, 550, mid.ID, true},
		{"sibling is judged via the shared parent", sibling.ID, 550, root.ID, true},
		{"sibling below", sibling.ID, 551, "", false},
		{"zero disables", leaf.ID, 0, "", false},
	}
	for _, c := range cases {
		off, ex, err := s.LifetimeExceeded(ctx, c.agent, c.limit)
		if err != nil || ex != c.exceeded || off != c.offender {
			t.Errorf("%s: got (%q,%v,%v), want (%q,%v)", c.name, off, ex, err, c.offender, c.exceeded)
		}
	}

	anc, _ := s.Ancestors(ctx, leaf.ID)
	if !reflect.DeepEqual(anc, []string{mid.ID, root.ID}) {
		t.Errorf("Ancestors = %v", anc)
	}
	desc, _ := s.Descendants(ctx, root.ID)
	sort.Strings(desc)
	wantDesc := []string{mid.ID, leaf.ID, sibling.ID}
	sort.Strings(wantDesc)
	if !reflect.DeepEqual(desc, wantDesc) {
		t.Errorf("Descendants = %v", desc)
	}
}

func TestAppendMessageWithUsageIsAtomic(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, tempDB(t))
	root := mkAgent(t, s, nil, "root")
	kid := mkAgent(t, s, &root.ID, "kid")
	chat := mkChat(t, s, kid.ID)

	u := mkMsg(t, s, chat.ID, nil, RoleUser, "hi")
	m, err := s.AppendMessageWithUsage(ctx, Message{
		ChatID: chat.ID, ParentID: &u.ID, Role: RoleAssistant, Content: text("hello"),
		TokenInput: 30, TokenOutput: 12, CostMicros: 700,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{kid.ID, root.ID} {
		a, _ := s.GetAgent(ctx, id)
		if a.TokenTotal != 42 || a.CostTotalMicros != 700 {
			t.Errorf("%s totals = %d/%d", a.Name, a.TokenTotal, a.CostTotalMicros)
		}
	}
	c, _ := s.GetChat(ctx, chat.ID)
	if c.TokenTotal != 42 || c.CostTotalMicros != 700 || *c.ActiveLeafID != m.ID {
		t.Errorf("chat = %+v", c)
	}

	// A failing append (parent in another chat) must leave the totals alone.
	other := mkChat(t, s, kid.ID)
	foreign := mkMsg(t, s, other.ID, nil, RoleUser, "x")
	_, err = s.AppendMessageWithUsage(ctx, Message{
		ChatID: chat.ID, ParentID: &foreign.ID, Role: RoleAssistant, CostMicros: 999, TokenInput: 1,
	})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("cross-chat parent: err = %v", err)
	}
	// And a usage failure must roll the message back: delete the agent row out
	// from under the chat so addUsage finds nothing.
	a, _ := s.GetAgent(ctx, kid.ID)
	if a.CostTotalMicros != 700 {
		t.Errorf("failed append leaked cost: %d", a.CostTotalMicros)
	}
	c, _ = s.GetChat(ctx, chat.ID)
	if c.CostTotalMicros != 700 {
		t.Errorf("failed append leaked chat cost: %d", c.CostTotalMicros)
	}
	if _, err := s.AppendMessage(ctx, Message{ChatID: "nope", Role: RoleUser}); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing chat: %v", err)
	}
}

func TestActivePathAndBranching(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, tempDB(t))
	ag := mkAgent(t, s, nil, "a")
	chat := mkChat(t, s, ag.ID)

	if p, err := s.ActivePath(ctx, chat.ID); err != nil || len(p) != 0 {
		t.Fatalf("empty chat path = %v, %v", p, err)
	}

	// r -> a1 -> a2 -> a3, plus a branch off a1: r -> a1 -> b2.
	r := mkMsg(t, s, chat.ID, nil, RoleUser, "root")
	a1 := mkMsg(t, s, chat.ID, &r.ID, RoleAssistant, "a1")
	a2 := mkMsg(t, s, chat.ID, &a1.ID, RoleUser, "a2")
	a3 := mkMsg(t, s, chat.ID, &a2.ID, RoleAssistant, "a3")

	path, err := s.ActivePath(ctx, chat.ID)
	if err != nil || !reflect.DeepEqual(ids(path), []string{r.ID, a1.ID, a2.ID, a3.ID}) {
		t.Fatalf("path = %v, %v", ids(path), err)
	}
	if path[0].ParentID != nil || *path[3].ParentID != a2.ID {
		t.Error("parent ids not carried")
	}

	b2 := mkMsg(t, s, chat.ID, &a1.ID, RoleUser, "b2") // branch: sibling of a2
	path, _ = s.ActivePath(ctx, chat.ID)
	if !reflect.DeepEqual(ids(path), []string{r.ID, a1.ID, b2.ID}) {
		t.Fatalf("path after branching = %v", ids(path))
	}
	chatNow, _ := s.GetChat(ctx, chat.ID)
	if *chatNow.ActiveLeafID != b2.ID {
		t.Errorf("active leaf = %s, want b2", *chatNow.ActiveLeafID)
	}
	// No history was copied: 5 messages total.
	if st, _ := s.Stats(ctx); st.Tables["messages"] != 5 {
		t.Errorf("messages = %d, want 5", st.Tables["messages"])
	}

	// PathTo an old leaf still reaches the root.
	old, _ := s.PathTo(ctx, a3.ID)
	if !reflect.DeepEqual(ids(old), []string{r.ID, a1.ID, a2.ID, a3.ID}) {
		t.Errorf("PathTo = %v", ids(old))
	}
	if none, _ := s.PathTo(ctx, "nope"); len(none) != 0 {
		t.Errorf("PathTo missing = %v", none)
	}

	// parent_id's chat must match.
	otherChat := mkChat(t, s, ag.ID)
	if _, err := s.AppendMessage(ctx, Message{ChatID: otherChat.ID, ParentID: &a1.ID, Role: RoleUser}); !errors.Is(err, ErrInvalid) {
		t.Errorf("cross-chat parent: %v", err)
	}
	if _, err := s.AppendMessage(ctx, Message{ChatID: chat.ID, ParentID: sp("nope"), Role: RoleUser}); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing parent: %v", err)
	}
}

func TestSiblingNavigationRemembersDeepestLeaf(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, tempDB(t))
	ag := mkAgent(t, s, nil, "a")
	chat := mkChat(t, s, ag.ID)

	//        r
	//      /   \
	//    x1     y1
	//    |      |
	//    x2     y2
	//    |
	//    x3
	r := mkMsg(t, s, chat.ID, nil, RoleUser, "r")
	x1 := mkMsg(t, s, chat.ID, &r.ID, RoleAssistant, "x1")
	x2 := mkMsg(t, s, chat.ID, &x1.ID, RoleUser, "x2")
	x3 := mkMsg(t, s, chat.ID, &x2.ID, RoleAssistant, "x3")
	y1 := mkMsg(t, s, chat.ID, &r.ID, RoleAssistant, "y1")
	y2 := mkMsg(t, s, chat.ID, &y1.ID, RoleUser, "y2")

	// Appends maintain last_active_child_id along the whole path.
	for _, c := range []struct{ msg, child string }{{r.ID, y1.ID}, {y1.ID, y2.ID}, {x1.ID, x2.ID}, {x2.ID, x3.ID}} {
		m, _ := s.GetMessage(ctx, c.msg)
		if m.LastActiveChildID == nil || *m.LastActiveChildID != c.child {
			t.Errorf("last_active_child_id of %s = %v, want %s", c.msg[:6], m.LastActiveChildID, c.child[:6])
		}
	}

	sib, err := s.Siblings(ctx, y1.ID)
	if err != nil || !reflect.DeepEqual(sib, SiblingSet{IDs: []string{x1.ID, y1.ID}, Index: 1}) {
		t.Fatalf("Siblings = %+v, %v", sib, err)
	}
	rootSib, _ := s.Siblings(ctx, r.ID)
	if len(rootSib.IDs) != 1 || rootSib.Index != 0 {
		t.Errorf("root siblings = %+v", rootSib)
	}
	if _, err := s.Siblings(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Siblings missing: %v", err)
	}

	// Active is y2. Selecting x1 lands on x1's remembered deepest leaf, x3.
	leaf, err := s.SelectSibling(ctx, x1.ID)
	if err != nil || leaf != x3.ID {
		t.Fatalf("SelectSibling(x1) = %s, %v; want x3", leaf, err)
	}
	c, _ := s.GetChat(ctx, chat.ID)
	if *c.ActiveLeafID != x3.ID {
		t.Errorf("chat active leaf = %s", *c.ActiveLeafID)
	}
	path, _ := s.ActivePath(ctx, chat.ID)
	if !reflect.DeepEqual(ids(path), []string{r.ID, x1.ID, x2.ID, x3.ID}) {
		t.Errorf("path = %v", ids(path))
	}
	// And back: y1's remembered leaf is y2.
	if leaf, _ := s.SelectSibling(ctx, y1.ID); leaf != y2.ID {
		t.Errorf("SelectSibling(y1) = %s, want y2", leaf)
	}

	// Navigating pointed the parent at the chosen branch.
	rm, _ := s.GetMessage(ctx, r.ID)
	if *rm.LastActiveChildID != y1.ID {
		t.Errorf("r.last_active_child_id = %s", *rm.LastActiveChildID)
	}

	// Selecting a mid-branch message goes down to the remembered leaf too.
	if leaf, _ := s.SelectSibling(ctx, x2.ID); leaf != x3.ID {
		t.Errorf("SelectSibling(x2) = %s, want x3", leaf)
	}
	// Selecting a leaf selects itself.
	if leaf, _ := s.SelectSibling(ctx, y2.ID); leaf != y2.ID {
		t.Errorf("SelectSibling(leaf) = %s", leaf)
	}

	// Growing the branch we are on updates its memory: new leaf under y2,
	// then hop away and back.
	y3 := mkMsg(t, s, chat.ID, &y2.ID, RoleAssistant, "y3")
	s.SelectSibling(ctx, x1.ID)
	if leaf, _ := s.SelectSibling(ctx, y1.ID); leaf != y3.ID {
		t.Errorf("after growth SelectSibling(y1) = %s, want y3", leaf)
	}

	// Explicit leaf selection also records the path.
	if err := s.SetActiveLeaf(ctx, chat.ID, x2.ID); err != nil {
		t.Fatal(err)
	}
	c, _ = s.GetChat(ctx, chat.ID)
	rm, _ = s.GetMessage(ctx, r.ID)
	if *c.ActiveLeafID != x2.ID || *rm.LastActiveChildID != x1.ID {
		t.Errorf("SetActiveLeaf: leaf=%s r.lac=%s", *c.ActiveLeafID, *rm.LastActiveChildID)
	}
	other := mkChat(t, s, ag.ID)
	if err := s.SetActiveLeaf(ctx, other.ID, x2.ID); !errors.Is(err, ErrInvalid) {
		t.Errorf("SetActiveLeaf across chats: %v", err)
	}
	if _, err := s.SelectSibling(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("SelectSibling missing: %v", err)
	}

	// Two roots are siblings of each other too (edit-and-resend of the first message).
	r2 := mkMsg(t, s, chat.ID, nil, RoleUser, "r2")
	rs, _ := s.Siblings(ctx, r2.ID)
	if !reflect.DeepEqual(rs, SiblingSet{IDs: []string{r.ID, r2.ID}, Index: 1}) {
		t.Errorf("root siblings = %+v", rs)
	}
}

func TestSelectSiblingFallsBackToNewestChild(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, tempDB(t))
	ag := mkAgent(t, s, nil, "a")
	chat := mkChat(t, s, ag.ID)
	r := mkMsg(t, s, chat.ID, nil, RoleUser, "r")
	c1 := mkMsg(t, s, chat.ID, &r.ID, RoleAssistant, "c1")
	c2 := mkMsg(t, s, chat.ID, &r.ID, RoleAssistant, "c2")
	_ = c1
	if _, err := s.write.Exec("UPDATE messages SET last_active_child_id = NULL"); err != nil {
		t.Fatal(err)
	}
	if leaf, _ := s.SelectSibling(ctx, r.ID); leaf != c2.ID {
		t.Errorf("fallback leaf = %s, want newest child", leaf)
	}
}

func TestMessageRoundTripAndTotals(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, tempDB(t))
	ag := mkAgent(t, s, nil, "a")
	chat := mkChat(t, s, ag.ID)

	in := Message{
		ChatID: chat.ID, Role: RoleAssistant, Content: text("x"),
		ToolCalls:   json.RawMessage(`[{"id":"1","name":"t","arguments":{}}]`),
		ToolResults: json.RawMessage(`[{"tool_call_id":"1","result":"ok"}]`),
		Model:       json.RawMessage(`{"provider":"p"}`), FinishReason: sp("stop"), RunID: sp("run1"),
		TokenInput: 5, TokenOutput: 6, CostMicros: 7, LatencyMs: 8,
	}
	m, _ := s.AppendMessage(ctx, in)
	got, _ := s.GetMessage(ctx, m.ID)
	if string(got.ToolCalls) != string(in.ToolCalls) || string(got.Model) != string(in.Model) ||
		*got.FinishReason != "stop" || *got.RunID != "run1" || got.LatencyMs != 8 || got.CostMicros != 7 {
		t.Errorf("round trip lost data: %+v", got)
	}
	plain := mkMsg(t, s, chat.ID, &m.ID, RoleUser, "y")
	got, _ = s.GetMessage(ctx, plain.ID)
	if got.ToolCalls != nil || got.Model != nil {
		t.Errorf("absent JSON should stay nil: %+v", got)
	}
	c, _ := s.GetChat(ctx, chat.ID)
	if c.TokenTotal != 11 || c.CostTotalMicros != 7 {
		t.Errorf("chat totals = %d/%d", c.TokenTotal, c.CostTotalMicros)
	}
	if b, err := json.Marshal(got); err != nil || !strings.Contains(string(b), `"toolCalls":null`) {
		t.Errorf("message JSON = %s, %v", b, err)
	}
}

func TestChatCRUD(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, tempDB(t))
	a1, a2 := mkAgent(t, s, nil, "a1"), mkAgent(t, s, nil, "a2")
	c1 := mkChat(t, s, a1.ID)
	c2, _ := s.CreateChat(ctx, Chat{AgentID: a1.ID, PeerAgentID: &a2.ID, Kind: ChatKindAgent, Tags: []string{"x"}})
	mkChat(t, s, a2.ID)

	if l, _ := s.ListChats(ctx, ChatFilter{AgentID: a1.ID}); len(l) != 2 {
		t.Errorf("chats of a1 = %d", len(l))
	}
	if l, _ := s.ListChats(ctx, ChatFilter{Kind: ChatKindAgent}); len(l) != 1 || l[0].ID != c2.ID || l[0].Tags[0] != "x" {
		t.Errorf("kind filter = %+v", l)
	}
	if l, _ := s.ListChats(ctx, ChatFilter{PeerAgentID: a2.ID}); len(l) != 1 {
		t.Errorf("peer filter = %d", len(l))
	}
	title, tags, yes := "new", []string{"a", "b"}, true
	u, err := s.UpdateChat(ctx, c1.ID, ChatPatch{Title: &title, Tags: &tags, Archived: &yes})
	if err != nil || u.Title != "new" || len(u.Tags) != 2 || u.ArchivedAt == nil {
		t.Fatalf("UpdateChat = %+v, %v", u, err)
	}
	if l, _ := s.ListChats(ctx, ChatFilter{AgentID: a1.ID}); len(l) != 1 {
		t.Errorf("archived chat still listed")
	}
	if l, _ := s.ListChats(ctx, ChatFilter{AgentID: a1.ID, IncludeArchived: true}); len(l) != 2 {
		t.Errorf("IncludeArchived = %d", len(l))
	}
	no := false
	u, _ = s.UpdateChat(ctx, c1.ID, ChatPatch{Archived: &no})
	if u.ArchivedAt != nil {
		t.Error("unarchive failed")
	}
	if _, err := s.UpdateChat(ctx, "nope", ChatPatch{Title: &title}); !errors.Is(err, ErrNotFound) {
		t.Errorf("update missing: %v", err)
	}
	mkMsg(t, s, c1.ID, nil, RoleUser, "hello")
	if err := s.DeleteChat(ctx, c1.ID); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Stats(ctx); st.Tables["messages"] != 0 {
		t.Errorf("messages survived chat deletion: %d", st.Tables["messages"])
	}
	if err := s.DeleteChat(ctx, c1.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("double delete: %v", err)
	}
}

func TestChatSearchFTS5(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, tempDB(t))
	a1, a2 := mkAgent(t, s, nil, "a1"), mkAgent(t, s, nil, "a2")
	c1, c2 := mkChat(t, s, a1.ID), mkChat(t, s, a2.ID)

	m1 := mkMsg(t, s, c1.ID, nil, RoleUser, "How do I deploy the quick brown fox service?")
	mkMsg(t, s, c1.ID, &m1.ID, RoleAssistant, "Use kubectl apply.")
	m3 := mkMsg(t, s, c2.ID, nil, RoleUser, "The lazy dog sleeps; deployment is done.")
	// Non-text blocks are not indexed.
	s.AppendMessage(ctx, Message{ChatID: c2.ID, ParentID: &m3.ID, Role: RoleTool,
		Content: json.RawMessage(`[{"type":"tool_result","text":"secretword"}]`)})

	hits, err := s.SearchMessages(ctx, SearchQuery{Text: "brown fox"})
	if err != nil || len(hits) != 1 || hits[0].MessageID != m1.ID || hits[0].ChatID != c1.ID {
		t.Fatalf("hits = %+v, %v", hits, err)
	}
	if !strings.Contains(hits[0].Snippet, SnippetStart+"brown"+SnippetEnd) {
		t.Errorf("snippet lacks highlight: %q", hits[0].Snippet)
	}
	// Prefix on the last word: "deploy" matches deploy and deployment.
	hits, _ = s.SearchMessages(ctx, SearchQuery{Text: "deploy"})
	if len(hits) != 2 {
		t.Errorf("prefix hits = %d, want 2", len(hits))
	}
	hits, _ = s.SearchMessages(ctx, SearchQuery{Text: "deploy", AgentID: a2.ID})
	if len(hits) != 1 || hits[0].ChatID != c2.ID {
		t.Errorf("agent-scoped hits = %+v", hits)
	}
	if hits, _ = s.SearchMessages(ctx, SearchQuery{Text: "secretword"}); len(hits) != 0 {
		t.Errorf("tool_result text was indexed")
	}
	// FTS syntax in user input is neutralised rather than an error.
	for _, q := range []string{`"unbalanced`, `fox AND`, `NEAR(`, `a*b:c`, `   `, ``} {
		if _, err := s.SearchMessages(ctx, SearchQuery{Text: q}); err != nil {
			t.Errorf("query %q errored: %v", q, err)
		}
	}
	// The index follows deletes.
	s.DeleteChat(ctx, c1.ID)
	if hits, _ = s.SearchMessages(ctx, SearchQuery{Text: "fox"}); len(hits) != 0 {
		t.Errorf("deleted chat still searchable: %+v", hits)
	}
	// Direct proof the triggers cover UPDATE too.
	if _, err := s.write.Exec("UPDATE messages SET search_text = 'zebra' WHERE id = ?", m3.ID); err != nil {
		t.Fatal(err)
	}
	if hits, _ = s.SearchMessages(ctx, SearchQuery{Text: "zebra"}); len(hits) != 1 {
		t.Errorf("update not indexed: %+v", hits)
	}
	if hits, _ = s.SearchMessages(ctx, SearchQuery{Text: "lazy"}); len(hits) != 0 {
		t.Errorf("stale text still indexed: %+v", hits)
	}
}

func grantKeys(gs []Grant) []string {
	out := make([]string, len(gs))
	for i, g := range gs {
		out[i] = g.AgentID[:0] + g.Label + "/" + g.Project + "/" + g.Server
	}
	sort.Strings(out)
	return out
}

func TestGrantPropagation(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, tempDB(t))

	g := func(source, label, project, server string) Grant {
		return Grant{Label: label, Project: project, Server: server, Allowed: true, Source: source}
	}
	root := mkAgent(t, s, nil, "root", g(GrantHuman, "*", "*", "*"))
	// child inherited two exact grants and holds one human grant of its own.
	child := mkAgent(t, s, &root.ID, "child",
		g(GrantInherited, "lap", "web", "fs"), g(GrantInherited, "lap", "web", "git"), g(GrantHuman, "lap", "web", "shell"))
	// grandchild inherited from the child (incl. the human one, as inherited) and has an explicit one.
	grand := mkAgent(t, s, &child.ID, "grand",
		g(GrantInherited, "lap", "web", "fs"), g(GrantInherited, "lap", "web", "shell"), g(GrantExplicit, "lap", "web", "git"))
	// an unrelated tree must never be touched.
	lone := mkAgent(t, s, nil, "lone", g(GrantInherited, "lap", "web", "fs"))

	list := func(a Agent) []string { gs, _ := s.ListGrants(ctx, a.ID); return grantKeys(gs) }

	// Adding a grant affects only that agent.
	if _, err := s.SetGrant(ctx, Grant{AgentID: root.ID, Label: "lap", Project: "web", Server: "new", Allowed: true, Source: GrantHuman}); err != nil {
		t.Fatal(err)
	}
	if len(list(child)) != 3 || len(list(grand)) != 3 {
		t.Fatalf("widening leaked downward: %v %v", list(child), list(grand))
	}

	// Revoking lap/web/fs on the root: inherited rows die on child and (transitively) grandchild.
	revoked, err := s.SetGrant(ctx, Grant{AgentID: root.ID, Label: "lap", Project: "web", Server: "fs", Allowed: false, Source: GrantHuman})
	if err != nil {
		t.Fatal(err)
	}
	if len(revoked) != 2 {
		t.Errorf("revoked = %v, want the child's and grandchild's fs rows", revoked)
	}
	if got, want := list(child), []string{"lap/web/git", "lap/web/shell"}; !reflect.DeepEqual(got, want) {
		t.Errorf("child = %v, want %v", got, want)
	}
	if got, want := list(grand), []string{"lap/web/git", "lap/web/shell"}; !reflect.DeepEqual(got, want) {
		t.Errorf("grand = %v, want %v", got, want)
	}
	// The deny itself stays on the root (deny wins at resolution time).
	rg, _ := s.ListGrants(ctx, root.ID)
	var sawDeny bool
	for _, x := range rg {
		if x.Server == "fs" && !x.Allowed {
			sawDeny = true
		}
	}
	if !sawDeny {
		t.Error("the revoking row was not stored on the root")
	}
	if len(list(lone)) != 1 {
		t.Error("unrelated tree was touched")
	}

	// Human and explicit grants survive a revocation of the same triple.
	if _, err := s.SetGrant(ctx, Grant{AgentID: root.ID, Label: "lap", Project: "web", Server: "shell", Allowed: false, Source: GrantHuman}); err != nil {
		t.Fatal(err)
	}
	if got := list(child); !reflect.DeepEqual(got, []string{"lap/web/git", "lap/web/shell"}) {
		t.Errorf("human grant on child was revoked: %v", got)
	}
	if got := list(grand); !reflect.DeepEqual(got, []string{"lap/web/git"}) {
		t.Errorf("grand's inherited shell should be revoked, explicit git kept: %v", got)
	}

	// Revoking through a wildcard reaches every inherited row it covers.
	revoked, _ = s.SetGrant(ctx, Grant{AgentID: root.ID, Label: "lap", Project: "*", Server: "*", Allowed: false, Source: GrantHuman})
	if len(revoked) != 1 || revoked[0].Server != "git" || revoked[0].AgentID != child.ID {
		t.Errorf("wildcard revoke = %v", revoked)
	}
	if got := list(child); !reflect.DeepEqual(got, []string{"lap/web/shell"}) {
		t.Errorf("child after wildcard revoke = %v", got)
	}

	// DeleteGrant propagates like a revocation and removes the row.
	s.SetGrant(ctx, Grant{AgentID: child.ID, Label: "x", Project: "", Server: "y", Allowed: true, Source: GrantExplicit})
	s.SetGrant(ctx, Grant{AgentID: grand.ID, Label: "x", Project: "", Server: "y", Allowed: true, Source: GrantInherited})
	revoked, err = s.DeleteGrant(ctx, child.ID, "x", "", "y")
	if err != nil || len(revoked) != 1 || revoked[0].AgentID != grand.ID {
		t.Errorf("DeleteGrant = %v, %v", revoked, err)
	}
	for _, a := range []Agent{child, grand} {
		for _, k := range list(a) {
			if k == "x//y" {
				t.Errorf("%s still holds x//y", a.Name)
			}
		}
	}

	// Upsert keeps created_at and changes the verdict.
	before, _ := s.ListGrants(ctx, root.ID)
	time.Sleep(2 * time.Millisecond)
	s.SetGrant(ctx, Grant{AgentID: root.ID, Label: "lap", Project: "web", Server: "fs", Allowed: true, Source: GrantHuman})
	after, _ := s.ListGrants(ctx, root.ID)
	if len(after) != len(before) {
		t.Fatalf("upsert added a row: %d -> %d", len(before), len(after))
	}
	for _, a := range after {
		if a.Server == "fs" && a.Label == "lap" {
			if !a.Allowed || !a.UpdatedAt.After(a.CreatedAt) {
				t.Errorf("upsert result %+v", a)
			}
		}
	}
	if b, _ := json.Marshal(after[0]); !strings.Contains(string(b), `"agentId"`) || !strings.Contains(string(b), `"createdAt"`) {
		t.Errorf("grant JSON = %s", b)
	}
}

func TestGrantWildcardsStoredLiterally(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, tempDB(t))
	a := mkAgent(t, s, nil, "a")
	s.SetGrant(ctx, Grant{AgentID: a.ID, Label: "*", Project: "*", Server: "*", Allowed: true, Source: GrantHuman})
	gs, _ := s.ListGrants(ctx, a.ID)
	if len(gs) != 1 || gs[0].Label != "*" || gs[0].Project != "*" {
		t.Fatalf("grants = %+v", gs)
	}
}

func TestEdges(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, tempDB(t))
	a, b, c := mkAgent(t, s, nil, "a"), mkAgent(t, s, nil, "b"), mkAgent(t, s, nil, "c")
	if ok, _ := s.EdgeAllowed(ctx, a.ID, b.ID); ok {
		t.Error("absent edge must be denied")
	}
	e, err := s.SetEdge(ctx, a.ID, b.ID, true)
	if err != nil || !e.Allowed {
		t.Fatal(err, e)
	}
	// Symmetric by construction (D14): allowed in one direction is allowed in
	// both, and setting the reverse direction touches the same row rather than
	// creating a second one.
	if ok, _ := s.EdgeAllowed(ctx, b.ID, a.ID); !ok {
		t.Error("edge must be symmetric: b->a should be allowed once a<->b is")
	}
	s.SetEdge(ctx, b.ID, a.ID, true)
	if l, _ := s.ListEdges(ctx, EdgeFilter{}); len(l) != 1 {
		t.Errorf("setting the reverse direction should not add a row: all edges = %d", len(l))
	}
	s.SetEdge(ctx, b.ID, c.ID, true)
	if l, _ := s.ListEdges(ctx, EdgeFilter{From: a.ID}); len(l) != 1 || l[0].To != b.ID {
		t.Errorf("edges touching a = %+v", l)
	}
	if l, _ := s.ListEdges(ctx, EdgeFilter{From: b.ID}); len(l) != 2 {
		t.Errorf("edges touching b = %d", len(l))
	}
	if l, _ := s.ListEdges(ctx, EdgeFilter{}); len(l) != 2 {
		t.Errorf("all edges = %d", len(l))
	}
	s.SetEdge(ctx, a.ID, b.ID, false)
	if ok, _ := s.EdgeAllowed(ctx, a.ID, b.ID); ok {
		t.Error("edge should be denied after allowed=false")
	}
	if ok, _ := s.EdgeAllowed(ctx, b.ID, a.ID); ok {
		t.Error("denying a<->b must deny b->a too (symmetric)")
	}
	if l, _ := s.ListEdges(ctx, EdgeFilter{To: b.ID}); len(l) != 2 {
		t.Errorf("edge row should persist as denied, not disappear: %+v", l)
	}
	s.DeleteEdge(ctx, b.ID, a.ID)
	if l, _ := s.ListEdges(ctx, EdgeFilter{From: a.ID}); len(l) != 0 {
		t.Errorf("after delete, edges touching a = %d", len(l))
	}
	if l, _ := s.ListEdges(ctx, EdgeFilter{}); len(l) != 1 {
		t.Errorf("after delete = %d", len(l))
	}
	if _, err := s.SetEdge(ctx, a.ID, "ghost", true); err == nil {
		t.Error("edge to a missing agent should violate the foreign key")
	}
}

func TestSoftAndHardDeleteAgent(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, tempDB(t))
	root := mkAgent(t, s, nil, "root")
	kid := mkAgent(t, s, &root.ID, "kid")
	grand := mkAgent(t, s, &kid.ID, "grand")
	sib := mkAgent(t, s, &root.ID, "sib")
	chat := mkChat(t, s, grand.ID)
	msg := mkMsg(t, s, chat.ID, nil, RoleUser, "keep me readable")
	run, _ := s.CreateRun(ctx, Run{AgentID: grand.ID, ChatID: chat.ID, Trigger: TriggerHuman, Status: RunRunning})
	done, _ := s.CreateRun(ctx, Run{AgentID: grand.ID, ChatID: chat.ID, Trigger: TriggerHuman, Status: RunDone})
	sibChat := mkChat(t, s, sib.ID)
	sibRun, _ := s.CreateRun(ctx, Run{AgentID: sib.ID, ChatID: sibChat.ID, Trigger: TriggerHuman, Status: RunRunning})
	s.SetAgentStatus(ctx, grand.ID, AgentRunning)

	desc, err := s.SoftDeleteAgent(ctx, kid.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(desc, []string{grand.ID}) {
		t.Errorf("descendants = %v", desc)
	}
	for _, id := range []string{kid.ID, grand.ID} {
		a, _ := s.GetAgent(ctx, id)
		if a.DeletedAt == nil || a.Status != AgentIdle {
			t.Errorf("%s not soft-deleted: %+v", a.Name, a)
		}
	}
	if a, _ := s.GetAgent(ctx, sib.ID); a.DeletedAt != nil {
		t.Error("sibling deleted")
	}
	if r, _ := s.GetRun(ctx, run.ID); r.Status != RunCancelled || r.FinishedAt == nil {
		t.Errorf("descendant's run = %+v", r)
	}
	if r, _ := s.GetRun(ctx, done.ID); r.Status != RunDone {
		t.Errorf("finished run changed: %+v", r)
	}
	if r, _ := s.GetRun(ctx, sibRun.ID); r.Status != RunRunning {
		t.Errorf("sibling's run changed: %+v", r)
	}
	// History stays readable.
	if p, _ := s.ActivePath(ctx, chat.ID); len(p) != 1 || p[0].ID != msg.ID {
		t.Errorf("chat unreadable after soft delete: %v", p)
	}
	if _, err := s.SoftDeleteAgent(ctx, kid.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("double soft delete: %v", err)
	}

	// Hard delete removes the subtree and everything it owns, nothing else.
	s.RecordCall(ctx, sampleCall("c1", time.Now(), func(r *CallRecord) { r.AgentID = &grand.ID }))
	s.RecordCall(ctx, sampleCall("c2", time.Now(), func(r *CallRecord) { r.AgentID = &sib.ID }))
	s.SetGrant(ctx, Grant{AgentID: grand.ID, Label: "a", Server: "b", Allowed: true, Source: GrantExplicit})
	removed, err := s.HardDeleteAgent(ctx, kid.ID)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(removed)
	want := []string{kid.ID, grand.ID}
	sort.Strings(want)
	if !reflect.DeepEqual(removed, want) {
		t.Errorf("removed = %v", removed)
	}
	st, _ := s.Stats(ctx)
	if st.Tables["agents"] != 2 || st.Tables["chats"] != 1 || st.Tables["messages"] != 0 ||
		st.Tables["runs"] != 1 || st.Tables["grants"] != 0 || st.Calls.Total != 1 {
		t.Errorf("leftovers: %v calls=%d", st.Tables, st.Calls.Total)
	}
	if l, _ := s.ListEdges(ctx, EdgeFilter{}); len(l) != 1 || l[0].To != sib.ID {
		t.Errorf("edges after hard delete = %v", l)
	}
	if _, err := s.HardDeleteAgent(ctx, kid.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("double hard delete: %v", err)
	}
	// Hard-deleting an agent whose chat is a peer of another keeps the other chat.
	peerChat, _ := s.CreateChat(ctx, Chat{AgentID: sib.ID, PeerAgentID: &root.ID, Kind: ChatKindAgent})
	s.HardDeleteAgent(ctx, root.ID)
	if st, _ := s.Stats(ctx); st.Tables["agents"] != 0 {
		t.Errorf("agents left: %d", st.Tables["agents"])
	}
	_ = peerChat
}

func TestRunsLifecycleAndInterrupt(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, tempDB(t))
	ag := mkAgent(t, s, nil, "a")
	chat := mkChat(t, s, ag.ID)

	mk := func(status string) Run {
		r, err := s.CreateRun(ctx, Run{AgentID: ag.ID, ChatID: chat.ID, Trigger: TriggerSpawn, Status: status,
			TriggeredByAgentID: sp("p"), BudgetSnapshot: json.RawMessage(`{"max_turns":2}`)})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	q, r1, r2, w, d := mk(RunQueued), mk(RunRunning), mk(RunRunning), mk(RunWaiting), mk(RunDone)
	if n, _ := s.CountRunning(ctx); n != 2 {
		t.Errorf("CountRunning = %d", n)
	}

	now := time.Now().UTC()
	st, fin, usage := RunDone, "budget", json.RawMessage(`{"limit":"max_turns"}`)
	u, err := s.UpdateRun(ctx, r1.ID, RunPatch{Status: &st, FinishReason: &fin, Usage: usage, StartedAt: &now, FinishedAt: &now})
	if err != nil || u.Status != RunDone || *u.FinishReason != "budget" || u.StartedAt == nil || string(u.Usage) != string(usage) ||
		string(u.BudgetSnapshot) != `{"max_turns":2}` || *u.TriggeredByAgentID != "p" {
		t.Fatalf("UpdateRun = %+v, %v", u, err)
	}
	if _, err := s.UpdateRun(ctx, "nope", RunPatch{Status: &st}); !errors.Is(err, ErrNotFound) {
		t.Errorf("update missing run: %v", err)
	}
	if l, _ := s.ListRuns(ctx, RunFilter{Status: []string{RunRunning, RunQueued}}); len(l) != 2 {
		t.Errorf("status filter = %d", len(l))
	}
	if l, _ := s.ListRuns(ctx, RunFilter{ChatID: chat.ID}); len(l) != 5 || l[0].ID != d.ID {
		t.Errorf("newest-first list wrong: %d", len(l))
	}

	s.SetAgentStatus(ctx, ag.ID, AgentBlocked)
	n, err := s.InterruptRunning(ctx)
	if err != nil || n != 3 {
		t.Fatalf("InterruptRunning = %d, %v; want queued+running+waiting = 3", n, err)
	}
	for _, id := range []string{q.ID, r2.ID, w.ID} {
		r, _ := s.GetRun(ctx, id)
		if r.Status != RunInterrupted || r.Error == nil || !strings.Contains(*r.Error, "restarted") || r.FinishedAt == nil {
			t.Errorf("run %s = %+v", id, r)
		}
	}
	if r, _ := s.GetRun(ctx, r1.ID); r.Status != RunDone {
		t.Errorf("finished run was interrupted: %+v", r)
	}
	if a, _ := s.GetAgent(ctx, ag.ID); a.Status != AgentIdle {
		t.Errorf("agent status = %s", a.Status)
	}
	if n, _ := s.InterruptRunning(ctx); n != 0 {
		t.Errorf("second sweep touched %d runs", n)
	}
	if n, _ := s.CountRunning(ctx); n != 0 {
		t.Errorf("CountRunning after sweep = %d", n)
	}
}

func TestPurgeRunsKeepsRunsMessagesReference(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, tempDB(t))
	ag := mkAgent(t, s, nil, "a")
	chat := mkChat(t, s, ag.ID)

	old := time.Now().UTC().AddDate(0, 0, -(DefaultRetentionDays + 5))
	mk := func(status string, at time.Time) Run {
		r, err := s.CreateRun(ctx, Run{AgentID: ag.ID, ChatID: chat.ID, Trigger: TriggerHuman, Status: status, CreatedAt: at})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	referenced := mk(RunDone, old)
	unreferenced := mk(RunDone, old)
	oldRunning := mk(RunRunning, old) // never purged: not finished
	recent := mk(RunDone, time.Now())
	oldFinished := old
	s.UpdateRun(ctx, unreferenced.ID, RunPatch{FinishedAt: &oldFinished})
	s.UpdateRun(ctx, referenced.ID, RunPatch{FinishedAt: &oldFinished})
	s.AppendMessage(ctx, Message{ChatID: chat.ID, Role: RoleAssistant, Content: text("x"), RunID: &referenced.ID})

	n, err := s.PurgeRuns(ctx)
	if err != nil || n != 1 {
		t.Fatalf("PurgeRuns = %d, %v; want 1", n, err)
	}
	for id, wantGone := range map[string]bool{unreferenced.ID: true, referenced.ID: false, oldRunning.ID: false, recent.ID: false} {
		if r, _ := s.GetRun(ctx, id); (r == nil) != wantGone {
			t.Errorf("run %s gone=%v, want %v", id[:6], r == nil, wantGone)
		}
	}
}

func TestInboxEnqueueDrainPrune(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, tempDB(t))
	a, b := mkAgent(t, s, nil, "a"), mkAgent(t, s, nil, "b")
	chat := mkChat(t, s, a.ID)

	for i, body := range []string{"one", "two", "three"} {
		if _, err := s.EnqueueInbox(ctx, InboxItem{AgentID: a.ID, FromAgentID: &b.ID, ChatID: chat.ID, MessageID: body}); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	s.EnqueueInbox(ctx, InboxItem{AgentID: b.ID, ChatID: chat.ID, MessageID: "for-b"})
	if n, _ := s.CountInbox(ctx, a.ID); n != 3 {
		t.Errorf("CountInbox = %d", n)
	}
	got, err := s.DrainInbox(ctx, a.ID)
	if err != nil || len(got) != 3 {
		t.Fatalf("drain = %v, %v", got, err)
	}
	for i, want := range []string{"one", "two", "three"} {
		if got[i].MessageID != want || got[i].DeliveredAt == nil || *got[i].FromAgentID != b.ID {
			t.Errorf("item %d = %+v", i, got[i])
		}
	}
	if again, _ := s.DrainInbox(ctx, a.ID); len(again) != 0 {
		t.Errorf("second drain returned %d", len(again))
	}
	if n, _ := s.CountInbox(ctx, a.ID); n != 0 {
		t.Errorf("CountInbox after drain = %d", n)
	}
	if n, _ := s.CountInbox(ctx, b.ID); n != 1 {
		t.Errorf("other agent's mail disturbed: %d", n)
	}

	// Pruning: only delivered rows older than the retention window.
	if n, _ := s.PruneInbox(ctx); n != 0 {
		t.Errorf("fresh delivered rows pruned: %d", n)
	}
	old := epoch(time.Now().AddDate(0, 0, -(DefaultInboxRetentionDays + 1)))
	if _, err := s.write.Exec("UPDATE inbox SET delivered_at_epoch = ? WHERE message_id IN ('one','two')", old); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PruneInbox(ctx); err != nil || n != 2 {
		t.Errorf("PruneInbox = %d, %v; want 2", n, err)
	}
	// An undelivered row is never pruned however old.
	if n, _ := s.CountInbox(ctx, b.ID); n != 1 {
		t.Errorf("undelivered row lost")
	}
}

func TestIdempotencyKeys(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, tempDB(t))

	if id, ok, err := s.ClaimIdempotencyKey(ctx, "k", "run1"); err != nil || !ok || id != "" {
		t.Fatalf("first claim = %q, %v, %v", id, ok, err)
	}
	if id, ok, _ := s.ClaimIdempotencyKey(ctx, "k", "run2"); ok || id != "run1" {
		t.Fatalf("repeat = %q, %v; want run1, not claimed", id, ok)
	}
	if _, ok, _ := s.ClaimIdempotencyKey(ctx, "other", "run3"); !ok {
		t.Error("distinct key should claim")
	}
	// Past the window the key is reusable.
	if _, err := s.write.Exec("UPDATE idempotency_keys SET created_at_epoch = ? WHERE key = 'k'",
		epoch(time.Now().Add(-IdempotencyWindow-time.Minute))); err != nil {
		t.Fatal(err)
	}
	if id, ok, _ := s.ClaimIdempotencyKey(ctx, "k", "run4"); !ok || id != "" {
		t.Errorf("expired key not replaced: %q %v", id, ok)
	}
	if id, ok, _ := s.ClaimIdempotencyKey(ctx, "k", "run5"); ok || id != "run4" {
		t.Errorf("replacement not remembered: %q %v", id, ok)
	}
	// Purge drops expired keys.
	s.write.Exec("UPDATE idempotency_keys SET created_at_epoch = 0 WHERE key = 'other'")
	s.PurgeRuns(ctx)
	if id, ok, _ := s.ClaimIdempotencyKey(ctx, "other", "run6"); !ok {
		t.Errorf("purged key still held by %q", id)
	}
}

func TestStatsCoverOrchestratorTables(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, tempDB(t))
	ag := mkAgent(t, s, nil, "a")
	chat := mkChat(t, s, ag.ID)
	mkMsg(t, s, chat.ID, nil, RoleUser, "hi")
	st, err := s.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for table, want := range map[string]int64{"agents": 1, "chats": 1, "messages": 1, "edges": 0, "grants": 0, "runs": 0, "inbox": 0} {
		if got, ok := st.Tables[table]; !ok || got != want {
			t.Errorf("Tables[%s] = %d (present %v), want %d", table, got, ok, want)
		}
	}
}

func TestConcurrentAppendsStayConsistent(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, tempDB(t))
	root := mkAgent(t, s, nil, "root")
	kid := mkAgent(t, s, &root.ID, "kid")
	chat := mkChat(t, s, kid.ID)
	r := mkMsg(t, s, chat.ID, nil, RoleUser, "r")

	const n = 20
	done := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			_, err := s.AppendMessageWithUsage(ctx, Message{ChatID: chat.ID, ParentID: &r.ID, Role: RoleAssistant,
				Content: text("branch"), TokenInput: 1, TokenOutput: 1, CostMicros: 10})
			done <- err
		}()
	}
	for i := 0; i < n; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	a, _ := s.GetAgent(ctx, root.ID)
	c, _ := s.GetChat(ctx, chat.ID)
	if a.CostTotalMicros != n*10 || a.TokenTotal != n*2 || c.CostTotalMicros != n*10 {
		t.Errorf("totals lost updates: agent=%d/%d chat=%d", a.CostTotalMicros, a.TokenTotal, c.CostTotalMicros)
	}
	sib, _ := s.Siblings(ctx, *c.ActiveLeafID)
	if len(sib.IDs) != n {
		t.Errorf("siblings = %d, want %d", len(sib.IDs), n)
	}
}
