package agents

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/registry"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

func TestStartSeedsDefaultAssistantOnce(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ps, err := e.m.ListProfiles(ctx)
	if err != nil || len(ps) != 1 || ps[0].Name != "Assistant" || !ps[0].IsDefault ||
		ps[0].Approval != store.ApprovalDestructive || ps[0].Capabilities.CanSpawn || ps[0].Capabilities.CanMessage {
		t.Fatalf("seed = %+v %v", ps, err)
	}
	if err := e.m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _ := e.st.CountProfiles(ctx); n != 1 {
		t.Fatalf("seeded twice: %d", n)
	}
}

func TestProfileValidationAndDeleteRules(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	assistant, _ := e.st.GetDefaultProfile(ctx)

	for _, bad := range []ProfileInput{{Name: "", SystemPrompt: "x"}, {Name: "n"}, {Name: "n", SystemPrompt: "x", Approval: "bogus"}} {
		if _, err := e.m.CreateProfile(ctx, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: err = %v, want ErrInvalid", bad, err)
		}
	}
	if _, err := e.m.CreateProfile(ctx, ProfileInput{Name: "Assistant", SystemPrompt: "x"}); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("duplicate: %v", err)
	}
	if err := e.m.DeleteProfile(ctx, assistant.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("delete only profile: %v", err)
	}
	p2, err := e.m.CreateProfile(ctx, ProfileInput{Name: "Coder", SystemPrompt: "code"})
	if err != nil || p2.IsDefault {
		t.Fatalf("second profile: %+v %v", p2, err)
	}
	if err := e.m.DeleteProfile(ctx, assistant.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("delete default with others: %v", err)
	}
	off := false
	if _, err := e.m.UpdateProfile(ctx, assistant.ID, ProfileUpdate{IsDefault: &off}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("un-default: %v", err)
	}
	on := true
	if p, err := e.m.UpdateProfile(ctx, p2.ID, ProfileUpdate{IsDefault: &on}); err != nil || !p.IsDefault {
		t.Fatalf("make default: %+v %v", p, err)
	}
	if d, _ := e.st.GetDefaultProfile(ctx); d.ID != p2.ID {
		t.Fatal("default did not move")
	}
	if err := e.m.DeleteProfile(ctx, assistant.ID); err != nil {
		t.Fatalf("delete non-default: %v", err)
	}
	if _, err := e.m.GetProfile(ctx, assistant.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get deleted: %v", err)
	}
}

