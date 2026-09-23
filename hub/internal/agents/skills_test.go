package agents

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/llm"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

func TestSkillValidation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, bad := range []SkillInput{{Name: "", Body: "x"}, {Name: "Has Space", Body: "x"}, {Name: "ok"}} {
		if _, err := e.m.CreateSkill(ctx, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: err = %v, want ErrInvalid", bad, err)
		}
	}
	if _, err := e.m.CreateSkill(ctx, SkillInput{Name: "review", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.CreateSkill(ctx, SkillInput{Name: "review", Body: "b"}); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("duplicate: %v", err)
	}
}

func TestSlashSkillExpandsOnlyWhatTheModelSees(t *testing.T) {
	skills := []store.Skill{{Name: "review", Body: "Review the code.", Auto: true}}
	msgs := []llm.Message{
		llm.TextMessage(llm.RoleUser, "/review main.go please"),
		llm.TextMessage(llm.RoleUser, "/reviewer nope"),
		llm.TextMessage(llm.RoleUser, "/etc/hosts is broken"),
		llm.TextMessage(llm.RoleUser, "/review"),
		llm.TextMessage(llm.RoleAssistant, "/review"),
	}
	out := expandSlashSkills(msgs, skills)
	if got := out[0].Text(); !strings.Contains(got, "Review the code.") || !strings.HasSuffix(got, "main.go please") {
		t.Errorf("expanded = %q", got)
	}
	for _, i := range []int{1, 2, 4} {
		if out[i].Text() != msgs[i].Text() || strings.Contains(out[i].Text(), "<skill") {
			t.Errorf("message %d changed: %q", i, out[i].Text())
		}
	}
	if got := out[3].Text(); !strings.Contains(got, "Review the code.") || strings.Contains(got, "\n\n\n") {
		t.Errorf("bare /review = %q", got)
	}
}

