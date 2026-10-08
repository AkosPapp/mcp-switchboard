package agents

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/calls"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/config"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/events"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/library"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/llm"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/registry"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

func newLibEnv(t *testing.T) (*env, *library.Library) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(ctx, store.Options{Path: filepath.Join(dir, "hub.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	lib, err := library.New(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	set := config.Settings{AgentsEnabled: true, AgentMaxDepth: 4, AgentMaxChildren: 8,
		AgentMaxConcurrentRuns: 16, AgentMaxParallelToolCalls: 8, AgentReplyTimeout: 10,
		ApprovalTimeout: 3600, AgentDefaultBudget: config.DefaultAgentBudget()}
	bus := events.NewBus()
	e := &env{t: t, st: st, reg: registry.New(bus), llm: llm.NewRegistry(), strm: &recStreamer{}, set: set}
	e.m = New(Options{Store: st, Registry: e.reg, Dispatcher: calls.NewDispatcher(calls.Options{Store: st, Bus: bus}),
		LLM: e.llm, Bus: bus, Streamer: e.strm, Settings: set, Library: lib})
	if err := e.m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.m.Shutdown(ctx) })
	return e, lib
}

func TestManagedSkillWritesFileAndDiskEditsWin(t *testing.T) {
	ctx := context.Background()
	e, lib := newLibEnv(t)

	k, err := e.m.CreateSkill(ctx, SkillInput{Name: "pdf", Description: "work with pdfs", Body: "v1", Auto: true})
	if err != nil {
		t.Fatal(err)
	}
	d, err := lib.GetSkill("pdf")
	if err != nil || d.ID != k.ID || !strings.Contains(d.Raw, "auto: true") {
		t.Fatalf("mirror after create: %+v %v", d, err)
	}

	// A human edits the file on disk with a text editor: the next read wins.
	path := filepath.Join(lib.Dir(), "skills", "pdf", "SKILL.md")
	edited := "---\nname: pdf\ndescription: edited by hand\nauto: false\nlicense: MIT\n---\nedited body\n"
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	rows, err := e.m.ListSkills(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %+v %v", rows, err)
	}
	if rows[0].Description != "edited by hand" || rows[0].Body != "edited body" || rows[0].Auto {
		t.Errorf("disk edit not adopted: %+v", rows[0])
	}
	// Extras survive the Manager's next rewrite.
	if _, err := e.m.UpdateSkill(ctx, rows[0].ID, SkillUpdate{Body: libStr("v3")}); err != nil {
		t.Fatal(err)
	}
	d, _ = lib.GetSkill("pdf")
	if !strings.Contains(d.Raw, `license: "MIT"`) || !strings.Contains(d.Body, "v3") {
		t.Errorf("extras not preserved or update not mirrored: %q", d.Raw)
	}

	// Deleting the file deletes the row (disk is the record).
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.ListSkills(ctx); err != nil {
		t.Fatal(err)
	}
	rows, _ = e.m.ListSkills(ctx)
	if len(rows) != 0 {
		t.Errorf("row survived file deletion: %+v", rows)
	}
}

func TestHandWrittenSkillAdoptedAndPromptMirrored(t *testing.T) {
	ctx := context.Background()
	e, lib := newLibEnv(t)

	dir := filepath.Join(lib.Dir(), "skills", "hand-made")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"),
		[]byte("---\nname: hand-made\ndescription: written on disk\n---\nstand up\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rows, err := e.m.ListSkills(ctx)
	if err != nil || len(rows) != 1 || rows[0].Name != "hand-made" {
		t.Fatalf("adoption failed: %+v %v", rows, err)
	}
	d, _ := lib.GetSkill("hand-made")
	if d.ID != rows[0].ID {
		t.Errorf("id not stamped into the file: %q vs %q", d.ID, rows[0].ID)
	}

	// The seeded default profile exists on disk too (create mirrored it).
	profiles, err := e.m.ListProfiles(ctx)
	if err != nil || len(profiles) != 1 {
		t.Fatalf("seeded profiles = %+v %v", profiles, err)
	}
	if _, err := lib.GetPrompt(profiles[0].ID); err != nil {
		t.Errorf("seeded profile not on disk: %v", err)
	}

	// Persona seeds once and leads the next turn's prompt.
	persona, err := e.m.PersonaPrompt()
	if err != nil || !strings.Contains(persona, "opencode") {
		t.Errorf("persona = %q err %v", persona, err)
	}
}

func libStr(s string) *string { return &s }
