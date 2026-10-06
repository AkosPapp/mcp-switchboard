package agents

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/calls"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/llm"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/registry"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

func TestValidateArgs(t *testing.T) {
	schema := map[string]any{"type": "object",
		"properties": map[string]any{
			"path":  map[string]any{"type": "string"},
			"count": map[string]any{"type": "integer"},
			"mode":  map[string]any{"type": "string", "enum": []any{"r", "w"}},
			"tags":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
		"required": []any{"path"}}
	cases := []struct {
		name string
		args map[string]any
		want string // substring of the problem; "" is valid
	}{
		{"valid", map[string]any{"path": "a", "count": float64(2), "mode": "r", "tags": []any{"x"}}, ""},
		{"missing required", map[string]any{"count": float64(1)}, `missing required property "path"`},
		{"wrong type", map[string]any{"path": float64(3)}, "must be string, got number"},
		{"non-integer", map[string]any{"path": "a", "count": 1.5}, "must be integer"},
		{"bad enum", map[string]any{"path": "a", "mode": "x"}, "must be one of"},
		{"bad item", map[string]any{"path": "a", "tags": []any{"x", true}}, "tags"},
		{"extra keys pass", map[string]any{"path": "a", "other": 1}, ""},
	}
	for _, c := range cases {
		got := validateArgs(schema, c.args)
		if c.want == "" && got != "" || c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
		if c.want != "" && (!strings.Contains(got, "path: string [required]") || !strings.Contains(got, "retry")) {
			t.Errorf("%s: message lacks the schema hint / retry advice: %q", c.name, got)
		}
	}
	for _, s := range []map[string]any{nil, {}, {"type": "object"}, {"type": "object", "properties": map[string]any{}}} {
		if got := validateArgs(s, map[string]any{}); got != "" {
			t.Errorf("schema %v rejected empty args: %q", s, got)
		}
	}
	if got := validateArgs(map[string]any{"type": "object", "properties": map[string]any{}}, map[string]any{"x": 1}); got != "" {
		t.Errorf("no-properties schema rejected extra args: %q", got)
	}
}

func TestNormalizeArgsDecodesStringifiedObject(t *testing.T) {
	schema := map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}}
	for _, in := range []map[string]any{
		{"arguments": `{"path":"a"}`},
		{"_raw_arguments": `{"path":"a"}`},
		{"_raw_arguments": `"{\"path\":\"a\"}"`},
	} {
		got := normalizeArgs(schema, in)
		if got["path"] != "a" || len(got) != 1 {
			t.Errorf("normalizeArgs(%v) = %v", in, got)
		}
	}
	// A declared property is never unwrapped, nor are ordinary args touched.
	decl := map[string]any{"properties": map[string]any{"arguments": map[string]any{"type": "string"}}}
	if got := normalizeArgs(decl, map[string]any{"arguments": `{"a":1}`}); got["arguments"] == nil {
		t.Errorf("declared property was unwrapped: %v", got)
	}
	if got := normalizeArgs(schema, map[string]any{"path": `{"a":1}`}); got["path"] == nil {
		t.Errorf("ordinary args were unwrapped: %v", got)
	}
}

// setSchema replaces the tools of the server addServer registered, adding a schema.
func (e *env) setSchema(server, tool string, schema map[string]any) {
	e.t.Helper()
	for _, ent := range e.reg.IterServers() {
		if ent.Channel.Name != server {
			continue
		}
		tools := ent.Channel.Tools()
		for i := range tools {
			if tools[i].Name == tool {
				tools[i].InputSchema = schema
			}
		}
		ent.Channel.SetTools(tools)
	}
}

