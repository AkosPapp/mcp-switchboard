package agents

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/llm"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/registry"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

// TestChatAutoApproveRunsWhatAlwaysWouldGate covers docs/improvements.md I1:
// once the chat opts into the auto-approver, even a tool the approval mode
// would gate runs without a pause — the user's explicit "pushes included"
// decision.
func TestChatAutoApproveRunsWhatAlwaysWouldGate(t *testing.T) {
	e := newEnv(t)
	e.addServer("box", "", "harness", destructiveTool(), registry.ToolInfo{Name: "safe"})
	e.scripted("p", llm.CallTool("box__harness__danger", map[string]any{"x": 1}), llm.Say("finished"))
	a := e.agent("careful", chain(withModel("p"), func(in *CreateAgentInput) { in.Approval = store.ApprovalAlways }))
	yes := true
	if _, err := e.m.UpdateChat(context.Background(), e.chatOf(a.ID), ChatUpdate{AutoApprove: &yes}); err != nil {
		t.Fatal(err)
	}
	res := e.post(a.ID, "do it")
	if r := e.waitRun(res.RunID); r.Status != store.RunDone {
		t.Fatalf("run = %+v", r)
	}
	if pend := e.m.PendingApprovals(res.RunID); len(pend) != 0 {
		t.Fatalf("auto-approved run paused: %+v", pend)
	}
	if rows := e.callRows(store.CallFilter{Source: store.SourceAgent}); len(rows) != 1 || rows[0].Status != store.StatusOK {
		t.Fatalf("rows = %+v", rows)
	}
}

// TestChatApprovalOverridesAgentMode covers I3: the chat can start "never"
// (the agent's mode) and be switched later to gate every call.
func TestChatApprovalOverridesAgentMode(t *testing.T) {
	e := newEnv(t)
	e.addServer("box", "", "harness", destructiveTool(), registry.ToolInfo{Name: "safe"})
	e.scripted("p",
		llm.CallTool("box__harness__safe", map[string]any{}), llm.Say("finished"),
		llm.CallTool("box__harness__safe", map[string]any{}), llm.Say("second finished"))
	a := e.agent("loose", withModel("p")) // agents default to approval=never
	res := e.post(a.ID, "first, ungated")
	if r := e.waitRun(res.RunID); r.Status != store.RunDone {
		t.Fatalf("run1 = %+v", r)
	}
	if _, err := e.m.UpdateChat(context.Background(), e.chatOf(a.ID), ChatUpdate{SetApproval: true, Approval: store.ApprovalAlways}); err != nil {
		t.Fatal(err)
	}
	res = e.post(a.ID, "now gated")
	e.waitStatus(res.RunID, store.RunWaiting)
	pend := e.m.PendingApprovals(res.RunID)
	if len(pend) != 1 {
		t.Fatalf("chat override did not gate: %+v", pend)
	}
	if err := e.m.Approve(context.Background(), res.RunID, pend[0].CallID, true, ""); err != nil {
		t.Fatal(err)
	}
	if r := e.waitRun(res.RunID); r.Status != store.RunDone {
		t.Fatalf("run2 = %+v", r)
	}
}

// TestCompactCommandCoversOlderHistory covers I11/I15: "/compact" is answered
// by the hub (the scripted queue shows no ordinary turn taken for it — the
// only provider call is the summariser), records the summary on the chat, and
// later turns build on the summary rather than the full prefix.
func TestCompactCommandCoversOlderHistory(t *testing.T) {
	e := newEnv(t)
	// Six exchanged turns (> the keep-minimum), the summary the summariser
	// will produce, and the reply to the post-compact message.
	e.scripted("p",
		llm.Say("r1"), llm.Say("r2"), llm.Say("r3"),
		llm.Say("r4"), llm.Say("r5"), llm.Say("r6"),
		llm.Say("the gist of the early work"),
		llm.Say("after"),
	)
	a := e.agent("worker", withModel("p"))
	chat := e.chatOf(a.ID)
	// A real window makes the keep-tail math meaningful (the scripted model
	// has no declared context window).
	lim := int64(4096)
	if _, err := e.m.UpdateChat(context.Background(), chat, ChatUpdate{ContextLimit: &lim}); err != nil {
		t.Fatal(err)
	}
	for i := range 6 {
		r := e.post(a.ID, string(rune('a'+i))+" answer me")
		if w := e.waitRun(r.RunID); w.Status != store.RunDone {
			t.Fatalf("turn %d = %+v", i, w)
		}
	}
	res := e.post(a.ID, "/compact")
	if r := e.waitRun(res.RunID); r.Status != store.RunDone {
		t.Fatalf("compact run = %+v", r)
	}
	c, err := e.st.GetChat(context.Background(), chat)
	if err != nil || c == nil || !strings.Contains(c.Summary, "the gist of the early work") {
		t.Fatalf("chat after /compact = %+v err=%v", c, err)
	}
	if c.SummarizeUptoMsg == "" {
		t.Fatal("no marker recorded")
	}
	path := e.path(chat)
	last := path[len(path)-1]
	if last.Role != store.RoleAssistant || !strings.Contains(string(last.Content), "Context compressed") {
		t.Fatalf("hub did not answer /compact: %+v", last)
	}

	// A regular turn afterwards still runs…
	r := e.post(a.ID, "continue please")
	if w := e.waitRun(r.RunID); w.Status != store.RunDone {
		t.Fatalf("post-compact run = %+v", w)
	}
	// …and the stored summary makes the next built prompt start with it
	// (applyChatSummary is the sole consumer at turn build time).
	chatNow, _ := e.st.GetChat(context.Background(), chat)
	latest := e.path(chat)
	rebuilt := applyChatSummary(latest, chatNow)
	if rebuilt[0].Role != store.RoleUser || !strings.Contains(string(rebuilt[0].Content), "the gist of the early work") {
		t.Fatalf("rebuilt prompt does not start with the summary: %+v", rebuilt[0])
	}
	if len(rebuilt) >= len(latest) || len(rebuilt) >= len(path)-1 {
		t.Fatalf("summary dropped nothing: rebuilt %d vs latest %d (at-compact %d)", len(rebuilt), len(latest), len(path))
	}
}