func TestCreateChatFromProfileGrantsOnlyTheClient(t *testing.T) {
	e := newEnv(t) // AgentDefaultGrants is {*,*,*}: it must NOT apply here
	ctx := context.Background()
	e.scripted("fake")
	p, err := e.m.CreateProfile(ctx, ProfileInput{Name: "Boss", SystemPrompt: "lead", Approval: store.ApprovalAlways,
		Capabilities: store.Capabilities{CanSpawn: true, CanMessage: true}})
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"*"} {
		if _, err := e.m.CreateChat(ctx, CreateChatInput{ProfileID: p.ID, ClientLabel: bad}); !errors.Is(err, ErrInvalid) {
			t.Errorf("clientLabel %q: %v", bad, err)
		}
	}
	if _, err := e.m.CreateChat(ctx, CreateChatInput{ProfileID: "nope", ClientLabel: "laptop"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown profile: %v", err)
	}
	chat, err := e.m.CreateChat(ctx, CreateChatInput{ProfileID: p.ID, ClientLabel: "laptop", Title: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if chat.Kind != store.ChatKindHuman || chat.Title != "hi" || *chat.ProfileID != p.ID || *chat.ClientLabel != "laptop" {
		t.Fatalf("chat = %+v", chat)
	}
	a := e.agentNow(chat.AgentID)
	if a.ParentID != nil || a.Depth != 0 || a.ProfileID == nil || *a.ProfileID != p.ID ||
		a.SystemPrompt != "lead" || a.Approval != store.ApprovalAlways || !a.Capabilities.CanSpawn ||
		!strings.HasPrefix(a.Name, "hi · ") || string(a.Model) != `{"provider":"fake","model":"fake"}` {
		t.Fatalf("agent = %+v", a)
	}
	gs, _ := e.st.ListGrants(ctx, a.ID)
	if len(gs) != 1 || gs[0].Label != "laptop" || gs[0].Project != "*" || gs[0].Server != "*" || !gs[0].Allowed || gs[0].Source != store.GrantExplicit {
		t.Fatalf("grants = %+v", gs)
	}
	// Default profile when none named; two chats never collide on the agent name.
	c2, err := e.m.CreateChat(ctx, CreateChatInput{ClientLabel: "laptop"})
	if err != nil {
		t.Fatal(err)
	}
	c3, err := e.m.CreateChat(ctx, CreateChatInput{ClientLabel: "laptop"})
	if err != nil || c2.AgentID == c3.AgentID {
		t.Fatalf("second chat: %v", err)
	}
	def, _ := e.st.GetDefaultProfile(ctx)
	if *c2.ProfileID != def.ID {
		t.Fatalf("default profile not used: %+v", c2)
	}
	// Deleting the profile leaves the chat and agent working.
	if err := e.m.DeleteProfile(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := e.st.GetChat(ctx, chat.ID)
	if got.ProfileID != nil || got.ClientLabel == nil {
		t.Fatalf("chat after profile delete: %+v", got)
	}
}

func TestChatToolsMatchesTheRunCatalogAndHubTools(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.scripted("fake")
	e.addServer("laptop", "", "fs", registry.ToolInfo{Name: "read"})
	e.addServer("other", "", "fs", registry.ToolInfo{Name: "read"})
	p, _ := e.m.CreateProfile(ctx, ProfileInput{Name: "Boss", SystemPrompt: "x", Capabilities: store.Capabilities{CanSpawn: true}})
	chat, err := e.m.CreateChat(ctx, CreateChatInput{ProfileID: p.ID, ClientLabel: "laptop"})
	if err != nil {
		t.Fatal(err)
	}
	view, err := e.m.ChatTools(ctx, chat.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !view.ClientConnected || *view.ClientLabel != "laptop" {
		t.Fatalf("view = %+v", view)
	}
	cat, err := e.m.buildCatalog(ctx, e.agentNow(chat.AgentID))
	if err != nil || len(cat.tools) != len(view.Tools) {
		t.Fatalf("catalog %d vs view %d (%v)", len(cat.tools), len(view.Tools), err)
	}
	var mcpN, hubN int
	for i, tl := range view.Tools {
		if tl.Name != cat.tools[i].Name {
			t.Errorf("tool %d = %s, catalog %s", i, tl.Name, cat.tools[i].Name)
		}
		if strings.Contains(tl.Name, "other") {
			t.Errorf("another client's tool leaked: %s", tl.Name)
		}
		switch tl.Origin {
		case "mcp":
			mcpN++
			if tl.Server == nil || tl.Server.Label != "laptop" || tl.Server.Server != "fs" {
				t.Errorf("server = %+v", tl.Server)
			}
		case "hub":
			hubN++
			if !strings.HasPrefix(tl.Name, "switchboard.") {
				t.Errorf("hub tool name %q", tl.Name)
			}
		}
	}
	if mcpN != 1 || hubN == 0 {
		t.Fatalf("mcp=%d hub=%d", mcpN, hubN)
	}

	req := map[string]string{}
	for _, ht := range e.m.HubTools() {
		req[ht.Name] = ht.Requires
	}
	if len(req) != len(sbTools) || req["switchboard.chat.spawn"] != "canSpawn" || req["switchboard.chat.send"] != "canMessage" ||
		req["switchboard.chat.list"] != "canSpawn or canMessage" {
		t.Fatalf("requires = %v", req)
	}

	// Offline client: only hub tools, clientConnected false.
	e.reg.RemoveConnection("conn-laptop")
	view, _ = e.m.ChatTools(ctx, chat.ID)
	if view.ClientConnected {
		t.Fatal("still connected")
	}
	for _, tl := range view.Tools {
		if tl.Origin != "hub" {
			t.Fatalf("offline chat offers %+v", tl)
		}
	}
	if _, err := e.m.ChatTools(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown chat: %v", err)
	}
}

func TestSpawnInheritsAndNeverReachesAnotherClient(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.scripted("fake")
	e.addServer("laptop", "", "fs", registry.ToolInfo{Name: "read"})
	e.addServer("other", "", "fs", registry.ToolInfo{Name: "read"})
	p, _ := e.m.CreateProfile(ctx, ProfileInput{Name: "Boss", SystemPrompt: "x", Approval: store.ApprovalAlways,
		Capabilities: store.Capabilities{CanSpawn: true, CanMessage: true}})
	chat, err := e.m.CreateChat(ctx, CreateChatInput{ProfileID: p.ID, ClientLabel: "laptop"})
	if err != nil {
		t.Fatal(err)
	}
	parent := e.agentNow(chat.AgentID)
	cc := &callCtx{agent: parent}

	// Asking for another client's grants yields nothing, and asking for every
	// client yields only the parent's.
	for name, grants := range map[string][]any{
		"other": {map[string]any{"label": "other", "server": "*", "project": "*"}},
		"star":  {map[string]any{"label": "*", "server": "*", "project": "*"}},
	} {
		out, err := e.m.toolSpawn(ctx, cc, map[string]any{"title": "kid-" + name, "system_prompt": "s", "grants": grants})
		if err != nil {
			t.Fatal(err)
		}
		kid := e.agentOfChat(out.(map[string]any)["chat_id"].(string))
		cat, err := e.m.buildCatalog(ctx, kid)
		if err != nil {
			t.Fatal(err)
		}
		for _, tl := range cat.tools {
			if strings.Contains(tl.Name, "other") {
				t.Errorf("%s: child can reach another client: %s", name, tl.Name)
			}
		}
		gs, _ := e.st.ListGrants(ctx, kid.ID)
		for _, g := range gs {
			if g.Allowed && g.Label != "laptop" {
				t.Errorf("%s: child holds %+v", name, g)
			}
		}
		if name == "star" && len(gs) != 1 {
			t.Errorf("star grants = %+v", gs)
		}
	}

	out, err := e.m.toolSpawn(ctx, cc, map[string]any{"title": "kid", "system_prompt": "s"})
	if err != nil {
		t.Fatal(err)
	}
	kid := e.agentOfChat(out.(map[string]any)["chat_id"].(string))
	if kid.Approval != store.ApprovalAlways || !kid.Capabilities.CanSpawn || !kid.Capabilities.CanMessage ||
		kid.ProfileID == nil || *kid.ProfileID != p.ID || string(kid.Model) != string(parent.Model) {
		t.Fatalf("child did not inherit: %+v", kid)
	}
	cs, _ := e.st.ListChats(ctx, store.ChatFilter{AgentID: kid.ID})
	if len(cs) != 1 || cs[0].Kind != store.ChatKindHuman || cs[0].PeerAgentID != nil || cs[0].ParentChatID == nil || *cs[0].ParentChatID != chat.ID || cs[0].Title != "kid" || cs[0].ClientLabel == nil || *cs[0].ClientLabel != "laptop" ||
		cs[0].ProfileID == nil || *cs[0].ProfileID != p.ID {
		t.Fatalf("spawn chat = %+v", cs)
	}
	if view, err := e.m.ChatTools(ctx, cs[0].ID); err != nil || *view.ClientLabel != "laptop" {
		t.Fatalf("child chat tools: %+v %v", view, err)
	}

	// Explicit values may only narrow.
	out, err = e.m.toolSpawn(ctx, cc, map[string]any{"title": "narrow", "system_prompt": "s",
		"capabilities": map[string]any{"can_message": true}})
	if err != nil {
		t.Fatal(err)
	}
	narrow := e.agentOfChat(out.(map[string]any)["chat_id"].(string))
	if narrow.Capabilities.CanSpawn || !narrow.Capabilities.CanMessage {
		t.Fatalf("narrowed caps = %+v", narrow.Capabilities)
	}
	// A child without spawn cannot hand spawn on; a laxer approval is refused.
	cc2 := &callCtx{agent: narrow}
	if _, err := e.m.toolSpawn(ctx, cc2, map[string]any{"title": "x", "system_prompt": "s",
		"capabilities": map[string]any{"can_spawn": true}}); err == nil {
		t.Fatal("child was given a capability its parent lacks")
	}
	if _, err := e.m.toolSpawn(ctx, cc, map[string]any{"title": "lax", "system_prompt": "s", "approval": "never"}); err == nil {
		t.Fatal("laxer approval accepted")
	}
}