func TestExecToolValidatesArgumentsBeforeRunning(t *testing.T) {
	e := newEnv(t)
	e.addServer("box", "", "demo", registry.ToolInfo{Name: "echo"})
	e.setSchema("demo", "echo", map[string]any{"type": "object",
		"properties": map[string]any{"message": map[string]any{"type": "string"}}, "required": []any{"message"}})
	a := e.agent("a", func(in *CreateAgentInput) {
		in.Grants = []store.Grant{{Label: "*", Project: "*", Server: "*", Allowed: true}}
	})
	ctx := context.Background()

	bad := e.m.execTool(ctx, a, nil, "box__demo__echo", map[string]any{}, "")
	if !bad.IsError || !strings.Contains(bad.Text(), `missing required property "message"`) || !strings.Contains(bad.Text(), "retry") {
		t.Fatalf("bad call: %+v", bad)
	}
	good := e.m.execTool(ctx, a, nil, "box__demo__echo", map[string]any{"message": "hi"}, "")
	if good.IsError || !strings.Contains(good.Text(), "echo ran") {
		t.Fatalf("good call: %+v", good)
	}
	str := e.m.execTool(ctx, a, nil, "box__demo__echo", map[string]any{"_raw_arguments": `{"message":"hi"}`}, "")
	if str.IsError {
		t.Fatalf("stringified args: %+v", str)
	}
	rows := e.callRows(store.CallFilter{})
	var errs, denied, oks int
	for _, r := range rows {
		switch r.Status {
		case store.StatusError:
			errs++
		case store.StatusDenied:
			denied++
		case store.StatusOK:
			oks++
		}
	}
	if errs != 1 || denied != 0 || oks != 2 {
		t.Fatalf("logged calls: error=%d denied=%d ok=%d (%+v)", errs, denied, oks, rows)
	}
}

func TestHubToolArgsAreValidated(t *testing.T) {
	e := newEnv(t)
	boss := e.agent("boss", caps(true, true))
	out := e.m.execTool(context.Background(), boss, nil, "switchboard.chat.spawn", map[string]any{"title": "x", "approval": "sometimes"}, "")
	if !out.IsError || !strings.Contains(out.Text(), "system_prompt") || !strings.Contains(out.Text(), "must be one of") {
		t.Fatalf("out = %+v", out)
	}
}

