package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
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

func TestProfileRoutesPassInputAndMapErrors(t *testing.T) {
	o := newOrch(t)

	rec := o.do(t, "POST", "/api/profiles", `{"name":"Coder","systemPrompt":"code","capabilities":{"canSpawn":true,"canMessage":false},"approval":"always"}`)
	requireStatus(t, rec, 201)
	if len(o.agents.profileIn) != 1 || o.agents.profileIn[0].Name != "Coder" || !o.agents.profileIn[0].Capabilities.CanSpawn ||
		o.agents.profileIn[0].Approval != "always" {
		t.Fatalf("input = %+v", o.agents.profileIn)
	}

	requireStatus(t, o.do(t, "PATCH", "/api/profiles/p1", `{"isDefault":true,"model":null,"capabilities":{"canMessage":true}}`), 200)
	up := o.agents.profileUp[0]
	if up.IsDefault == nil || !*up.IsDefault || string(up.Model) != "null" || up.CanMessage == nil || up.CanSpawn != nil {
		t.Fatalf("update = %+v", up)
	}
	requireStatus(t, o.do(t, "GET", "/api/profiles/p1", ""), 200)
	requireStatus(t, o.do(t, "DELETE", "/api/profiles/p1", ""), 204)

	rec = o.do(t, "GET", "/api/profiles", "")
	requireStatus(t, rec, 200)
	if !strings.Contains(rec.Body.String(), `"profiles":[]`) {
		t.Fatalf("empty list body = %s", rec.Body.String())
	}

	for err, want := range map[error]int{
		agents.ErrConflict: 409, store.ErrNameTaken: 409, agents.ErrInvalid: 400, agents.ErrNotFound: 404,
	} {
		o.agents.err = err
		requireStatus(t, o.do(t, "DELETE", "/api/profiles/p1", ""), want)
		requireStatus(t, o.do(t, "POST", "/api/profiles", `{"name":"x","systemPrompt":"y"}`), want)
	}
	o.agents.err = errors.New("boom")
	requireStatus(t, o.do(t, "GET", "/api/profiles", ""), 500)
}

func TestCreateChatNewAndLegacyForms(t *testing.T) {
	o := newOrch(t)
	rec := o.do(t, "POST", "/api/chats", `{"profileId":"p1","clientLabel":"laptop","title":"T"}`)
	requireStatus(t, rec, 201)
	if len(o.agents.chatIn) != 1 || o.agents.chatIn[0].ProfileID != "p1" || o.agents.chatIn[0].ClientLabel != "laptop" || o.agents.chatIn[0].Title != "T" {
		t.Fatalf("chatIn = %+v", o.agents.chatIn)
	}
	if !strings.Contains(rec.Body.String(), `"clientLabel":"laptop"`) || !strings.Contains(rec.Body.String(), `"profileId":null`) {
		t.Fatalf("chat json = %s", rec.Body.String())
	}
	for _, body := range []string{`{"clientLabel":"*"}`} {
		requireStatus(t, o.do(t, "POST", "/api/chats", body), 400)
	}
	// Legacy {agentId} still works and does not touch the profile path.
	a := o.agent(t, "legacy")
	requireStatus(t, o.do(t, "POST", "/api/chats", `{"agentId":"`+a.ID+`"}`), 201)
	if len(o.agents.chatIn) != 1 {
		t.Fatal("legacy form went through CreateChat")
	}
}

func TestHubToolsAndChatToolsRoutes(t *testing.T) {
	o := newOrch(t)
	o.agents.hubTools = []agents.HubTool{{Name: "switchboard.chat.spawn", Requires: "canSpawn", InputSchema: map[string]any{"type": "object"}}}
	rec := o.do(t, "GET", "/api/hub-tools", "")
	requireStatus(t, rec, 200)
	if !strings.Contains(rec.Body.String(), `"requires":"canSpawn"`) || !strings.Contains(rec.Body.String(), `"annotations":{}`) {
		t.Fatalf("hub-tools = %s", rec.Body.String())
	}
	rec = o.do(t, "GET", "/api/chats/c1/tools", "")
	requireStatus(t, rec, 200)
	if !strings.Contains(rec.Body.String(), `"clientLabel":null`) || !strings.Contains(rec.Body.String(), `"clientConnected":false`) ||
		!strings.Contains(rec.Body.String(), `"tools":[]`) {
		t.Fatalf("chat tools = %s", rec.Body.String())
	}
	o.agents.err = agents.ErrNotFound
	requireStatus(t, o.do(t, "GET", "/api/chats/nope/tools", ""), 404)
}

