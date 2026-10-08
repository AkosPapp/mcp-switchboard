package library

import (
	"strings"
	"testing"
)

func newTestLib(t *testing.T) *Library {
	t.Helper()
	l, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestSkillRoundTripPreservesExtras(t *testing.T) {
	l := newTestLib(t)
	doc := SkillDoc{Name: "pdf", Description: "work with pdfs", Auto: true,
		License: "MIT", Body: "Do pdf things.\nMore lines."}
	w, err := l.WriteSkill(doc, "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := l.GetSkill("pdf")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != w.ID || got.Description != doc.Description || !got.Auto || got.License != "MIT" || got.Body != doc.Body {
		t.Errorf("round trip lost fields: %+v", got)
	}
	if !strings.HasPrefix(got.Raw, "---\nname") {
		t.Errorf("raw is not a frontmatter doc: %q", got.Raw)
	}
}

func TestSkillCreateRejectsCollisionsAndJunk(t *testing.T) {
	l := newTestLib(t)
	if _, err := l.WriteSkill(SkillDoc{Name: "Bad Name", Body: "x"}, ""); err == nil {
		t.Error("uppercase name accepted")
	}
	if _, err := l.WriteSkill(SkillDoc{Name: "ok", Body: "x"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := l.WriteSkill(SkillDoc{Name: "ok", Body: "again"}, ""); err == nil {
		t.Error("duplicate create accepted")
	}
}

func TestRenameMovesDirectory(t *testing.T) {
	l := newTestLib(t)
	w, err := l.WriteSkill(SkillDoc{Name: "old", Description: "d", Body: "b"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.WriteSkill(SkillDoc{ID: w.ID, Name: "new", Description: "d", Body: "b"}, "old"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.GetSkill("old"); err == nil {
		t.Error("old name still there")
	}
	if got, err := l.GetSkill("new"); err != nil || got.ID != w.ID {
		t.Errorf("new not carried over: %+v %v", got, err)
	}
}

func TestHostSkillsWholesaleReplace(t *testing.T) {
	l := newTestLib(t)
	mk := func(name string) RawSkill {
		return RawSkill{Name: name, Description: "d " + name, Source: "global:.claude/skills",
			Content: "---\nname: " + name + "\ndescription: d\n---\nbody " + name}
	}
	if err := l.PutHostSkills("box", []RawSkill{mk("a"), mk("b")}); err != nil {
		t.Fatal(err)
	}
	if err := l.PutHostSkills("box", []RawSkill{mk("b"), mk("c")}); err != nil {
		t.Fatal(err)
	}
	docs, err := l.ListHostSkills()
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, d := range docs {
		names[d.Host+"/"+d.Name] = true
		if d.Host != "box" {
			t.Errorf("wrong host %q", d.Host)
		}
	}
	if names["box/a"] || !names["box/b"] || !names["box/c"] || len(docs) != 2 {
		t.Errorf("stale set survived or counts off: %+v", docs)
	}
	d, err := l.GetHostSkill("box", "b")
	if err != nil || d.Description != "d" || !strings.Contains(d.Body, "body b") {
		t.Errorf("host doc = %+v %v", d, err)
	}
}

func TestPromptRoundTripAndDefault(t *testing.T) {
	l := newTestLib(t)
	p, err := l.WritePrompt(PromptDoc{Name: "Coding agent", Description: "d", Body: "You are…", Approval: "never"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.WritePrompt(PromptDoc{Name: "Other one", Body: "Hi"}); err != nil {
		t.Fatal(err)
	}
	if err := l.EnsureDefaultPrompt(p.ID); err != nil {
		t.Fatal(err)
	}
	all, err := l.ListPrompts()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || !all[0].IsDefault || all[0].ID != p.ID {
		t.Errorf("list = %+v", all)
	}
	got, err := l.GetPrompt(p.ID)
	if err != nil || got.Approval != "never" || got.Body != "You are…" {
		t.Errorf("prompt = %+v %v", got, err)
	}
	if err := l.DeletePrompt(p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := l.GetPrompt(p.ID); err == nil {
		t.Error("deleted prompt still readable")
	}
}

func TestBasePersonaSeedsOnce(t *testing.T) {
	l := newTestLib(t)
	text, err := l.BasePrompt()
	if err != nil || !strings.Contains(text, "opencode") {
		t.Errorf("seed = %q, %v", text, err)
	}
	if err := l.SetBasePrompt("custom persona"); err != nil {
		t.Fatal(err)
	}
	again, _ := l.BasePrompt()
	if again != "custom persona" {
		t.Errorf("persona not updated: %q", again)
	}
	all, _ := l.ListPrompts()
	for _, p := range all {
		if p.Name == "base" {
			t.Error("base.md must not list as a prompt")
		}
	}
}

func TestParseSkillMarkdownFallbacks(t *testing.T) {
	d, err := ParseSkillMarkdown("body only text", "fallback")
	if err != nil || d.Name != "fallback" || d.Body != "body only text" {
		t.Errorf("no-frontmatter parse: %+v %v", d, err)
	}
	d, err = ParseSkillMarkdown("---\nname: x\ndescription: \"quote: here\"\n---\nbody\n", "ignored")
	if err != nil || d.Name != "x" || d.Description != "quote: here" {
		t.Errorf("quoted parse: %+v %v", d, err)
	}
}