func TestToolAllowGlobs(t *testing.T) {
	if !toolAllowed(nil, "anything") || !toolAllowed([]string{"file_read", "git_*"}, "git_status") ||
		toolAllowed([]string{"file_read", "git_*"}, "file_write") {
		t.Fatal("toolAllowed")
	}
	if _, err := deriveToolAllow(nil, []string{"[bad"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad glob: %v", err)
	}
	if got, err := deriveToolAllow([]string{"git_*"}, nil); err != nil || len(got) != 1 {
		t.Errorf("inherit: %v %v", got, err)
	}
	if got, err := deriveToolAllow([]string{"git_*", "file_read"}, []string{"git_status", "git_*"}); err != nil || len(got) != 2 {
		t.Errorf("narrow: %v %v", got, err)
	}
	for _, bad := range []string{"*", "file_write", "git*"} {
		if _, err := deriveToolAllow([]string{"git_*", "file_read"}, []string{bad}); !errors.Is(err, calls.ErrDenied) {
			t.Errorf("widening %q accepted: %v", bad, err)
		}
	}
}

func TestSpawnAllowedToolsFiltersCatalogAndExec(t *testing.T) {
	e := newEnv(t)
	e.addServer("box", "", "demo", registry.ToolInfo{Name: "file_read"}, registry.ToolInfo{Name: "git_status"}, registry.ToolInfo{Name: "rm_all"})
	e.scripted("kid", llm.Say("ok"))
	boss := e.agent("boss", chain(withModel("kid"), caps(true, true), func(in *CreateAgentInput) {
		in.Grants = []store.Grant{{Label: "*", Project: "*", Server: "*", Allowed: true}}
	}))
	ctx := context.Background()
	out, err := e.m.toolSpawn(ctx, &callCtx{agent: boss}, map[string]any{
		"title": "kid", "system_prompt": "s", "allowed_tools": []any{"file_read", "git_*"}})
	if err != nil {
		t.Fatal(err)
	}
	kid := e.agentOfChat(out.(map[string]any)["chat_id"].(string))
	if strings.Join(kid.ToolAllow, ",") != "file_read,git_*" {
		t.Fatalf("stored allow = %v", kid.ToolAllow)
	}
	cat, err := e.m.buildCatalog(ctx, kid, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cat.byName["box__demo__file_read"] == nil || cat.byName["box__demo__git_status"] == nil || cat.byName["box__demo__rm_all"] != nil {
		t.Fatalf("catalog = %v", cat.byName)
	}
	if o := e.m.execTool(ctx, kid, nil, "box__demo__rm_all", map[string]any{}, ""); !o.IsError || !strings.Contains(o.Text(), "not in this chat's allowed tools") {
		t.Fatalf("rm_all: %+v", o)
	}
	if o := e.m.execTool(ctx, kid, nil, "box__demo__file_read", map[string]any{}, ""); o.IsError {
		t.Fatalf("file_read: %+v", o)
	}
	// The parent is unaffected.
	if cat, _ := e.m.buildCatalog(ctx, boss, nil); cat.byName["box__demo__rm_all"] == nil {
		t.Error("parent lost a tool")
	}
	// A grandchild can only narrow the child's list.
	kid.Capabilities = store.Capabilities{CanSpawn: true, CanMessage: true}
	if _, err := e.m.toolSpawn(ctx, &callCtx{agent: kid}, map[string]any{"title": "g", "system_prompt": "s", "allowed_tools": []any{"*"}}); err == nil {
		t.Error("child widened its allowlist")
	}
	if _, err := e.m.toolSpawn(ctx, &callCtx{agent: kid}, map[string]any{"title": "g2", "system_prompt": "s", "allowed_tools": []any{"git_status"}}); err != nil {
		t.Errorf("child narrowing rejected: %v", err)
	}
	// The stored value survives a reload and shows in the JSON view.
	if got := e.agentNow(kid.ID); len(got.ToolAllow) != 2 {
		t.Errorf("reloaded = %v", got.ToolAllow)
	}
}

func TestChatReportDeliversFixedFormatToParentAndWakes(t *testing.T) {
	e := newEnv(t)
	e.scripted("boss", llm.Say("noted"))
	e.scripted("kid", llm.Say("working"))
	boss := e.agent("boss", chain(withModel("boss"), caps(true, true)))
	ctx := context.Background()
	out, err := e.m.toolSpawn(ctx, &callCtx{agent: boss}, map[string]any{
		"title": "worker", "system_prompt": "s", "model": map[string]any{"provider": "kid", "model": "fake"}})
	if err != nil {
		t.Fatal(err)
	}
	kidChat := out.(map[string]any)["chat_id"].(string)
	kid := e.agentOfChat(kidChat)

	// Only a chat with a parent is offered the tool.
	if cat, _ := e.m.buildCatalog(ctx, boss, nil); cat.byName["switchboard.chat.report"] != nil {
		t.Error("a root chat is offered chat.report")
	}
	if cat, _ := e.m.buildCatalog(ctx, kid, nil); cat.byName["switchboard.chat.report"] == nil {
		t.Error("a child is not offered chat.report")
	}
	if o := e.m.execTool(ctx, boss, nil, "switchboard.chat.report", map[string]any{"status": "done", "summary": "x"}, ""); !o.IsError {
		t.Error("root chat could report")
	}

	o := e.m.execTool(ctx, kid, nil, "switchboard.chat.report", map[string]any{
		"status": "blocked", "summary": "need a key", "details": "no API key", "artifacts": []any{"a.txt", "http://x/y"}}, "")
	if o.IsError {
		t.Fatalf("report: %+v", o)
	}
	want := "[report status=blocked]\nneed a key\n\nDetails:\nno API key\n\nArtifacts:\n- a.txt\n- http://x/y"
	e.waitFor("report in parent chat", func() bool {
		for _, m := range e.path(e.chatOf(boss.ID)) {
			if mt, ok := parseSender(m.Sender); ok && mt.ChatID == kidChat && msgText(m) == want {
				return true
			}
		}
		return false
	})
	e.waitFor("parent woken", func() bool {
		rs, _ := e.st.ListRuns(ctx, store.RunFilter{AgentID: boss.ID})
		return len(rs) == 1 && terminal(rs[0].Status)
	})

	if o := e.m.execTool(ctx, kid, nil, "switchboard.chat.report", map[string]any{"status": "weird", "summary": "x"}, ""); !o.IsError {
		t.Error("bad status accepted")
	}
	if got := reportText("done", "ok", "", nil); got != "[report status=done]\nok" {
		t.Errorf("minimal report = %q", got)
	}
	ann := sbByName["switchboard.chat.report"].ann
	if ann.ReadOnlyHint || ann.DestructiveHint == nil || *ann.DestructiveHint || ann.OpenWorldHint == nil || *ann.OpenWorldHint {
		t.Errorf("annotations = %+v", ann)
	}
}