// TestCreateChatInitialPreferences covers I1–I4/I12 end-to-end at the service
// boundary: creation persists the chosen model, limit, mode and effort.
func TestCreateChatInitialPreferences(t *testing.T) {
	e := newEnv(t)
	e.scripted("p", llm.Say("ok"))
	limit := int64(9000)
	in := CreateChatInput{
		AutoApprove:  true,
		Approval:     store.ApprovalDestructive,
		ContextLimit: limit,
		Effort:       "high",
		Model:        json.RawMessage(`{"provider":"p","model":"other"}`),
	}
	chat, err := e.m.CreateChat(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !chat.AutoApprove || chat.Approval != store.ApprovalDestructive || chat.ContextLimit != limit || chat.Effort != "high" {
		t.Fatalf("prefs not persisted: %+v", chat)
	}
	if got := string(chat.ModelPref); !strings.Contains(got, "other") {
		t.Fatalf("model_pref = %s", got)
	}
	c2, err := e.st.GetChat(context.Background(), chat.ID)
	if err != nil || c2.ContextLimit != limit || !c2.AutoApprove {
		t.Fatalf("reread = %+v err=%v", c2, err)
	}
}

// TestNoteAppendPersistsAndReprompts covers the capability pass: the agent's
// appended note is stored on the chat and reappears in the NEXT turn's system
// prompt (it outlives the context window).
func TestNoteAppendPersistsAndReprompts(t *testing.T) {
	e := newEnv(t)
	prov := e.scripted("p",
		llm.Say("r1"),
		llm.CallTool("switchboard_note_append", map[string]any{"entry": "the web tests run from hub/web"}),
		llm.Say("noted"),
		llm.Say("r3"))
	a := e.agent("w", withModel("p"))
	for _, text := range []string{"first", "remember this"} {
		if w := e.waitRun(e.post(a.ID, text).RunID); w.Status != store.RunDone {
			t.Fatalf("run(%s) = %+v", text, w)
		}
	}
	c, _ := e.st.GetChat(context.Background(), e.chatOf(a.ID))
	if !strings.Contains(c.Notes, "- the web tests run from hub/web") {
		t.Fatalf("notes = %q", c.Notes)
	}
	r := e.post(a.ID, "next please")
	if w := e.waitRun(r.RunID); w.Status != store.RunDone {
		t.Fatalf("run3 = %+v", w)
	}
	calls := prov.Calls()
	sys := calls[len(calls)-1].Options.System
	if !strings.Contains(sys, "## Notes") || !strings.Contains(sys, "hub/web") {
		t.Fatalf("note not re-injected; tail of system:\n%s", sys[max(0, len(sys)-400):])
	}
}

// TestHistorySearchAndCallStatsTools gives the model the two self-reflection
// views over stored state: the whole chat DAG and the shape of its calls.
func TestHistorySearchAndCallStatsTools(t *testing.T) {
	e := newEnv(t)
	e.scripted("p",
		llm.Say("the violet answer about zebras"),
		llm.CallTool("switchboard_chat_search", map[string]any{"query": "zebras"}),
		llm.Say("found it"),
		llm.CallTool("switchboard_calls_stats", map[string]any{}),
		llm.Say("stats noted"))
	a := e.agent("w", withModel("p"))
	for _, text := range []string{"tell me about zebras", "search the history", "and the stats"} {
		if w := e.waitRun(e.post(a.ID, text).RunID); w.Status != store.RunDone {
			t.Fatalf("run(%s) = %+v", text, w)
		}
	}
	var searchSummary, statsSummary string
	for _, r := range e.callRows(store.CallFilter{Source: store.SourceAgent}) {
		switch r.Tool {
		case "switchboard.chat.search":
			if strings.Contains(string(mustJSON(r.Result)), "zebra") {
				searchSummary = "ok"
			} else {
				t.Fatalf("search result envelope = %s", mustJSON(r.Result))
			}
		case "switchboard.calls.stats":
			statsSummary = string(mustJSON(r.Result))
		}
	}
	if searchSummary != "ok" {
		t.Fatal("chat.search never completed with hits")
	}
	if !strings.Contains(statsSummary, "per tool") || !strings.Contains(statsSummary, "switchboard.chat.search") {
		t.Fatalf("stats summary = %q", statsSummary)
	}
}

// TestOptimizeSkillsUnlockFlow covers the gated self-modification surface:
// locked by default, tools appear only after /optimize_skills, the brief
// reaches the transcript, a call actually edits live state, and "off" locks
// it again.
func TestOptimizeSkillsUnlockFlow(t *testing.T) {
	e := newEnv(t)
	def, err := e.m.ListProfiles(context.Background())
	if err != nil || len(def) == 0 {
		t.Fatalf("seeded profiles: %v", err)
	}
	prov := e.scripted("p",
		llm.Say("plain turn"),
		llm.CallTool("switchboard_optimize_chats_list", map[string]any{}),
		llm.CallTool("switchboard_prompt_set", map[string]any{"profile_id": def[0].ID, "system_prompt": "IMPROVED SYSTEM PROMPT"}),
		llm.CallTool("switchboard_skill_set", map[string]any{"name": "optimized-out", "description": "made by the optimizer", "body": "do the improved thing", "auto": false}),
		llm.Say("read done"),
		llm.Say("locked again"),
	)
	a := e.agent("w", withModel("p"))
	chat := e.chatOf(a.ID)

	off := func() bool {
		for _, tl := range toolNamesList(prov.Calls()[len(prov.Calls())-1].Tools) {
			if strings.HasPrefix(tl, "switchboard_optimize_") {
				return false
			}
		}
		return true
	}
	if w := e.waitRun(e.post(a.ID, "hello").RunID); w.Status != store.RunDone {
		t.Fatalf("base run = %+v", w)
	}
	if !off() {
		t.Fatal("optimize tools offered before the unlock")
	}

	// The command: hub-answered flag flip + the meta-prompt as its reply.
	if w := e.waitRun(e.post(a.ID, "/optimize_skills").RunID); w.Status != store.RunDone {
		t.Fatalf("unlock run = %+v", w)
	}
	c, _ := e.st.GetChat(context.Background(), chat)
	if !c.Optimize {
		t.Fatal("chat not unlocked")
	}
	path := e.path(chat)
	last := path[len(path)-1]
	if last.Role != store.RoleAssistant || !strings.Contains(string(last.Content), "Harness optimization unlocked") {
		t.Fatalf("unlock reply = %+v", last)
	}

	if w := e.waitRun(e.post(a.ID, "review your work").RunID); w.Status != store.RunDone {
		t.Fatalf("unlocked run = %+v", w)
	}
	if off() {
		t.Fatal("optimize tools missing after the unlock")
	}
	// Hub tool calls do not write call rows (upstream tools do); assert on
	// the persisted tool results and the state the calls changed.
	var sawChats bool
	for _, msg := range e.path(chat) {
		if len(msg.ToolResults) == 0 {
			continue
		}
		var res []storedResult
		_ = json.Unmarshal(msg.ToolResults, &res)
		for _, x := range res {
			if x.Error == "" && strings.Contains(string(mustJSON(x.Result)), chat) {
				sawChats = true
			}
			if x.Error != "" && (strings.Contains(x.Error, "optimize_skills") || strings.Contains(x.Error, "not permitted")) {
				t.Fatalf("gated tool refused while unlocked: %s", x.Error)
			}
		}
	}
	if !sawChats {
		for _, msg := range e.path(chat) {
			t.Logf("MSG role=%s toolres=%s", msg.Role, string(msg.ToolResults))
		}
		t.Fatal("chats_list did not return this chat")
	}
	if pr, err := e.st.GetProfile(context.Background(), def[0].ID); err != nil || pr == nil || pr.SystemPrompt != "IMPROVED SYSTEM PROMPT" {
		t.Fatalf("prompt_set did not land: %+v %v", pr, err)
	}
	skills, _ := e.m.ListSkills(context.Background())
	var made bool
	for _, sk := range skills {
		if sk.Name == "optimized-out" {
			made = true
		}
	}
	if !made {
		t.Fatal("skill_set did not create the skill")
	}

	if w := e.waitRun(e.post(a.ID, "/optimize_skills off").RunID); w.Status != store.RunDone {
		t.Fatalf("lock run = %+v", w)
	}
	if c, _ = e.st.GetChat(context.Background(), chat); c.Optimize {
		t.Fatal("chat still unlocked")
	}
	w := e.waitRun(e.post(a.ID, "back to normal").RunID)
	if w.Status != store.RunDone {
		t.Fatalf("locked run = %+v", w)
	}
	if !off() {
		t.Fatal("optimize tools still offered after locking")
	}
}

func toolNamesList(tools []llm.Tool) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name)
	}
	return out
}
