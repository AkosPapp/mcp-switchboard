package agents

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/llm"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/registry"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

func (e *env) title(chatID, title string) {
	e.t.Helper()
	if _, err := e.st.UpdateChat(context.Background(), chatID, store.ChatPatch{Title: &title}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) allChats() []store.Chat {
	e.t.Helper()
	cs, err := e.st.ListChats(context.Background(), store.ChatFilter{IncludeArchived: true, Limit: store.MaxLimit})
	if err != nil {
		e.t.Fatal(err)
	}
	return cs
}

func (e *env) waitFor(what string, ok func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("timed out waiting for %s", what)
}

func msgText(m store.Message) string {
	var bs []llm.Block
	_ = json.Unmarshal(m.Content, &bs)
	return blocksText(bs)
}

// chat.send is always fire-and-forget (spec.md 5.5): there is no wait, no
// synchronous reply, and no automatic routing back to the sender. If the
// recipient wants to answer, it has to call chat.send itself.
func TestSendInjectsIntoRecipientChatFireAndForget(t *testing.T) {
	e := newEnv(t)
	var recv *store.Agent
	sender := e.agent("S", chain(withModel("s"), caps(false, true)))
	recv = e.agent("R", withModel("r"))
	e.title(e.chatOf(sender.ID), "researcher")
	if _, err := e.m.SetEdge(context.Background(), sender.ID, recv.ID, true); err != nil {
		t.Fatal(err)
	}
	sp := llm.NewScriptedFunc("s", func(call int, msgs []llm.Message) llm.Turn {
		switch {
		case call == 0:
			return llm.CallTool("switchboard_chat_send", map[string]any{"to_chat_id": e.chatOf(recv.ID), "message": "hello there"})
		default:
			return llm.Say("noted")
		}
	})
	rp := llm.NewScripted("r", llm.Say("pong"))
	e.provider(sp)
	e.provider(rp)

	before := len(e.allChats())
	first := e.waitRun(e.post(sender.ID, "go").RunID)
	if first.Status != store.RunDone {
		t.Fatalf("sender run = %+v", first)
	}
	// The tool result carries only message_id, never a reply.
	if calls := sp.Calls(); len(calls) < 2 {
		t.Fatal("sender did not get a second turn")
	} else {
		toolMsg := calls[1].Messages[len(calls[1].Messages)-1]
		if len(toolMsg.ToolResults) != 1 || strings.Contains(blocksText(toolMsg.ToolResults[0].Content), "reply") {
			t.Errorf("chat.send result must not carry a reply: %+v", toolMsg.ToolResults)
		}
	}

	// The recipient's own chat holds the message as a user turn with sender data,
	// and its run was woken by it.
	rchat := e.chatOf(recv.ID)
	e.waitFor("recipient run", func() bool {
		runs, _ := e.st.ListRuns(context.Background(), store.RunFilter{AgentID: recv.ID})
		return len(runs) == 1 && terminal(runs[0].Status)
	})
	rpath := e.path(rchat)
	if roles(rpath) != "user,assistant" {
		t.Fatalf("recipient path = %s", roles(rpath))
	}
	meta, ok := parseSender(rpath[0].Sender)
	if !ok || meta.ChatID != e.chatOf(sender.ID) || meta.ChatTitle != "researcher" || meta.Kind != SenderMessage {
		t.Fatalf("sender = %s", rpath[0].Sender)
	}
	if msgText(rpath[0]) != "hello there" {
		t.Errorf("stored text must be raw, got %q", msgText(rpath[0]))
	}
	runs, _ := e.st.ListRuns(context.Background(), store.RunFilter{AgentID: recv.ID})
	if runs[0].Trigger != store.TriggerAgentMessage || runs[0].TriggeredByAgentID == nil || *runs[0].TriggeredByAgentID != sender.ID {
		t.Errorf("run = %+v", runs[0])
	}
	// ... and the model saw the fixed preamble before the text.
	seen := rp.Calls()[0].Messages[0]
	want := `[Message from chat "researcher" (id ` + e.chatOf(sender.ID) + `). Reply with switchboard.chat.send to that id.]` + "\n\nhello there"
	if seen.Role != llm.RoleUser || blocksText(seen.Content) != want {
		t.Errorf("model saw %q, want %q", blocksText(seen.Content), want)
	}

	// Nothing routes the recipient's "pong" turn back anywhere: chat.send has
	// no reply route, unlike chat.spawn's message (B8). The sender never runs
	// again and no message with sender metadata ever lands in its chat.
	time.Sleep(100 * time.Millisecond)
	if rs, _ := e.st.ListRuns(context.Background(), store.RunFilter{AgentID: sender.ID}); len(rs) != 1 {
		t.Errorf("sender ran %d times; chat.send must not trigger an automatic reply", len(rs))
	}
	for _, m := range e.path(e.chatOf(sender.ID)) {
		if _, ok := parseSender(m.Sender); ok {
			t.Errorf("chat.send must never inject a reply into the sender's chat: %+v", m)
		}
	}
	if rs, _ := e.st.ListRuns(context.Background(), store.RunFilter{AgentID: recv.ID}); len(rs) != 1 {
		t.Errorf("recipient ran %d times", len(rs))
	}
	if n := len(e.allChats()); n != before {
		t.Errorf("chats went from %d to %d: messaging must not create chats", before, n)
	}
	for _, c := range e.allChats() {
		if c.Kind != store.ChatKindHuman || c.PeerAgentID != nil {
			t.Errorf("peer chat created: %+v", c)
		}
	}
}

func TestSendToUnknownOrForbiddenChat(t *testing.T) {
	e := newEnv(t)
	sender := e.agent("S", caps(false, true))
	other := e.agent("O", nil)
	cc := &callCtx{agent: sender}
	if _, err := e.m.toolSend(context.Background(), cc, map[string]any{"to_chat_id": "nope", "message": "x"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown chat: %v", err)
	}
	if _, err := e.m.toolSend(context.Background(), cc, map[string]any{"to_chat_id": e.chatOf(other.ID), "message": "x"}); err == nil || !strings.Contains(err.Error(), "no allowed edge") {
		t.Errorf("no edge: %v", err)
	}
	if _, err := e.m.toolSend(context.Background(), cc, map[string]any{"to_chat_id": e.chatOf(sender.ID), "message": "x"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("self: %v", err)
	}
}

func TestSpawnMakesChildChatUnderParentChatWithFirstMessage(t *testing.T) {
	e := newEnv(t)
	boss := e.agent("boss", chain(withModel("boss"), caps(true, true)))
	e.title(e.chatOf(boss.ID), "planner")
	e.scripted("boss", llm.Say("ack"))
	e.scripted("kid", llm.Say("done: 7"))
	out, err := e.m.toolSpawn(context.Background(), &callCtx{agent: boss}, map[string]any{
		"title": "worker", "system_prompt": "work", "message": "do the thing",
		"model": map[string]any{"provider": "kid", "model": "fake"},
	})
	if err != nil {
		t.Fatal(err)
	}
	res := out.(map[string]any)
	if len(res) != 1 {
		t.Errorf("spawn returns only chat_id, got %v", res)
	}
	kidChatID := res["chat_id"].(string)
	kc, _ := e.st.GetChat(context.Background(), kidChatID)
	if kc == nil || kc.ParentChatID == nil || *kc.ParentChatID != e.chatOf(boss.ID) || kc.Title != "worker" ||
		kc.Kind != store.ChatKindHuman || kc.PeerAgentID != nil {
		t.Fatalf("child chat = %+v", kc)
	}
	kid := e.agentOfChat(kidChatID)
	if kid.ParentID == nil || *kid.ParentID != boss.ID || kid.Origin != store.OriginSpawn {
		t.Fatalf("child record = %+v", kid)
	}
	e.waitFor("child run", func() bool {
		rs, _ := e.st.ListRuns(context.Background(), store.RunFilter{AgentID: kid.ID})
		return len(rs) == 1 && terminal(rs[0].Status)
	})
	kp := e.path(kidChatID)
	meta, ok := parseSender(kp[0].Sender)
	if !ok || meta.Kind != SenderSpawn || meta.ChatID != e.chatOf(boss.ID) || meta.ChatTitle != "planner" || msgText(kp[0]) != "do the thing" {
		t.Fatalf("child first message = %+v", kp[0])
	}
	if rs, _ := e.st.ListRuns(context.Background(), store.RunFilter{AgentID: kid.ID}); rs[0].Trigger != store.TriggerSpawn {
		t.Errorf("trigger = %s", rs[0].Trigger)
	}
	// The child's answer comes back into the parent's chat as a reply.
	e.waitFor("reply in parent chat", func() bool {
		for _, m := range e.path(e.chatOf(boss.ID)) {
			if mt, ok := parseSender(m.Sender); ok && mt.Kind == SenderReply && msgText(m) == "done: 7" && mt.ChatID == kidChatID {
				return true
			}
		}
		return false
	})
	// chat.list shows it, with chat ids.
	lst, err := e.m.toolList(context.Background(), &callCtx{agent: boss}, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	rows := lst.([]map[string]any)
	if len(rows) != 1 || rows[0]["chat_id"] != kidChatID || rows[0]["title"] != "worker" || rows[0]["depth"] != 1 {
		t.Fatalf("chat.list = %+v", rows)
	}
	if _, err := e.m.toolSpawn(context.Background(), &callCtx{agent: boss}, map[string]any{"system_prompt": "x"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("spawn without a title: %v", err)
	}
}

func TestInjectedMessageRendering(t *testing.T) {
	sender := mustJSON(senderMeta{ChatID: "abc", ChatTitle: `the "boss"`, Kind: SenderMessage})
	path := []store.Message{
		{Role: store.RoleUser, Content: mustJSON(textBlocks("plain"))},
		{Role: store.RoleUser, Content: mustJSON(textBlocks("hi")), Sender: sender},
		{Role: store.RoleUser, Content: mustJSON([]llm.Block{{Type: llm.BlockImage, Data: "x", MediaType: "image/png"}}),
			Sender: mustJSON(senderMeta{ChatID: "abc", Kind: SenderReply})},
	}
	got := toLLMMessages(path)
	if blocksText(got[0].Content) != "plain" {
		t.Errorf("human message changed: %q", blocksText(got[0].Content))
	}
	if want := `[Message from chat "the \"boss\"" (id abc). Reply with switchboard.chat.send to that id.]` + "\n\nhi"; blocksText(got[1].Content) != want {
		t.Errorf("got %q", blocksText(got[1].Content))
	}
	if len(got[2].Content) != 2 || got[2].Content[0].Text != `[Reply from chat "untitled" (id abc).]` {
		t.Errorf("non-text first block: %+v", got[2].Content)
	}
	if string(path[1].Content) != string(mustJSON(textBlocks("hi"))) {
		t.Error("the stored message was modified")
	}
}

func TestPrimaryChatIsMostRecentlyActiveHumanChat(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.agent("a", nil)
	first := e.chatOf(a.ID)
	// A legacy peer chat, and a second human chat that becomes the active one.
	peer := e.agent("p", nil)
	if _, err := e.st.CreateChat(ctx, store.Chat{AgentID: a.ID, PeerAgentID: &peer.ID, Kind: store.ChatKindAgent}); err != nil {
		t.Fatal(err)
	}
	second := e.newChat(a.ID)
	got, err := e.m.primaryChat(ctx, a, false)
	if err != nil || got.ID != second.ID {
		t.Fatalf("primary = %+v %v, want %s", got, err, second.ID)
	}
	if _, err := e.st.AppendMessage(ctx, store.Message{ChatID: first, Role: store.RoleUser}); err != nil {
		t.Fatal(err)
	}
	if got, _ = e.m.primaryChat(ctx, a, false); got.ID != first {
		t.Fatalf("primary after activity = %s, want %s", got.ID, first)
	}
	// A record without any chat gets one on demand.
	bare := e.manual("bare", nil)
	if c, _ := e.m.primaryChat(ctx, bare, false); c != nil {
		t.Fatal("primaryChat(create=false) made a chat")
	}
	if c, err := e.m.primaryChat(ctx, bare, true); err != nil || c == nil || c.AgentID != bare.ID {
		t.Fatalf("created = %+v %v", c, err)
	}
}

func TestCreateChatForms(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.scripted("fake")
	e.addServer("laptop", "", "fs", registry.ToolInfo{Name: "read"})
	prof := e.profile("Coder", "you code", store.Capabilities{CanMessage: true})
	def, _ := e.st.GetDefaultProfile(ctx)

	// Profile: a live reference, 1:1 record, default title.
	c, err := e.m.CreateChat(ctx, CreateChatInput{ProfileID: prof.ID, ClientLabel: "laptop"})
	if err != nil {
		t.Fatal(err)
	}
	a := e.agentNow(c.AgentID)
	if c.Title != "New chat" || c.ParentChatID != nil || c.Kind != store.ChatKindHuman || c.ProfileID == nil || *c.ProfileID != prof.ID ||
		a.SystemPrompt != "you code" || a.ProfileID == nil || a.Origin != store.OriginChat || !a.Capabilities.CanMessage {
		t.Fatalf("chat %+v agent %+v", c, a)
	}
	if cs, _ := e.st.ListChats(ctx, store.ChatFilter{AgentID: a.ID}); len(cs) != 1 {
		t.Fatalf("record must be 1:1, has %d chats", len(cs))
	}
	if v, _ := e.m.ChatSystemPrompt(ctx, c.ID); v.Source != "profile" || v.SystemPrompt != "you code" {
		t.Errorf("prompt view = %+v", v)
	}
	// Own prompt: explicit profileId null.
	own := "be brief"
	c, err = e.m.CreateChat(ctx, CreateChatInput{ProfileNone: true, SystemPrompt: &own, Title: "mine"})
	if err != nil {
		t.Fatal(err)
	}
	a = e.agentNow(c.AgentID)
	if c.Title != "mine" || c.ProfileID != nil || c.ClientLabel != nil || a.SystemPrompt != "be brief" || a.ProfileID != nil {
		t.Fatalf("own-prompt chat %+v agent %+v", c, a)
	}
	if gs, _ := e.st.ListGrants(ctx, a.ID); len(gs) != 0 {
		t.Errorf("no client must mean no grants (AGENT_DEFAULT_GRANTS must not apply): %+v", gs)
	}
	if v, _ := e.m.ChatSystemPrompt(ctx, c.ID); v.Source != "agent" || v.SystemPrompt != "be brief" {
		t.Errorf("prompt view = %+v", v)
	}
	// systemPrompt given, profileId omitted: the own prompt wins, not the default profile.
	c, _ = e.m.CreateChat(ctx, CreateChatInput{SystemPrompt: &own})
	if c.ProfileID != nil || e.agentNow(c.AgentID).SystemPrompt != "be brief" {
		t.Errorf("profile omitted with a prompt used the default: %+v", c)
	}
	// Explicit null and nothing else: no prompt at all.
	c, _ = e.m.CreateChat(ctx, CreateChatInput{ProfileNone: true})
	if v, _ := e.m.ChatSystemPrompt(ctx, c.ID); c.ProfileID != nil || v.Source != "none" || v.SystemPrompt != "" {
		t.Errorf("none: %+v %+v", c, v)
	}
	// Nothing said: the default profile.
	c, _ = e.m.CreateChat(ctx, CreateChatInput{})
	if c.ProfileID == nil || *c.ProfileID != def.ID {
		t.Errorf("default profile not used: %+v", c)
	}
	// Client vs none.
	c, _ = e.m.CreateChat(ctx, CreateChatInput{ClientLabel: "laptop", ProfileNone: true})
	if gs, _ := e.st.ListGrants(ctx, c.AgentID); c.ClientLabel == nil || len(gs) != 1 || gs[0].Label != "laptop" {
		t.Errorf("client chat: %+v %+v", c, gs)
	}
	for _, bad := range []string{"*"} {
		if _, err := e.m.CreateChat(ctx, CreateChatInput{ClientLabel: bad}); !errors.Is(err, ErrInvalid) {
			t.Errorf("clientLabel %q: %v", bad, err)
		}
	}
	if _, err := e.m.CreateChat(ctx, CreateChatInput{ProfileID: "nope"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown profile: %v", err)
	}
	if _, err := e.m.CreateChat(ctx, CreateChatInput{ParentChatID: "nope"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown parent: %v", err)
	}
	// A child chat nests under its parent and its record under the parent's record.
	root, _ := e.m.CreateChat(ctx, CreateChatInput{ProfileNone: true, Title: "root"})
	kid, err := e.m.CreateChat(ctx, CreateChatInput{ProfileNone: true, Title: "kid", ParentChatID: root.ID})
	if err != nil {
		t.Fatal(err)
	}
	ka := e.agentNow(kid.AgentID)
	if kid.ParentChatID == nil || *kid.ParentChatID != root.ID || ka.ParentID == nil || *ka.ParentID != root.AgentID || ka.Origin != store.OriginSpawn {
		t.Fatalf("child %+v / %+v", kid, ka)
	}
	// The default title gives way to the first message.
	c, _ = e.m.CreateChat(ctx, CreateChatInput{ProfileNone: true, Model: json.RawMessage(`{"provider":"fake","model":"fake"}`)})
	if _, err := e.m.Post(ctx, c.ID, PostInput{Content: say("what is the weather\nmore")}); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.st.GetChat(ctx, c.ID); got.Title != "what is the weather" {
		t.Errorf("auto title = %q", got.Title)
	}
}

func TestUpdateChatRebinds(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.scripted("fake")
	prof := e.profile("Coder", "you code", store.Capabilities{})
	c, err := e.m.CreateChat(ctx, CreateChatInput{ProfileID: prof.ID, ClientLabel: "laptop"})
	if err != nil {
		t.Fatal(err)
	}
	// Detach to own text.
	own := "own text"
	got, err := e.m.UpdateChat(ctx, c.ID, ChatUpdate{ProfileSet: true, SystemPrompt: &own})
	if err != nil || got.ProfileID != nil {
		t.Fatalf("detach: %+v %v", got, err)
	}
	if a := e.agentNow(c.AgentID); a.ProfileID != nil || a.SystemPrompt != "own text" {
		t.Fatalf("agent = %+v", a)
	}
	// systemPrompt is ignored while a profile is referenced.
	ignored := "ignored"
	if got, err = e.m.UpdateChat(ctx, c.ID, ChatUpdate{ProfileSet: true, ProfileID: &prof.ID, SystemPrompt: &ignored}); err != nil || *got.ProfileID != prof.ID {
		t.Fatalf("attach: %+v %v", got, err)
	}
	if a := e.agentNow(c.AgentID); a.SystemPrompt != "you code" {
		t.Errorf("prompt = %q", a.SystemPrompt)
	}
	if _, err := e.m.UpdateChat(ctx, c.ID, ChatUpdate{ProfileSet: true, ProfileID: strPtr("nope")}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown profile: %v", err)
	}
	// Client rebinding rewrites the grants and the mirrored field; nil = none.
	other := "desk"
	if got, err = e.m.UpdateChat(ctx, c.ID, ChatUpdate{ClientSet: true, ClientLabel: &other}); err != nil || got.ClientLabel == nil || *got.ClientLabel != "desk" {
		t.Fatalf("rebind: %+v %v", got, err)
	}
	if gs, _ := e.st.ListGrants(ctx, c.AgentID); len(gs) != 1 || gs[0].Label != "desk" {
		t.Errorf("grants = %+v", gs)
	}
	if got, err = e.m.UpdateChat(ctx, c.ID, ChatUpdate{ClientSet: true}); err != nil || got.ClientLabel != nil {
		t.Fatalf("unbind: %+v %v", got, err)
	}
	if gs, _ := e.st.ListGrants(ctx, c.AgentID); len(gs) != 0 {
		t.Errorf("grants = %+v", gs)
	}
	if _, err := e.m.UpdateChat(ctx, c.ID, ChatUpdate{ClientSet: true, ClientLabel: strPtr("*")}); !errors.Is(err, ErrInvalid) {
		t.Errorf("wildcard client: %v", err)
	}
}

func TestDeleteChatCascadesAndCancelsRuns(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.provider(stallProvider{})
	model := json.RawMessage(`{"provider":"stall","model":"fake"}`)
	root, err := e.m.CreateChat(ctx, CreateChatInput{ProfileNone: true, Title: "root", Model: model})
	if err != nil {
		t.Fatal(err)
	}
	kid, err := e.m.CreateChat(ctx, CreateChatInput{ProfileNone: true, Title: "kid", ParentChatID: root.ID, Model: model})
	if err != nil {
		t.Fatal(err)
	}
	keep, _ := e.m.CreateChat(ctx, CreateChatInput{ProfileNone: true, Title: "keep", Model: model})
	res, err := e.m.Post(ctx, kid.ID, PostInput{Content: say("spin")})
	if err != nil {
		t.Fatal(err)
	}
	e.waitStatus(res.RunID, store.RunRunning)

	n, err := e.m.DeleteChat(ctx, root.ID)
	if err != nil || n != 2 {
		t.Fatalf("deleted %d, %v", n, err)
	}
	for _, id := range []string{root.ID, kid.ID} {
		if c, _ := e.st.GetChat(ctx, id); c != nil {
			t.Errorf("chat %s survived", id)
		}
	}
	for _, id := range []string{root.AgentID, kid.AgentID} {
		if a, _ := e.st.GetAgent(ctx, id); a != nil {
			t.Errorf("record %s survived", id)
		}
	}
	if r, _ := e.st.GetRun(ctx, res.RunID); r != nil {
		t.Errorf("run survived: %+v", r)
	}
	e.waitFor("runs to stop", func() bool { return e.m.ActiveRuns() == 0 })
	if c, _ := e.st.GetChat(ctx, keep.ID); c == nil {
		t.Fatal("an unrelated chat was deleted")
	}
	if _, err := e.m.DeleteChat(ctx, root.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: %v", err)
	}
}

func TestHubToolsSpeakOfChatsOnly(t *testing.T) {
	e := newEnv(t)
	want := map[string]bool{
		"switchboard.chat.spawn": true, "switchboard.chat.send": true, "switchboard.chat.list": true, "switchboard.chat.stop": true,
		"switchboard.inbox.read": true, "switchboard.graph.set_edge": true, "switchboard.mcp.grant": true, "switchboard.mcp.list_servers": true,
	}
	for _, ht := range e.m.HubTools() {
		if !want[ht.Name] {
			t.Errorf("unexpected hub tool %s", ht.Name)
		}
		delete(want, ht.Name)
		blob, _ := json.Marshal(ht)
		if strings.Contains(strings.ToLower(string(blob)), "agent") {
			t.Errorf("%s still speaks of agents: %s", ht.Name, blob)
		}
	}
	if len(want) != 0 {
		t.Errorf("missing hub tools %v", want)
	}
	// The old names are not tools any more, not even for a full-capability chat.
	a := e.agent("all", caps(true, true))
	for _, old := range []string{"switchboard.agent.send", "switchboard.agent.spawn", "switchboard.agent.list", "switchboard.agent.stop"} {
		if out := e.m.execTool(context.Background(), a, nil, old, map[string]any{}, ""); !out.IsError || !strings.Contains(out.Text(), "unknown tool") {
			t.Errorf("%s still callable: %+v", old, out)
		}
	}
	// Argument checks by chat id.
	cc := &callCtx{agent: a}
	if _, err := e.m.toolSetEdge(context.Background(), cc, map[string]any{"chat_id": "nope", "allowed": true}); err == nil {
		t.Error("set_edge accepted an unknown chat")
	}
	if _, err := e.m.toolSetEdge(context.Background(), cc, map[string]any{"chat_id": e.chatOf(a.ID), "allowed": true}); err == nil {
		t.Error("set_edge accepted connecting a chat to itself")
	}
}

// set_edge connects the caller to any chat it can name, not just its own
// subtree (D14): two unrelated leaf chats can now open an edge directly.
func TestSetEdgeConnectsArbitraryChats(t *testing.T) {
	e := newEnv(t)
	x := e.agent("x", caps(true, true))
	y := e.agent("y", caps(true, true))
	cc := &callCtx{agent: x}
	if _, err := e.m.toolSetEdge(context.Background(), cc, map[string]any{"chat_id": e.chatOf(y.ID), "allowed": true}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := e.st.EdgeAllowed(context.Background(), x.ID, y.ID); !ok {
		t.Error("edge should be allowed after set_edge")
	}
	if ok, _ := e.st.EdgeAllowed(context.Background(), y.ID, x.ID); !ok {
		t.Error("edge should be symmetric (D14)")
	}
	if _, err := e.m.toolSetEdge(context.Background(), cc, map[string]any{"chat_id": e.chatOf(y.ID), "allowed": false}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := e.st.EdgeAllowed(context.Background(), y.ID, x.ID); ok {
		t.Error("edge should be denied from either side after closing it")
	}
}