func TestProfileRoutes404WhenAgentsDisabled(t *testing.T) {
	f := newFixture(t, nil)
	for _, c := range [][2]string{{"GET", "/api/profiles"}, {"POST", "/api/profiles"}, {"GET", "/api/hub-tools"}, {"GET", "/api/chats/x/tools"}} {
		requireStatus(t, f.do(t, c[0], c[1], ""), http.StatusNotFound)
	}
}

// profileView decodes the wire form (store.Profile only marshals).
type profileView struct {
	ID           string             `json:"id"`
	Name         string             `json:"name"`
	IsDefault    bool               `json:"isDefault"`
	Approval     string             `json:"approval"`
	Capabilities store.Capabilities `json:"capabilities"`
}

// The whole flow against the real Manager and SQLite.
func TestProfilesEndToEndWithRealManager(t *testing.T) {
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

	var list struct{ Profiles []profileView }
	rec := f.do(t, "GET", "/api/profiles", "")
	requireStatus(t, rec, 200)
	decode(t, rec, &list)
	if len(list.Profiles) != 1 || list.Profiles[0].Name != "Assistant" || !list.Profiles[0].IsDefault {
		t.Fatalf("seed: %+v", list.Profiles)
	}
	def := list.Profiles[0]

	requireStatus(t, f.do(t, "DELETE", "/api/profiles/"+def.ID, ""), 409)
	requireStatus(t, f.do(t, "POST", "/api/profiles", `{"name":"Assistant","systemPrompt":"x"}`), 409)
	requireStatus(t, f.do(t, "POST", "/api/profiles", `{"name":"NoPrompt"}`), 400)
	rec = f.do(t, "POST", "/api/profiles", `{"name":"Coder","systemPrompt":"code","capabilities":{"canSpawn":true,"canMessage":true}}`)
	requireStatus(t, rec, 201)
	var coder profileView
	decode(t, rec, &coder)
	if coder.IsDefault || coder.Approval != "destructive" || !coder.Capabilities.CanSpawn {
		t.Fatalf("coder = %+v", coder)
	}
	requireStatus(t, f.do(t, "PATCH", "/api/profiles/"+def.ID, `{"isDefault":false}`), 400)
	requireStatus(t, f.do(t, "PATCH", "/api/profiles/"+coder.ID, `{"isDefault":true}`), 200)
	requireStatus(t, f.do(t, "GET", "/api/profiles/nope", ""), 404)

	rec = f.do(t, "POST", "/api/chats", `{"clientLabel":"laptop"}`)
	requireStatus(t, rec, 201)
	var chat struct {
		ID          string  `json:"id"`
		ProfileID   *string `json:"profileId"`
		ClientLabel *string `json:"clientLabel"`
	}
	decode(t, rec, &chat)
	if chat.ProfileID == nil || *chat.ProfileID != coder.ID || chat.ClientLabel == nil || *chat.ClientLabel != "laptop" {
		t.Fatalf("chat = %+v", chat)
	}
	requireStatus(t, f.do(t, "POST", "/api/chats", `{"clientLabel":"*"}`), 400)
	requireStatus(t, f.do(t, "POST", "/api/chats", `{"clientLabel":"l","profileId":"nope"}`), 404)

	rec = f.do(t, "GET", "/api/chats/"+chat.ID+"/tools", "")
	requireStatus(t, rec, 200)
	var view agents.ChatToolsView
	decode(t, rec, &view)
	if view.ClientConnected || view.ClientLabel == nil || len(view.Tools) == 0 {
		t.Fatalf("tools = %+v", view)
	}
	for _, tl := range view.Tools {
		if tl.Origin != "hub" || !strings.HasPrefix(tl.Name, "switchboard.") {
			t.Fatalf("offline chat has %+v", tl)
		}
	}
	rec = f.do(t, "GET", "/api/hub-tools", "")
	requireStatus(t, rec, 200)
	var hub struct{ Tools []agents.HubTool }
	decode(t, rec, &hub)
	if len(hub.Tools) < 5 {
		t.Fatalf("hub tools = %d", len(hub.Tools))
	}
	if b, _ := json.Marshal(hub.Tools[0]); !strings.Contains(string(b), `"requires"`) {
		t.Fatal("no requires")
	}

	// The old default is now deletable; the chat made from it keeps working.
	requireStatus(t, f.do(t, "DELETE", "/api/profiles/"+coder.ID, ""), 409)
	requireStatus(t, f.do(t, "DELETE", "/api/profiles/"+def.ID, ""), 204)
	requireStatus(t, f.do(t, "GET", "/api/profiles/"+def.ID, ""), 404)
	rec = f.do(t, "GET", "/api/chats/"+chat.ID, "")
	requireStatus(t, rec, 200)
	decode(t, rec, &chat)
	if chat.ProfileID == nil {
		t.Fatal("chat whose profile is not deleted lost its profile")
	}
}

