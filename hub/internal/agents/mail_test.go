package agents

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/llm"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

func lastIsTool(msgs []llm.Message) bool {
	return len(msgs) > 0 && msgs[len(msgs)-1].Role == llm.RoleTool
}

// (V6a, A9/B5) Two agents with a symmetric allowed edge, each answering the
// other, terminate on the lifetime cost ceiling in bounded time.
func TestSymmetricEdgeCycleTerminatesOnLifetimeCeiling(t *testing.T) {
	e := newEnv(t)
	var a, b *store.Agent
	script := func(peer **store.Agent) func(int, []llm.Message) llm.Turn {
		return func(_ int, msgs []llm.Message) llm.Turn {
			if lastIsTool(msgs) {
				return llm.Say("sent").WithUsage(500, 500)
			}
			return llm.CallTool("switchboard_chat_send", map[string]any{"to_chat_id": e.chatOf((*peer).ID), "message": "ping"}).WithUsage(500, 500)
		}
	}
	pa, pb := llm.NewScriptedFunc("pa", script(&b)), llm.NewScriptedFunc("pb", script(&a))
	e.provider(pa)
	e.provider(pb)
	budget := func(in *CreateAgentInput) { in.Budget = []byte(`{"max_lifetime_cost_micros":3000}`) }
	a = e.agent("A", chain(withModel("pa"), caps(false, true), budget))
	b = e.agent("B", chain(withModel("pb"), caps(false, true), budget))
	for _, p := range [][2]string{{a.ID, b.ID}, {b.ID, a.ID}} {
		if _, err := e.m.SetEdge(context.Background(), p[0], p[1], true); err != nil {
			t.Fatal(err)
		}
	}

	start := time.Now()
	e.post(a.ID, "start")

	// Wait for quiescence: nothing active for a moment.
	deadline := time.Now().Add(15 * time.Second)
	quiet := 0
	for quiet < 20 {
		if time.Now().After(deadline) {
			t.Fatalf("cycle did not terminate; runs=%d, A=%d B=%d", e.m.ActiveRuns(), e.agentNow(a.ID).CostTotalMicros, e.agentNow(b.ID).CostTotalMicros)
		}
		if e.m.ActiveRuns() == 0 {
			quiet++
		} else {
			quiet = 0
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Logf("terminated in %s", time.Since(start))
	for _, ag := range []*store.Agent{a, b} {
		got := e.agentNow(ag.ID).CostTotalMicros
		if got < 3000 || got > 3000+2000 {
			t.Errorf("%s spent %d: expected to stop right after the 3000 ceiling", ag.Name, got)
		}
		runs, _ := e.st.ListRuns(context.Background(), store.RunFilter{AgentID: ag.ID})
		if len(runs) < 2 {
			t.Errorf("%s made only %d runs: the cycle never cycled", ag.Name, len(runs))
		}
	}
	// A further hop is refused, visibly (B5).
	res := e.post(a.ID, "again")
	if r := e.waitRun(res.RunID); r.FinishReason == nil || *r.FinishReason != "budget" || r.Status != store.RunDone {
		t.Errorf("post-ceiling run = %+v", r)
	}
}

func TestSendRequiresAnEdgeAndFallsBackToMailbox(t *testing.T) {
	e := newEnv(t)
	var target *store.Agent
	e.provider(llm.NewScriptedFunc("p", func(call int, msgs []llm.Message) llm.Turn {
		switch call {
		case 0:
			return llm.CallTool("switchboard_chat_send", map[string]any{"to_chat_id": e.chatOf(target.ID), "message": "hello"})
		}
		return llm.Say("done")
	}))
	sender := e.agent("sender", chain(withModel("p"), caps(false, true)))
	target = e.agent("sleeper", chain(withModel("p"), func(in *CreateAgentInput) { f := false; in.AutoWake = &f }))
	e.waitRun(e.post(sender.ID, "go").RunID)
	// No edge: denied, recorded as such.
	if rows := e.callRows(store.CallFilter{Status: store.StatusDenied}); len(rows) != 1 || !strings.Contains(rows[0].Error, "no allowed edge") {
		t.Fatalf("rows = %+v", rows)
	}
	// With an edge and auto_wake off: mail accumulates, nobody runs.
	if _, err := e.m.SetEdge(context.Background(), sender.ID, target.ID, true); err != nil {
		t.Fatal(err)
	}
	e.provider(llm.NewScriptedFunc("p", func(call int, msgs []llm.Message) llm.Turn {
		if lastIsTool(msgs) {
			return llm.Say("done")
		}
		return llm.CallTool("switchboard_chat_send", map[string]any{"to_chat_id": e.chatOf(target.ID), "message": "hello"})
	}))
	e.waitRun(e.post(sender.ID, "again").RunID)
	if n, _ := e.st.CountInbox(context.Background(), target.ID); n != 1 {
		t.Fatalf("inbox = %d", n)
	}
	runs, _ := e.st.ListRuns(context.Background(), store.RunFilter{AgentID: target.ID})
	if len(runs) != 0 {
		t.Errorf("auto_wake=false agent ran: %+v", runs)
	}
	g, _ := e.m.Graph(context.Background())
	for _, ga := range g.Agents {
		if ga.ID == target.ID && ga.UnreadMail != 1 {
			t.Errorf("unread = %d", ga.UnreadMail)
		}
	}
	// inbox.read drains it, with sender identity.
	out, err := e.m.toolInbox(context.Background(), &callCtx{agent: target}, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	items := out.([]map[string]any)
	if len(items) != 1 || items[0]["from_chat_id"] != e.chatOf(sender.ID) || items[0]["message"] != "hello" {
		t.Fatalf("inbox.read = %+v", items)
	}
	if n, _ := e.st.CountInbox(context.Background(), target.ID); n != 0 {
		t.Errorf("not drained: %d", n)
	}
}

// A8: CreateAgent opens a parent<->child edge automatically, so chat.send
// works between them right away. But chat.send is always fire-and-forget
// (spec.md 5.5) even to your own child: there is no automatic reply route,
// unlike chat.spawn's own message (B8, covered by
// TestSpawnMakesChildChatUnderParentChatWithFirstMessage). If the child wants
// to answer, it has to send back explicitly.
func TestSendToChildIsAllowedButFireAndForget(t *testing.T) {
	e := newEnv(t)
	var kid *store.Agent
	e.provider(llm.NewScriptedFunc("parent", func(_ int, msgs []llm.Message) llm.Turn {
		if lastIsTool(msgs) {
			return llm.Say("sent")
		}
		return llm.CallTool("switchboard_chat_send", map[string]any{"to_chat_id": e.chatOf(kid.ID), "message": "compute"})
	}))
	e.scripted("kid", llm.Say("forty-two"))
	parent := e.agent("parent", chain(withModel("parent"), caps(true, true)))
	var err error
	kid, err = e.m.CreateAgent(context.Background(), CreateAgentInput{ParentID: parent.ID, Name: "kid", Approval: "never", Model: []byte(`{"provider":"kid","model":"fake"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := e.st.EdgeAllowed(context.Background(), parent.ID, kid.ID); !ok {
		t.Fatal("A8: spawning a child must open an allowed edge to it")
	}
	run := e.waitRun(e.post(parent.ID, "ask").RunID)
	if run.Status != store.RunDone {
		t.Fatalf("run = %+v", run)
	}
	path := e.path(e.chatOf(parent.ID))
	if strings.Contains(string(path[len(path)-1].Content), "forty-two") {
		t.Errorf("chat.send must not auto-reply, even to a child: %s", path[len(path)-1].Content)
	}
	// The child's side is its own chat: no peer chat was made, and the message
	// sits in it as a user message from the parent's chat.
	cs, _ := e.st.ListChats(context.Background(), store.ChatFilter{AgentID: kid.ID})
	if len(cs) != 1 || cs[0].ID != e.chatOf(kid.ID) || cs[0].PeerAgentID != nil {
		t.Fatalf("kid chats = %+v", cs)
	}
	kp := e.path(cs[0].ID)
	meta, ok := parseSender(kp[0].Sender)
	if kp[0].Role != store.RoleUser || !ok || meta.ChatID != e.chatOf(parent.ID) || meta.Kind != SenderMessage ||
		string(kp[0].Content) != `[{"type":"text","text":"compute"}]` {
		t.Errorf("child chat = %+v", kp)
	}
	kr, _ := e.st.ListRuns(context.Background(), store.RunFilter{AgentID: kid.ID})
	if len(kr) != 1 || kr[0].Trigger != store.TriggerAgentMessage || *kr[0].TriggeredByAgentID != parent.ID {
		t.Errorf("child runs = %+v", kr)
	}
	// The child answered on its own chat ("forty-two"), which never routes
	// back automatically.
	if roles(kp) != "user,assistant" {
		t.Errorf("kid path = %s", roles(kp))
	}
}

func TestAgentStopOnlyDescendants(t *testing.T) {
	e := newEnv(t)
	e.provider(stallProvider{})
	boss := e.agent("boss", chain(withModel("stall"), caps(true, false)))
	kid, _ := e.m.CreateAgent(context.Background(), CreateAgentInput{ParentID: boss.ID, Name: "kid", Approval: "never", Model: boss.Model})
	other := e.agent("other", withModel("stall"))
	r := e.post(kid.ID, "spin")
	e.waitStatus(r.RunID, store.RunRunning)
	cc := &callCtx{agent: boss}
	if _, err := e.m.toolStop(context.Background(), cc, map[string]any{"chat_id": e.chatOf(other.ID)}); err == nil {
		t.Error("stopped a non-descendant")
	}
	if _, err := e.m.toolStop(context.Background(), cc, map[string]any{"chat_id": e.chatOf(kid.ID)}); err != nil {
		t.Fatal(err)
	}
	if run := e.waitRun(r.RunID); run.Status != store.RunCancelled {
		t.Errorf("run = %+v", run)
	}
}