func TestRunSendsExpandedSkillAndOffersLoadTool(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.m.CreateSkill(ctx, SkillInput{Name: "haiku", Description: "write haiku", Body: "Answer in a haiku.", Auto: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.CreateSkill(ctx, SkillInput{Name: "quiet", Description: "manual only", Body: "Whisper.", Auto: false}); err != nil {
		t.Fatal(err)
	}
	p := e.scripted("s", llm.Turn{Text: "ok"})
	a := e.agent("a", withModel("s"))
	res := e.post(a.ID, "/haiku about rain")
	e.waitRun(res.RunID)

	calls := p.Calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d", len(calls))
	}
	c := calls[0]
	if last := c.Messages[len(c.Messages)-1].Text(); !strings.Contains(last, "Answer in a haiku.") || !strings.HasSuffix(last, "about rain") {
		t.Errorf("model saw %q", last)
	}
	if !strings.Contains(c.Options.System, "- haiku: write haiku") || strings.Contains(c.Options.System, "quiet") {
		t.Errorf("system = %q", c.Options.System)
	}
	var offered bool
	for _, tl := range c.Tools {
		offered = offered || tl.Name == providerName(skillLoadName)
	}
	if !offered {
		t.Errorf("skill.load not offered: %+v", c.Tools)
	}
	// What is stored is what was typed.
	if got := storedText(t, e.path(e.chatOf(a.ID))[0]); got != "/haiku about rain" {
		t.Errorf("stored = %q", got)
	}
}

func TestSkillLoadToolReturnsBodyAndNoToolWithoutAutoSkills(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.agent("a", nil)
	cat, err := e.m.buildCatalog(ctx, a)
	if err != nil || cat.byName[skillLoadName] != nil {
		t.Fatalf("no skills yet, tool offered: %v %v", cat.byName[skillLoadName], err)
	}
	if _, err := e.m.CreateSkill(ctx, SkillInput{Name: "haiku", Body: "Answer in a haiku.", Auto: true}); err != nil {
		t.Fatal(err)
	}
	out := e.m.execTool(ctx, a, nil, skillLoadName, map[string]any{"name": "haiku"}, "c1")
	if out.IsError || out.Text() != "Answer in a haiku." {
		t.Fatalf("load = %+v", out)
	}
	if out := e.m.execTool(ctx, a, nil, skillLoadName, map[string]any{"name": "nope"}, "c2"); !out.IsError {
		t.Fatalf("unknown skill loaded: %+v", out)
	}
}

func storedText(t *testing.T, m store.Message) string {
	t.Helper()
	var bs []llm.Block
	if err := json.Unmarshal(m.Content, &bs); err != nil {
		t.Fatal(err)
	}
	return blocksText(bs)
}

func TestUserAskPausesTheRunAndReturnsTheAnswers(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.scripted("s",
		llm.CallTool("switchboard_user_ask", map[string]any{"questions": []any{
			map[string]any{"question": "Which db?", "header": "DB", "options": []any{
				map[string]any{"label": "sqlite"}, map[string]any{"label": "postgres", "description": "bigger"}}},
			map[string]any{"question": "Anything else?"},
		}}),
		llm.Say("thanks"))
	a := e.agent("a", withModel("s"))
	res := e.post(a.ID, "set up storage")

	var pending PendingApproval
	e.waitFor("a pending question", func() bool {
		ps := e.m.PendingApprovals(res.RunID)
		if len(ps) == 1 {
			pending = ps[0]
		}
		return len(ps) == 1
	})
	if len(pending.Questions) != 2 || pending.Questions[0].Options[1].Label != "postgres" || pending.Questions[0].Header != "DB" {
		t.Fatalf("questions = %+v", pending.Questions)
	}
	all, _ := e.m.AllPendingApprovals(ctx)
	if len(all) != 1 || len(all[0].Questions) != 2 {
		t.Fatalf("approvals list = %+v", all)
	}
	// A wrong number of answers, or an empty one, is refused and leaves the question open.
	for _, bad := range [][]QuestionAnswer{{{Answers: []string{"sqlite"}}}, {{Answers: []string{"sqlite"}}, {Answers: []string{"  "}}}} {
		if err := e.m.Answer(ctx, res.RunID, pending.CallID, bad); !errors.Is(err, ErrInvalid) {
			t.Fatalf("bad answers %+v: %v", bad, err)
		}
	}
	good := []QuestionAnswer{{Answers: []string{"sqlite"}}, {Answers: []string{"no", "thanks"}}}
	if err := e.m.Answer(ctx, res.RunID, pending.CallID, good); err != nil {
		t.Fatal(err)
	}
	if run := e.waitRun(res.RunID); run.Status != store.RunDone {
		t.Fatalf("run = %+v", run)
	}
	if err := e.m.Answer(ctx, res.RunID, pending.CallID, good); !errors.Is(err, ErrNotPending) {
		t.Errorf("second answer: %v", err)
	}
	last := p.Calls()[1].Messages
	result := last[len(last)-1].ToolResults[0].Content[0].Text
	for _, want := range []string{`"question":"Which db?"`, `"sqlite"`, `"no","thanks"`} {
		if !strings.Contains(result, want) {
			t.Errorf("tool result %q lacks %s", result, want)
		}
	}
}

func TestUserAskDeclinedAndInvalid(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.agent("a", nil)
	if out := e.m.execTool(ctx, a, nil, "switchboard.user.ask", map[string]any{"questions": []any{map[string]any{"question": "x"}}}, "c"); !out.IsError {
		t.Errorf("no run to pause, yet: %+v", out)
	}
	e.scripted("s", llm.CallTool("switchboard_user_ask", map[string]any{"questions": []any{}}), llm.Say("ok"))
	b := e.agent("b", withModel("s"))
	res := e.post(b.ID, "go")
	if run := e.waitRun(res.RunID); run.Status != store.RunDone {
		t.Fatalf("run = %+v", run)
	}
	rows := e.callRows(store.CallFilter{Source: store.SourceAgent})
	var sawError bool
	for _, r := range rows {
		sawError = sawError || (r.RunID != nil && *r.RunID == res.RunID && r.Status == store.StatusError)
	}
	if !sawError {
		t.Fatalf("an empty questions list should be a tool error: %+v", rows)
	}

	e.scripted("s2", llm.CallTool("switchboard_user_ask", map[string]any{"questions": []any{map[string]any{"question": "sure?"}}}), llm.Say("ok"))
	c := e.agent("c", withModel("s2"))
	res = e.post(c.ID, "go")
	e.waitFor("a pending question", func() bool { return len(e.m.PendingApprovals(res.RunID)) == 1 })
	id := e.m.PendingApprovals(res.RunID)[0].CallID
	if err := e.m.Approve(ctx, res.RunID, id, false, ""); err != nil { // Skip
		t.Fatal(err)
	}
	if run := e.waitRun(res.RunID); run.Status != store.RunDone {
		t.Fatalf("run = %+v", run)
	}
}