func TestAgentModelRoutes(t *testing.T) {
	o := newOrch(t)

	// clientLabel key present (even null) => manual; absent => legacy
	requireStatus(t, o.do(t, "POST", "/api/agents", `{"name":"m1","clientLabel":"laptop","profileId":"p1"}`), 201)
	requireStatus(t, o.do(t, "POST", "/api/agents", `{"name":"m2","clientLabel":null}`), 201)
	requireStatus(t, o.do(t, "POST", "/api/agents", `{"name":"legacy"}`), 201)
	requireStatus(t, o.do(t, "POST", "/api/agents", `{"name":"bad","clientLabel":"*"}`), 400)
	requireStatus(t, o.do(t, "POST", "/api/agents", `{"name":"bad","clientLabel":""}`), 400)
	requireStatus(t, o.do(t, "POST", "/api/agents", `{"name":"bad","clientLabel":5}`), 400)
	got := o.agents.created
	if len(got) != 3 || !got[0].ClientSet || got[0].ClientLabel == nil || *got[0].ClientLabel != "laptop" || got[0].ProfileID != "p1" ||
		!got[1].ClientSet || got[1].ClientLabel != nil || got[2].ClientSet {
		t.Fatalf("created = %+v", got)
	}

	// PATCH
	requireStatus(t, o.do(t, "PATCH", "/api/agents/a1", `{"profileId":"p2","clientLabel":"box","name":"n"}`), 200)
	requireStatus(t, o.do(t, "PATCH", "/api/agents/a1", `{"profileId":null,"clientLabel":null}`), 200)
	requireStatus(t, o.do(t, "PATCH", "/api/agents/a1", `{"clientLabel":"*"}`), 400)
	p := o.agents.patches
	if len(p) != 2 || !p[0].SetProfile || *p[0].ProfileID != "p2" || !p[0].SetClient || *p[0].ClientLabel != "box" ||
		!p[1].SetProfile || p[1].ProfileID != nil || !p[1].SetClient || p[1].ClientLabel != nil {
		t.Fatalf("patches = %+v", p)
	}

	// chat under an agent mirrors the agent's profile and client
	a := o.agent(t, "under")
	prof, err := o.db.CreateProfile(context.Background(), store.Profile{Name: "P", SystemPrompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	pid, cl := prof.ID, "laptop"
	if _, err := o.db.UpdateAgent(context.Background(), a.ID, store.AgentPatch{SetProfile: true, ProfileID: &pid}); err != nil {
		t.Fatal(err)
	}
	if _, err := o.db.SetAgentClient(context.Background(), a.ID, &cl); err != nil {
		t.Fatal(err)
	}
	rec := o.do(t, "POST", "/api/chats", `{"agentId":"`+a.ID+`"}`)
	requireStatus(t, rec, 201)
	if !strings.Contains(rec.Body.String(), `"clientLabel":"laptop"`) || !strings.Contains(rec.Body.String(), `"profileId":"`+prof.ID+`"`) {
		t.Fatalf("chat = %s", rec.Body.String())
	}

	// legacy chat creation: clientLabel optional
	requireStatus(t, o.do(t, "POST", "/api/chats", `{}`), 201)
	if n := len(o.agents.chatIn); n != 1 || o.agents.chatIn[0].ClientLabel != "" {
		t.Fatalf("chatIn = %+v", o.agents.chatIn)
	}

	rec = o.do(t, "GET", "/api/chats/c1/system-prompt", "")
	requireStatus(t, rec, 200)
	if !strings.Contains(rec.Body.String(), `"systemPrompt":"sp c1"`) || !strings.Contains(rec.Body.String(), `"toolCount":2`) {
		t.Fatalf("system-prompt = %s", rec.Body.String())
	}
}
