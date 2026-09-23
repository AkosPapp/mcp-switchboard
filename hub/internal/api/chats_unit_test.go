package api

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/agents"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/calls"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/events"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/llm"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/registry"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

type chatView struct {
	ID           string  `json:"id"`
	AgentID      string  `json:"agentId"`
	Title        string  `json:"title"`
	ParentChatID *string `json:"parentChatId"`
	ProfileID    *string `json:"profileId"`
	ClientLabel  *string `json:"clientLabel"`
}

func TestCreateChatFormsHandlerPassesSemantics(t *testing.T) {
	o := newOrch(t)
	cases := []struct {
		body string
		want func(in agents.CreateChatInput) bool
	}{
		{`{}`, func(in agents.CreateChatInput) bool {
			return in.ProfileID == "" && !in.ProfileNone && in.SystemPrompt == nil && in.ClientLabel == ""
		}},
		{`{"profileId":null}`, func(in agents.CreateChatInput) bool { return in.ProfileNone && in.ProfileID == "" }},
		{`{"profileId":"p1","clientLabel":"laptop","title":"T","parentChatId":"c9"}`, func(in agents.CreateChatInput) bool {
			return in.ProfileID == "p1" && in.ClientLabel == "laptop" && in.Title == "T" && in.ParentChatID == "c9"
		}},
		{`{"systemPrompt":"own","clientLabel":null}`, func(in agents.CreateChatInput) bool {
			return in.SystemPrompt != nil && *in.SystemPrompt == "own" && !in.ProfileNone && in.ClientLabel == ""
		}},
		{`{"profileId":null,"systemPrompt":""}`, func(in agents.CreateChatInput) bool {
			return in.ProfileNone && in.SystemPrompt != nil && *in.SystemPrompt == ""
		}},
	}
	for _, c := range cases {
		requireStatus(t, o.do(t, "POST", "/api/chats", c.body), 201)
		if got := o.agents.chatIn[len(o.agents.chatIn)-1]; !c.want(got) {
			t.Errorf("%s -> %+v", c.body, got)
		}
	}
	for _, bad := range []string{`{"clientLabel":"*"}`, `{"clientLabel":""}`, `{"clientLabel":5}`, `{"profileId":5}`} {
		requireStatus(t, o.do(t, "POST", "/api/chats", bad), 400)
	}
	// agentId wins: the legacy attach form never reaches CreateChat.
	n := len(o.agents.chatIn)
	a := o.agent(t, "legacy")
	requireStatus(t, o.do(t, "POST", "/api/chats", `{"agentId":"`+a.ID+`","profileId":"p1","systemPrompt":"x"}`), 201)
	if len(o.agents.chatIn) != n {
		t.Fatal("agentId did not take the legacy path")
	}
}

func TestPatchChatPassesRebinding(t *testing.T) {
	o := newOrch(t)
	a := o.agent(t, "a")
	c := o.chat(t, a.ID, "t")
	requireStatus(t, o.do(t, "PATCH", "/api/chats/"+c.ID, `{"profileId":null,"systemPrompt":"own","clientLabel":"laptop"}`), 200)
	up := o.agents.chatUp[0]
	if !up.ProfileSet || up.ProfileID != nil || up.SystemPrompt == nil || *up.SystemPrompt != "own" || !up.ClientSet || up.ClientLabel == nil || *up.ClientLabel != "laptop" {
		t.Fatalf("update = %+v", up)
	}
	requireStatus(t, o.do(t, "PATCH", "/api/chats/"+c.ID, `{"clientLabel":null}`), 200)
	if up := o.agents.chatUp[1]; !up.ClientSet || up.ClientLabel != nil || up.ProfileSet {
		t.Fatalf("update = %+v", up)
	}
	n := len(o.agents.chatUp)
	requireStatus(t, o.do(t, "PATCH", "/api/chats/"+c.ID, `{"title":"only"}`), 200)
	if len(o.agents.chatUp) != n {
		t.Fatal("a title edit went through UpdateChat")
	}
	for _, bad := range []string{`{"clientLabel":"*"}`, `{"clientLabel":""}`, `{"profileId":""}`, `{"profileId":3}`} {
		requireStatus(t, o.do(t, "PATCH", "/api/chats/"+c.ID, bad), 400)
	}
	requireStatus(t, o.do(t, "PATCH", "/api/chats/nope", `{"clientLabel":null}`), 404)
}

