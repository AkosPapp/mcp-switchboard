package agents

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/registry"
)

// P2-B: the environment brief must be present on turn 1 with no tool call,
// carry the hub-only facts (call deadline, push state) and the client's host
// facts, and a stale brief must be marked rather than trusted.
func TestEnvironmentBriefReachesTurnOnePrompt(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addServer("box", "", "harness", registry.ToolInfo{Name: "file_read"})
	conn := e.reg.Get("conn-box")
	if conn == nil {
		t.Fatal("no connection")
	}
	conn.SetBrief("user: uid=1000 user=akos\nhost: hostname=devbox\nsudo (non-interactive check): unknown", time.Now())
	a := e.agent("coder", func(in *CreateAgentInput) { in.SystemPrompt = "BASE-PROMPT" })

	v, err := e.m.ChatSystemPrompt(ctx, e.newChat(a.ID).ID)
	if err != nil {
		t.Fatal(err)
	}
	prompt := v.SystemPrompt
	for _, want := range []string{
		"BASE-PROMPT",
		"Environment brief",
		"hostname=devbox",
		"unknown",
		"CALL_TIMEOUT",
		"reachability is NOT probed",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("system prompt does not contain %q\n---\n%s", want, prompt)
		}
	}
	if !strings.Contains(prompt, "### box\n") {
		t.Error("client brief not labelled by connection label")
	}
	if strings.Contains(prompt, "STALE") {
		t.Error("fresh brief flagged as stale")
	}
}

func TestStaleEnvironmentBriefIsMarked(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addServer("box", "", "harness", registry.ToolInfo{Name: "file_read"})
	conn := e.reg.Get("conn-box")
	conn.SetBrief("host: hostname=devbox\n", time.Now().Add(-briefStaleAfter-time.Minute))
	a := e.agent("coder", func(in *CreateAgentInput) { in.SystemPrompt = "BASE" })

	v, err := e.m.ChatSystemPrompt(ctx, e.newChat(a.ID).ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(v.SystemPrompt, "STALE") {
		t.Errorf("stale brief not marked\n---\n%s", v.SystemPrompt)
	}
	// Marked, not dropped: the facts are still there to check cheaply.
	if !strings.Contains(v.SystemPrompt, "hostname=devbox") {
		t.Error("stale brief was dropped instead of marked")
	}
}

func TestNoBriefMeansHonestAbsenceLine(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addServer("box", "", "harness", registry.ToolInfo{Name: "file_read"})
	a := e.agent("coder", func(in *CreateAgentInput) { in.SystemPrompt = "BASE" })

	v, err := e.m.ChatSystemPrompt(ctx, e.newChat(a.ID).ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(v.SystemPrompt, "No client host brief was sent") {
		t.Errorf("missing-brief case not stated honestly\n---\n%s", v.SystemPrompt)
	}
	if !strings.Contains(v.SystemPrompt, "CALL_TIMEOUT") {
		t.Error("hub facts dropped together with the missing client brief")
	}
}
