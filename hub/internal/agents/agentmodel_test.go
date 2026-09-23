package agents

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/llm"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/registry"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

func sptr(s string) *string { return &s }

func (e *env) manual(name string, mut func(*CreateAgentInput)) *store.Agent {
	e.t.Helper()
	in := CreateAgentInput{Name: name, ClientSet: true}
	if mut != nil {
		mut(&in)
	}
	a, err := e.m.CreateAgent(context.Background(), in)
	if err != nil {
		e.t.Fatalf("CreateAgent %s: %v", name, err)
	}
	return a
}

func (e *env) newChat(agentID string) *store.Chat {
	e.t.Helper()
	c, err := e.st.CreateChat(context.Background(), store.Chat{AgentID: agentID, Kind: store.ChatKindHuman})
	if err != nil {
		e.t.Fatal(err)
	}
	return &c
}

func (e *env) profile(name, prompt string, caps store.Capabilities) *store.Profile {
	e.t.Helper()
	p, err := e.m.CreateProfile(context.Background(), ProfileInput{
		Name: name, SystemPrompt: prompt, Capabilities: caps,
		Model: json.RawMessage(`{"provider":"p","model":"fake"}`),
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

func TestManualAgentCreation(t *testing.T) {
	e := newEnv(t) // default grants {*,*,*} must not apply
	ctx := context.Background()
	e.scripted("p")
	prof := e.profile("Coder", "you code", store.Capabilities{CanSpawn: true})

	// client + profile
	a := e.manual("a", func(in *CreateAgentInput) { in.ClientLabel = sptr("laptop"); in.ProfileID = prof.ID })
	if a.Origin != store.OriginManual || a.ClientLabel == nil || *a.ClientLabel != "laptop" ||
		a.ProfileID == nil || *a.ProfileID != prof.ID || a.SystemPrompt != "you code" || !a.Capabilities.CanSpawn {
		t.Fatalf("agent = %+v", a)
	}
	gs, _ := e.st.ListGrants(ctx, a.ID)
	if len(gs) != 1 || gs[0].Label != "laptop" || gs[0].Project != "*" || gs[0].Server != "*" || !gs[0].Allowed || gs[0].Source != store.GrantExplicit {
		t.Fatalf("grants = %+v", gs)
	}
	if cs, _ := e.st.ListChats(ctx, store.ChatFilter{AgentID: a.ID}); len(cs) != 0 {
		t.Fatalf("a manual agent got an automatic chat: %+v", cs)
	}

	// no client, no profile, no prompt
	b := e.manual("b", func(in *CreateAgentInput) { in.SystemPrompt = "own" })
	if b.ClientLabel != nil || b.ProfileID != nil || b.SystemPrompt != "own" || b.Origin != store.OriginManual {
		t.Fatalf("b = %+v", b)
	}
	if gs, _ := e.st.ListGrants(ctx, b.ID); len(gs) != 0 {
		t.Fatalf("client-less agent has grants: %+v", gs)
	}
	c := e.manual("c", nil)
	if c.SystemPrompt != "" {
		t.Fatalf("no prompt expected: %q", c.SystemPrompt)
	}

	// validation and name uniqueness
	for _, bad := range []string{"*", "", "  "} {
		if _, err := e.m.CreateAgent(ctx, CreateAgentInput{Name: "x", ClientSet: true, ClientLabel: sptr(bad)}); !errors.Is(err, ErrInvalid) {
			t.Errorf("clientLabel %q: %v", bad, err)
		}
	}
	if _, err := e.m.CreateAgent(ctx, CreateAgentInput{Name: "a", ClientSet: true}); !errors.Is(err, ErrNameTaken) {
		t.Errorf("duplicate name: %v", err)
	}
	if _, err := e.m.CreateAgent(ctx, CreateAgentInput{Name: "y", ClientSet: true, ProfileID: "nope"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown profile: %v", err)
	}

	// legacy path (no clientLabel key): default grants, origin chat
	l := e.agent("legacy", nil)
	if l.Origin != store.OriginChat || l.ClientLabel != nil {
		t.Fatalf("legacy = %+v", l)
	}
	if gs, _ := e.st.ListGrants(ctx, l.ID); len(gs) != 1 || gs[0].Label != "*" {
		t.Fatalf("legacy grants = %+v", gs)
	}
}

func TestPatchClientSwapNarrowsChild(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.scripted("p")
	root := e.manual("root", func(in *CreateAgentInput) {
		in.ClientLabel = sptr("a")
		in.Capabilities = store.Capabilities{CanSpawn: true}
	})
	kid, err := e.m.CreateAgent(ctx, CreateAgentInput{ParentID: root.ID, Name: "kid"})
	if err != nil {
		t.Fatal(err)
	}
	if kid.Origin != store.OriginSpawn || kid.ClientLabel == nil || *kid.ClientLabel != "a" {
		t.Fatalf("kid = %+v", kid)
	}
	if gs, _ := e.st.ListGrants(ctx, kid.ID); len(gs) != 1 || gs[0].Label != "a" || gs[0].Source != store.GrantInherited {
		t.Fatalf("kid grants = %+v", gs)
	}

	got, err := e.m.UpdateAgent(ctx, root.ID, store.AgentPatch{SetClient: true, ClientLabel: sptr("b")})
	if err != nil {
		t.Fatal(err)
	}
	if got.ClientLabel == nil || *got.ClientLabel != "b" {
		t.Fatalf("patched = %+v", got)
	}
	gs, _ := e.st.ListGrants(ctx, root.ID)
	if len(gs) != 1 || gs[0].Label != "b" || gs[0].Source != store.GrantExplicit {
		t.Fatalf("root grants = %+v", gs)
	}
	if kg, _ := e.st.ListGrants(ctx, kid.ID); len(kg) != 0 {
		t.Fatalf("child kept grants: %+v", kg)
	}
	if k := e.agentNow(kid.ID); k.ClientLabel != nil {
		t.Fatalf("child client = %v", *k.ClientLabel)
	}

	// changing to null drops the client grants
	if _, err := e.m.UpdateAgent(ctx, root.ID, store.AgentPatch{SetClient: true}); err != nil {
		t.Fatal(err)
	}
	if gs, _ := e.st.ListGrants(ctx, root.ID); len(gs) != 0 {
		t.Fatalf("grants after null: %+v", gs)
	}
	if _, err := e.m.UpdateAgent(ctx, root.ID, store.AgentPatch{SetClient: true, ClientLabel: sptr("*")}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("* accepted: %v", err)
	}
	// profileId and name
	p := e.profile("P", "pp", store.Capabilities{})
	got, err = e.m.UpdateAgent(ctx, root.ID, store.AgentPatch{SetProfile: true, ProfileID: &p.ID, Name: sptr("renamed")})
	if err != nil || got.Name != "renamed" || got.SystemPrompt != "pp" || got.ProfileID == nil {
		t.Fatalf("profile patch = %+v %v", got, err)
	}
	if _, err := e.m.UpdateAgent(ctx, root.ID, store.AgentPatch{SetProfile: true, ProfileID: sptr("nope")}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown profile: %v", err)
	}
	got, err = e.m.UpdateAgent(ctx, root.ID, store.AgentPatch{SetProfile: true})
	if err != nil || got.ProfileID != nil || got.SystemPrompt != "pp" {
		t.Fatalf("detach = %+v %v", got, err)
	}
}

func TestLiveProfileEditAffectsNextRunAndDeleteKeepsPrompt(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	prov := e.scripted("p", llm.Say("one"), llm.Say("two"), llm.Say("three"))
	prof := e.profile("Coder", "v1 prompt", store.Capabilities{})
	other := e.profile("Other", "unused", store.Capabilities{}) // so Coder is deletable
	on := true
	if _, err := e.m.UpdateProfile(ctx, other.ID, ProfileUpdate{IsDefault: &on}); err != nil {
		t.Fatal(err)
	}
	a := e.manual("a", func(in *CreateAgentInput) { in.ProfileID = prof.ID })
	chat := e.newChat(a.ID)

	post := func(text string) {
		res, err := e.m.Post(ctx, chat.ID, PostInput{Content: say(text)})
		if err != nil {
			t.Fatal(err)
		}
		e.waitRun(res.RunID)
	}
	post("1")
	if got := prov.Calls()[0].Options.System; got != "v1 prompt" {
		t.Fatalf("first system = %q", got)
	}
	np := "v2 prompt"
	if _, err := e.m.UpdateProfile(ctx, prof.ID, ProfileUpdate{SystemPrompt: &np, CanSpawn: &on}); err != nil {
		t.Fatal(err)
	}
	if e.agentNow(a.ID).SystemPrompt != "v2 prompt" {
		t.Fatalf("agent row not synced: %q", e.agentNow(a.ID).SystemPrompt)
	}
	post("2")
	if got := prov.Calls()[1].Options.System; got != "v2 prompt" {
		t.Fatalf("second system = %q", got)
	}
	// the catalog follows the profile's capabilities too
	if names := toolNames(prov.Calls()[1].Tools); !strings.Contains(names, "switchboard_chat_spawn") {
		t.Fatalf("capabilities not live: %s", names)
	}

	if err := e.m.DeleteProfile(ctx, prof.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.agentNow(a.ID); got.ProfileID != nil || got.SystemPrompt != "v2 prompt" {
		t.Fatalf("after delete: %+v", got)
	}
	post("3")
	if got := prov.Calls()[2].Options.System; got != "v2 prompt" {
		t.Fatalf("third system = %q", got)
	}
}

func toolNames(ts []llm.Tool) string {
	var n []string
	for _, t := range ts {
		n = append(n, t.Name)
	}
	return strings.Join(n, ",")
}

func TestClientlessAgentSeesHubToolsOnlyAndPromptEndpointMatchesProvider(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	prov := e.scripted("p", llm.Say("hi"))
	e.addServer("laptop", "", "fs", registry.ToolInfo{Name: "read"})
	prof := e.profile("Coder", "be brief", store.Capabilities{CanMessage: true})
	a := e.manual("a", func(in *CreateAgentInput) { in.ProfileID = prof.ID })
	chat := e.newChat(a.ID)

	v, err := e.m.ChatSystemPrompt(ctx, chat.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v.Source != "profile" || v.SystemPrompt != "be brief" || v.ProfileID == nil || *v.ProfileID != prof.ID ||
		v.ProfileName == nil || *v.ProfileName != "Coder" || v.Model == nil || v.Model.Provider != "p" || v.Model.Model != "fake" {
		t.Fatalf("view = %+v", v)
	}
	res, err := e.m.Post(ctx, chat.ID, PostInput{Content: say("x")})
	if err != nil {
		t.Fatal(err)
	}
	e.waitRun(res.RunID)
	call := prov.Calls()[0]
	if call.Options.System != v.SystemPrompt || len(call.Tools) != v.ToolCount {
		t.Fatalf("provider got system=%q tools=%d; endpoint said %q / %d", call.Options.System, len(call.Tools), v.SystemPrompt, v.ToolCount)
	}
	if v.ToolCount == 0 {
		t.Fatal("expected hub tools")
	}
	for _, tl := range call.Tools {
		if !strings.HasPrefix(tl.Name, "switchboard_") {
			t.Errorf("client-less agent sees %s", tl.Name)
		}
	}
	tv, err := e.m.ChatTools(ctx, chat.ID)
	if err != nil || len(tv.Tools) != v.ToolCount {
		t.Fatalf("tools view = %+v %v", tv, err)
	}

	// own prompt, and no prompt at all
	own := e.manual("own", func(in *CreateAgentInput) { in.SystemPrompt = "mine" })
	if v, _ := e.m.ChatSystemPrompt(ctx, e.newChat(own.ID).ID); v.Source != "agent" || v.SystemPrompt != "mine" || v.ProfileID != nil || v.ProfileName != nil {
		t.Fatalf("own = %+v", v)
	}
	none := e.manual("none", nil)
	if v, _ := e.m.ChatSystemPrompt(ctx, e.newChat(none.ID).ID); v.Source != "none" || v.SystemPrompt != "" {
		t.Fatalf("none = %+v", v)
	}
	if _, err := e.m.ChatSystemPrompt(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown chat: %v", err)
	}
}

func TestLegacyCreateChatWithoutClient(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.scripted("p")
	chat, err := e.m.CreateChat(ctx, CreateChatInput{Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	a := e.agentNow(chat.AgentID)
	if a.Origin != store.OriginChat || a.ClientLabel != nil || chat.ClientLabel != nil {
		t.Fatalf("agent = %+v chat = %+v", a, chat)
	}
	if gs, _ := e.st.ListGrants(ctx, a.ID); len(gs) != 0 {
		t.Fatalf("grants = %+v", gs)
	}
	withClient, err := e.m.CreateChat(ctx, CreateChatInput{ClientLabel: "laptop"})
	if err != nil {
		t.Fatal(err)
	}
	if a := e.agentNow(withClient.AgentID); a.Origin != store.OriginChat || a.ClientLabel == nil || *a.ClientLabel != "laptop" {
		t.Fatalf("agent = %+v", a)
	}
}

// ChatSystemPrompt.ModelIsDefault: true when the effective model is exactly
// the hub's first configured model (what a turn falls back to), false when a
// profile names a different, later-registered model explicitly.
func TestChatSystemPromptModelIsDefault(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.provider(llm.NewScripted("p", llm.Say("hi"))) // first configured model: p/fake
	other := llm.NewScripted("q", llm.Say("hi"))
	e.llm.Register(other, llm.ModelSpec{Model: "other", Priced: true})

	// No explicit model anywhere: falls back to the first configured model.
	fallback := e.manual("fallback", nil)
	v, err := e.m.ChatSystemPrompt(ctx, e.newChat(fallback.ID).ID)
	if err != nil {
		t.Fatal(err)
	}
	if v.Model == nil || v.Model.Provider != "p" || v.Model.Model != "fake" || !v.ModelIsDefault {
		t.Fatalf("fallback view = %+v", v)
	}

	// A profile naming a different model: not the default.
	prof, err := e.m.CreateProfile(ctx, ProfileInput{
		Name: "Other", SystemPrompt: "be other", Model: json.RawMessage(`{"provider":"q","model":"other"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	picked := e.manual("picked", func(in *CreateAgentInput) { in.ProfileID = prof.ID })
	v2, err := e.m.ChatSystemPrompt(ctx, e.newChat(picked.ID).ID)
	if err != nil {
		t.Fatal(err)
	}
	if v2.Model == nil || v2.Model.Provider != "q" || v2.Model.Model != "other" || v2.ModelIsDefault {
		t.Fatalf("picked view = %+v", v2)
	}
}