func TestChatsAsUnitEndToEnd(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "hub.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	bus := events.NewBus()
	mgr := agents.New(agents.Options{Store: db, Registry: registry.New(bus), Dispatcher: calls.NewDispatcher(calls.Options{Store: db, Bus: bus}),
		LLM: llm.NewRegistry(), Bus: bus})
	if err := mgr.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Shutdown(ctx) })
	f := newFixture(t, func(opts *Options) { opts.Store = db; opts.Agents = mgr })

	mk := func(body string) chatView {
		t.Helper()
		rec := f.do(t, "POST", "/api/chats", body)
		requireStatus(t, rec, 201)
		var c chatView
		decode(t, rec, &c)
		return c
	}
	root := mk(`{"profileId":null,"systemPrompt":"be brief","title":"root"}`)
	if root.Title != "root" || root.ProfileID != nil || root.ClientLabel != nil || root.ParentChatID != nil {
		t.Fatalf("root = %+v", root)
	}
	if !strings.Contains(f.do(t, "GET", "/api/chats/"+root.ID, "").Body.String(), `"parentChatId":null`) {
		t.Fatal("parentChatId missing from chat json")
	}
	if c := mk(`{}`); c.Title != "New chat" || c.ProfileID == nil {
		t.Fatalf("default chat = %+v", c)
	}
	kid := mk(`{"profileId":null,"title":"kid","parentChatId":"` + root.ID + `"}`)
	if kid.ParentChatID == nil || *kid.ParentChatID != root.ID {
		t.Fatalf("kid = %+v", kid)
	}
	requireStatus(t, f.do(t, "POST", "/api/chats", `{"parentChatId":"nope"}`), 404)

	// Rebinding: attach the default profile, then go back to own text.
	var list struct{ Profiles []profileView }
	decode(t, f.do(t, "GET", "/api/profiles", ""), &list)
	pid := list.Profiles[0].ID
	rec := f.do(t, "PATCH", "/api/chats/"+root.ID, `{"profileId":"`+pid+`"}`)
	requireStatus(t, rec, 200)
	var got chatView
	decode(t, rec, &got)
	if got.ProfileID == nil || *got.ProfileID != pid {
		t.Fatalf("patched = %+v", got)
	}
	rec = f.do(t, "GET", "/api/chats/"+root.ID+"/system-prompt", "")
	if !strings.Contains(rec.Body.String(), `"source":"profile"`) {
		t.Fatalf("prompt = %s", rec.Body.String())
	}
	rec = f.do(t, "PATCH", "/api/chats/"+root.ID, `{"profileId":null,"systemPrompt":"custom now"}`)
	requireStatus(t, rec, 200)
	rec = f.do(t, "GET", "/api/chats/"+root.ID+"/system-prompt", "")
	if !strings.Contains(rec.Body.String(), `"systemPrompt":"custom now"`) || !strings.Contains(rec.Body.String(), `"source":"agent"`) {
		t.Fatalf("prompt = %s", rec.Body.String())
	}
	requireStatus(t, f.do(t, "PATCH", "/api/chats/"+root.ID, `{"profileId":"nope"}`), 404)

	// Messages carry "sender" (null for typed ones).
	rec = f.do(t, "GET", "/api/chats/"+root.ID+"/messages", "")
	requireStatus(t, rec, 200)

	// Deleting the parent takes the child and both records.
	rec = f.do(t, "DELETE", "/api/chats/"+root.ID, "")
	requireStatus(t, rec, 200)
	if strings.TrimSpace(rec.Body.String()) != `{"deletedChats":2}` {
		t.Fatalf("delete = %s", rec.Body.String())
	}
	requireStatus(t, f.do(t, "GET", "/api/chats/"+kid.ID, ""), 404)
	for _, id := range []string{root.AgentID, kid.AgentID} {
		requireStatus(t, f.do(t, "GET", "/api/agents/"+id, ""), 404)
	}
}

func TestMessagesCarrySender(t *testing.T) {
	o := newOrch(t)
	a := o.agent(t, "a")
	c := o.chat(t, a.ID, "t")
	plain := o.msg(t, c.ID, nil, "user", "typed")
	if _, err := o.db.AppendMessage(context.Background(), store.Message{
		ChatID: c.ID, ParentID: &plain.ID, Role: "user", Content: []byte(`[{"type":"text","text":"hi"}]`),
		Sender: []byte(`{"chatId":"x","chatTitle":"other","kind":"message"}`)}); err != nil {
		t.Fatal(err)
	}
	body := o.do(t, "GET", "/api/chats/"+c.ID+"/messages", "").Body.String()
	if !strings.Contains(body, `"sender":null`) || !strings.Contains(body, `"sender":{"chatId":"x","chatTitle":"other","kind":"message"}`) {
		t.Fatalf("messages = %s", body)
	}
}
