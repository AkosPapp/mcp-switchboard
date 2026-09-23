package agents

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/config"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/llm"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/registry"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

func TestRunHappyPathWithRealToolCall(t *testing.T) {
	e := newEnv(t)
	e.addServer("box", "", "demo", registry.ToolInfo{Name: "echo"})
	prov := e.scripted("p",
		llm.CallTool("box__demo__echo", map[string]any{"message": "hi"}).WithText("calling").WithUsage(100, 20),
		llm.Say("all done").WithUsage(150, 10))
	a := e.agent("worker", withModel("p"))
	res := e.post(a.ID, "please echo")
	run := e.waitRun(res.RunID)
	if run.Status != store.RunDone || run.FinishReason == nil || *run.FinishReason != "stop" {
		t.Fatalf("run = %+v", run)
	}

	path := e.path(e.chatOf(a.ID))
	if got := roles(path); got != "user,assistant,tool,assistant" {
		t.Fatalf("roles = %s", got)
	}
	if string(path[1].ToolCalls) == "" || !strings.Contains(string(path[1].ToolCalls), "box__demo__echo") {
		t.Errorf("tool calls not persisted: %s", path[1].ToolCalls)
	}
	// An export must be able to reproduce this turn byte for byte later, even
	// after the chat's profile/grants change - so the assistant message
	// carries its own snapshot of the model options, system prompt and tool
	// catalog it actually ran against, not just {provider, model}.
	if path[1].SystemPrompt == nil {
		t.Errorf("system prompt not snapshotted onto the assistant message")
	}
	if !strings.Contains(string(path[1].Tools), "box__demo__echo") {
		t.Errorf("tool catalog not snapshotted: %s", path[1].Tools)
	}
	var mc struct {
		Provider  string `json:"provider"`
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
	}
	if err := json.Unmarshal(path[1].Model, &mc); err != nil || mc.Provider != "p" || mc.MaxTokens == 0 {
		t.Errorf("model options not fully snapshotted: %s", path[1].Model)
	}
	if !strings.Contains(string(path[2].ToolResults), "echo ran") || !strings.Contains(string(path[2].ToolResults), `"call_id"`) {
		t.Errorf("tool result: %s", path[2].ToolResults)
	}
	// Second request saw the tool result, and the tool list was sent.
	calls := prov.Calls()
	if len(calls) != 2 || len(calls[0].Tools) != 1 || calls[0].Tools[0].Name != "box__demo__echo" {
		t.Fatalf("provider calls: %+v", calls)
	}
	last := calls[1].Messages[len(calls[1].Messages)-1]
	if last.Role != llm.RoleTool || len(last.ToolResults) != 1 || !strings.Contains(last.ToolResults[0].Content[0].Text, "echo ran") {
		t.Errorf("tool result not fed back: %+v", last)
	}

	// R1: one dispatch path with source agent and ids.
	rows := e.callRows(store.CallFilter{Source: store.SourceAgent})
	if len(rows) != 1 || rows[0].Status != store.StatusOK || *rows[0].AgentID != a.ID || *rows[0].RunID != res.RunID {
		t.Fatalf("call rows: %+v", rows)
	}
	// Cost: 1 micro per token; 120 + 160.
	if got := e.agentNow(a.ID).CostTotalMicros; got != 280 {
		t.Errorf("agent cost = %d", got)
	}
	if e.agentNow(a.ID).Status != store.AgentIdle {
		t.Errorf("status = %s", e.agentNow(a.ID).Status)
	}
	var usage map[string]any
	_ = json.Unmarshal(run.Usage, &usage)
	if usage["costMicros"].(float64) != 280 || usage["turns"].(float64) != 2 {
		t.Errorf("usage = %v", usage)
	}
	types := strings.Join(e.strm.types(res.RunID), ",")
	for _, want := range []string{"run_started", "delta", "tool_call", "tool_result", "message_done", "run_done"} {
		if !strings.Contains(types, want) {
			t.Errorf("missing frame %s in %s", want, types)
		}
	}
	if e.m.ActiveRuns() != 0 {
		t.Errorf("active runs = %d", e.m.ActiveRuns())
	}
}

func TestGrantDenialIsAToolResult(t *testing.T) {
	e := newEnv(t)
	e.addServer("box", "", "demo", registry.ToolInfo{Name: "echo"})
	// Pinned to client "box" but granted only its "srv" server, so the "demo"
	// server's tool is named as this agent sees it (no client prefix) and denied.
	e.scripted("p", llm.CallTool("demo__echo", nil), llm.Say("ok"))
	a := e.agent("worker", chain(withModel("p"), func(in *CreateAgentInput) {
		in.Grants = []store.Grant{{Label: "box", Project: "", Server: "srv", Allowed: true}}
	}))
	res := e.post(a.ID, "go")
	if run := e.waitRun(res.RunID); run.Status != store.RunDone {
		t.Fatalf("run = %+v", run)
	}
	rows := e.callRows(store.CallFilter{Status: store.StatusDenied})
	if len(rows) != 1 || rows[0].Source != store.SourceAgent || rows[0].Error != "not permitted: box/demo" {
		t.Fatalf("rows = %+v", rows)
	}
	path := e.path(e.chatOf(a.ID))
	if !strings.Contains(string(path[2].ToolResults), "not permitted: box/demo") {
		t.Errorf("model did not see the denial: %s", path[2].ToolResults)
	}
}

func TestCatalogIsGrantsIntersectLiveAndStable(t *testing.T) {
	e := newEnv(t)
	e.addServer("box", "", "demo", registry.ToolInfo{Name: "b"}, registry.ToolInfo{Name: "a"})
	e.addServer("nas", "proj", "files", registry.ToolInfo{Name: "read"})
	a := e.agent("w", chain(withModel("p"), func(in *CreateAgentInput) {
		in.Grants = []store.Grant{{Label: "*", Project: "*", Server: "*", Allowed: true}, {Label: "nas", Project: "*", Server: "files", Allowed: false}}
	}))
	cat, err := e.m.buildCatalog(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range cat.tools {
		names = append(names, tl.Name)
	}
	if strings.Join(names, ",") != "box__demo__a,box__demo__b" {
		t.Fatalf("catalog = %v", names)
	}
}

func TestSwitchboardToolVisibilityFollowsCapabilities(t *testing.T) {
	e := newEnv(t)
	names := func(a *store.Agent) []string {
		tools, err := e.m.AgentTools(context.Background(), a.ID)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, tl := range tools {
			out = append(out, tl.Name)
		}
		return out
	}
	if got := names(e.agent("leaf", caps(false, false))); len(got) != 0 {
		t.Errorf("leaf sees %v", got)
	}
	if got := strings.Join(names(e.agent("boss", caps(true, false))), ","); got !=
		"switchboard.chat.list,switchboard.chat.spawn,switchboard.chat.stop,switchboard.mcp.list_tools" {
		t.Errorf("spawner sees %s", got)
	}
	if got := strings.Join(names(e.agent("talker", caps(false, true))), ","); got !=
		"switchboard.chat.list,switchboard.chat.send,switchboard.mcp.list_tools" {
		t.Errorf("messenger sees %s", got)
	}
	// The catalog offered to the model uses provider-safe names.
	a := e.agent("both", caps(true, true))
	cat, _ := e.m.buildCatalog(context.Background(), a)
	for _, tl := range cat.llmTools() {
		if strings.Contains(tl.Name, ".") {
			t.Errorf("provider tool name %q has a dot", tl.Name)
		}
	}
}

func TestBudgetStopsARun(t *testing.T) {
	e := newEnv(t)
	e.addServer("box", "", "demo", registry.ToolInfo{Name: "echo"})
	e.provider(llm.NewScriptedFunc("p", func(int, []llm.Message) llm.Turn {
		return llm.CallTool("box__demo__echo", nil).WithUsage(10, 10)
	}))
	a := e.agent("looper", chain(withModel("p"), func(in *CreateAgentInput) { in.Budget = json.RawMessage(`{"max_turns":3}`) }))
	res := e.post(a.ID, "loop")
	run := e.waitRun(res.RunID)
	if run.Status != store.RunDone || *run.FinishReason != "budget" {
		t.Fatalf("run = %+v", run)
	}
	var usage map[string]any
	_ = json.Unmarshal(run.Usage, &usage)
	if usage["limit"] != "max_turns" || usage["turns"].(float64) != 3 {
		t.Errorf("usage = %v", usage)
	}
	var snap config.AgentBudget
	_ = json.Unmarshal(run.BudgetSnapshot, &snap)
	if snap.MaxTurns != 3 || snap.MaxCostMicros != 5_000_000 {
		t.Errorf("snapshot = %+v", snap)
	}
}

func TestLifetimeBudgetRefusesEnqueue(t *testing.T) {
	e := newEnv(t)
	e.scripted("p", llm.Say("a").WithUsage(60, 60))
	a := e.agent("spender", chain(withModel("p"), func(in *CreateAgentInput) { in.Budget = json.RawMessage(`{"max_lifetime_cost_micros":100}`) }))
	r1 := e.waitRun(e.post(a.ID, "one").RunID)
	if *r1.FinishReason != "stop" {
		t.Fatalf("r1 = %+v", r1)
	}
	r2 := e.waitRun(e.post(a.ID, "two").RunID)
	if r2.Status != store.RunDone || r2.FinishReason == nil || *r2.FinishReason != "budget" {
		t.Fatalf("second run should be refused (B5): %+v", r2)
	}
	if !strings.Contains(string(r2.Usage), "max_lifetime_cost_micros") {
		t.Errorf("usage = %s", r2.Usage)
	}
}

// stallProvider streams one delta and then blocks until cancelled.
type stallProvider struct{}

func (stallProvider) Name() string { return "stall" }
func (stallProvider) Complete(ctx context.Context, _ []llm.Message, _ []llm.Tool, _ llm.Options) (<-chan llm.Delta, error) {
	ch := make(chan llm.Delta, 1)
	ch <- llm.Delta{Type: llm.DeltaText, Text: "partial"}
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

func TestCancellationPersistsPartialMessage(t *testing.T) {
	e := newEnv(t)
	e.provider(stallProvider{})
	a := e.agent("slow", withModel("stall"))
	res := e.post(a.ID, "go")
	e.waitStatus(res.RunID, store.RunRunning)
	deadline := time.Now().Add(5 * time.Second)
	for e.m.ActiveRuns() == 0 || len(e.strm.types(res.RunID)) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("never streamed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := e.m.CancelRun(context.Background(), res.RunID); err != nil {
		t.Fatal(err)
	}
	run := e.waitRun(res.RunID)
	if run.Status != store.RunCancelled {
		t.Fatalf("run = %+v", run)
	}
	path := e.path(e.chatOf(a.ID))
	last := path[len(path)-1]
	if last.Role != "assistant" || *last.FinishReason != "cancelled" || !strings.Contains(string(last.Content), "partial") {
		t.Fatalf("partial message = %+v", last)
	}
	if e.agentNow(a.ID).Status != store.AgentIdle {
		t.Errorf("agent status = %s", e.agentNow(a.ID).Status)
	}
}

func TestQueuedRunOnBusyChatWaitsAndCancelsCleanly(t *testing.T) {
	e := newEnv(t)
	e.provider(stallProvider{})
	a := e.agent("slow", withModel("stall"))
	r1 := e.post(a.ID, "one")
	e.waitStatus(r1.RunID, store.RunRunning)
	r2 := e.post(a.ID, "two") // R4: queued behind the first, message deferred
	if r, _ := e.st.GetRun(context.Background(), r2.RunID); r.Status != store.RunQueued {
		t.Fatalf("second run = %+v", r)
	}
	if got := roles(e.path(e.chatOf(a.ID))); got != "user" {
		// the deferred message must not have landed mid-run
		t.Fatalf("path while busy = %s", got)
	}
	if err := e.m.CancelRun(context.Background(), r2.RunID); err != nil {
		t.Fatal(err)
	}
	if r := e.waitRun(r2.RunID); r.Status != store.RunCancelled {
		t.Fatalf("r2 = %+v", r)
	}
	_ = e.m.CancelRun(context.Background(), r1.RunID)
	e.waitRun(r1.RunID)
}

func destructiveTool() registry.ToolInfo {
	return registry.ToolInfo{Name: "danger", Annotations: &registry.ToolAnnotations{Destructive: bptr(true)}}
}

func approvalEnv(t *testing.T, timeoutSeconds int) (*env, *store.Agent, string) {
	e := newEnv(t, func(s *config.Settings) { s.ApprovalTimeout = timeoutSeconds })
	e.addServer("box", "", "harness", destructiveTool(), registry.ToolInfo{Name: "safe"})
	e.scripted("p", llm.CallTool("box__harness__danger", map[string]any{"x": 1}), llm.Say("finished"))
	a := e.agent("careful", chain(withModel("p"), func(in *CreateAgentInput) { in.Approval = store.ApprovalDestructive }))
	res := e.post(a.ID, "do it")
	e.waitStatus(res.RunID, store.RunWaiting)
	return e, a, res.RunID
}

func TestApprovalApprove(t *testing.T) {
	e, a, run := approvalEnv(t, 3600)
	if e.agentNow(a.ID).Status != store.AgentBlocked {
		// status is written right after the run row; allow a beat
		time.Sleep(50 * time.Millisecond)
	}
	if e.agentNow(a.ID).Status != store.AgentBlocked {
		t.Errorf("agent status = %s", e.agentNow(a.ID).Status)
	}
	pend := e.m.PendingApprovals(run)
	if len(pend) != 1 || pend[0].Tool != "box__harness__danger" || pend[0].Arguments["x"] != float64(1) && pend[0].Arguments["x"] != 1 {
		t.Fatalf("pending = %+v", pend)
	}
	e.m.mu.Lock()
	holding := e.m.running
	e.m.mu.Unlock()
	if holding != 0 {
		t.Errorf("a waiting run holds %d slots (R7)", holding)
	}
	if err := e.m.Approve(context.Background(), run, pend[0].CallID, true, ""); err != nil {
		t.Fatal(err)
	}
	if r := e.waitRun(run); r.Status != store.RunDone {
		t.Fatalf("run = %+v", r)
	}
	if rows := e.callRows(store.CallFilter{Source: store.SourceAgent}); len(rows) != 1 || rows[0].Status != store.StatusOK {
		t.Fatalf("rows = %+v", rows)
	}
	if err := e.m.Approve(context.Background(), run, "x", true, ""); !errors.Is(err, ErrNotPending) {
		t.Errorf("err = %v", err)
	}
}

func TestApprovalDeny(t *testing.T) {
	e, _, run := approvalEnv(t, 3600)
	pend := e.m.PendingApprovals(run)
	if err := e.m.Approve(context.Background(), run, pend[0].CallID, false, "too risky"); err != nil {
		t.Fatal(err)
	}
	if r := e.waitRun(run); r.Status != store.RunDone {
		t.Fatalf("run = %+v", r)
	}
	rows := e.callRows(store.CallFilter{Status: store.StatusDenied})
	if len(rows) != 1 || !strings.Contains(rows[0].Error, "too risky") {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestApprovalTimeoutAutoDenies(t *testing.T) {
	e, _, run := approvalEnv(t, 1)
	if r := e.waitRunFor(run, 8*time.Second); r.Status != store.RunDone {
		t.Fatalf("run = %+v", r)
	}
	rows := e.callRows(store.CallFilter{Status: store.StatusDenied})
	if len(rows) != 1 || !strings.Contains(rows[0].Error, "timed out") {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestApprovalDerivedAtCreation(t *testing.T) {
	e := newEnv(t)
	a, _ := e.m.CreateAgent(context.Background(), CreateAgentInput{Name: "root"}) // default grants (*,*,*) match harness
	if a.Approval != store.ApprovalDestructive {
		t.Errorf("approval = %s", a.Approval)
	}
	b, _ := e.m.CreateAgent(context.Background(), CreateAgentInput{Name: "narrow", Grants: []store.Grant{{Label: "*", Project: "*", Server: "fetch", Allowed: true}}})
	if b.Approval != store.ApprovalNever {
		t.Errorf("approval = %s", b.Approval)
	}
}

func TestCostRollsUpToAncestors(t *testing.T) {
	e := newEnv(t)
	e.scripted("p", llm.Say("hi").WithUsage(40, 60))
	parent := e.agent("parent", withModel("p"))
	child, err := e.m.CreateAgent(context.Background(), CreateAgentInput{ParentID: parent.ID, Name: "kid", Approval: "never", Model: parent.Model})
	if err != nil {
		t.Fatal(err)
	}
	e.waitRun(e.post(child.ID, "work").RunID)
	if got := e.agentNow(parent.ID).CostTotalMicros; got != 100 {
		t.Errorf("parent cost = %d", got)
	}
	if got := e.agentNow(child.ID).CostTotalMicros; got != 100 {
		t.Errorf("child cost = %d", got)
	}
}

func TestSpawnIntersectsGrantsAndEnforcesLimits(t *testing.T) {
	e := newEnv(t, func(s *config.Settings) { s.AgentMaxChildren = 1 })
	e.addServer("box", "", "demo", registry.ToolInfo{Name: "echo"})
	e.scripted("p",
		llm.CallTool("switchboard_chat_spawn", map[string]any{
			"title": "kid", "system_prompt": "be a kid",
			"grants": []any{map[string]any{"label": "*", "project": "*", "server": "*"}},
		}),
		llm.CallTool("switchboard_chat_spawn", map[string]any{"title": "kid2", "system_prompt": "x"}),
		llm.Say("spawned"))
	parent := e.agent("parent", chain(withModel("p"), caps(true, false), func(in *CreateAgentInput) {
		in.Grants = []store.Grant{{Label: "box", Project: "*", Server: "demo", Allowed: true}}
	}))
	res := e.post(parent.ID, "spawn")
	if r := e.waitRun(res.RunID); r.Status != store.RunDone {
		t.Fatalf("run = %+v", r)
	}
	kids, _ := e.st.ListAgents(context.Background(), store.AgentFilter{ParentID: parent.ID})
	if len(kids) != 1 || !strings.HasPrefix(kids[0].Name, "kid · ") || kids[0].Depth != 1 {
		t.Fatalf("kids = %+v", kids)
	}
	gs, _ := e.st.ListGrants(context.Background(), kids[0].ID)
	if len(gs) != 1 || gs[0].Label != "box" || gs[0].Server != "demo" || gs[0].Source != store.GrantInherited || !gs[0].Allowed {
		t.Fatalf("A12: child grants = %+v", gs)
	}
	if kids[0].Approval != store.ApprovalNever {
		t.Errorf("child approval = %s", kids[0].Approval)
	}
	// The spawn was logged as a call, and the second one hit AGENT_MAX_CHILDREN.
	rows := e.callRows(store.CallFilter{Server: "orchestrator"})
	if len(rows) != 2 {
		t.Fatalf("switchboard rows = %+v", rows)
	}
	var sawLimit bool
	for _, r := range rows {
		if r.Status == store.StatusError && strings.Contains(r.Error, "AGENT_MAX_CHILDREN") {
			sawLimit = true
		}
	}
	if !sawLimit {
		t.Errorf("no children-limit error in %+v", rows)
	}
	// Depth limit.
	e2 := newEnv(t, func(s *config.Settings) { s.AgentMaxDepth = 1 })
	root := e2.agent("r", nil)
	c1, _ := e2.m.CreateAgent(context.Background(), CreateAgentInput{ParentID: root.ID, Name: "c1", Approval: "never"})
	if _, err := e2.m.CreateAgent(context.Background(), CreateAgentInput{ParentID: c1.ID, Name: "c2"}); !errors.Is(err, ErrLimit) {
		t.Errorf("depth err = %v", err)
	}
}

func TestIdempotentPost(t *testing.T) {
	e := newEnv(t)
	e.scripted("p", llm.Say("once"))
	a := e.agent("a", withModel("p"))
	chat := e.chatOf(a.ID)
	r1, err := e.m.Post(context.Background(), chat, PostInput{Content: say("hi"), IdempotencyKey: "k1"})
	if err != nil {
		t.Fatal(err)
	}
	e.waitRun(r1.RunID)
	r2, err := e.m.Post(context.Background(), chat, PostInput{Content: say("hi"), IdempotencyKey: "k1"})
	if err != nil {
		t.Fatal(err)
	}
	if !r2.Deduplicated || r2.RunID != r1.RunID || r2.MessageID != r1.MessageID {
		t.Fatalf("r1=%+v r2=%+v", r1, r2)
	}
	runs, _ := e.st.ListRuns(context.Background(), store.RunFilter{AgentID: a.ID})
	if len(runs) != 1 {
		t.Errorf("runs = %d", len(runs))
	}
}

func TestBranchRegenerate(t *testing.T) {
	e := newEnv(t)
	e.scripted("p", llm.Say("first"), llm.Say("second"))
	a := e.agent("a", withModel("p"))
	r := e.post(a.ID, "q")
	e.waitRun(r.RunID)
	chat := e.chatOf(a.ID)
	asst := e.path(chat)[1]
	res, err := e.m.Branch(context.Background(), chat, BranchInput{FromMessageID: asst.ID})
	if err != nil {
		t.Fatal(err)
	}
	e.waitRun(res.RunID)
	p := e.path(chat)
	if len(p) != 2 || p[1].ID == asst.ID || !strings.Contains(string(p[1].Content), "second") {
		t.Fatalf("path = %+v", p)
	}
	sib, _ := e.st.Siblings(context.Background(), p[1].ID)
	if len(sib.IDs) != 2 {
		t.Errorf("siblings = %+v", sib)
	}
}

func TestProviderErrorFinishesWithError(t *testing.T) {
	e := newEnv(t)
	e.scripted("p", llm.Fail(errors.New("upstream 500")))
	a := e.agent("a", withModel("p"))
	run := e.waitRun(e.post(a.ID, "x").RunID)
	if run.Status != store.RunError || run.Error == nil || !strings.Contains(*run.Error, "upstream 500") {
		t.Fatalf("run = %+v", run)
	}
	if e.agentNow(a.ID).Status != store.AgentError {
		t.Errorf("agent status = %s", e.agentNow(a.ID).Status)
	}
}

func TestStartupInterruptsStaleRuns(t *testing.T) {
	e := newEnv(t)
	a := e.agent("a", nil)
	c := e.chatOf(a.ID)
	run, _ := e.st.CreateRun(context.Background(), store.Run{AgentID: a.ID, ChatID: c, Trigger: "human", Status: store.RunRunning})
	if err := e.m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	r, _ := e.st.GetRun(context.Background(), run.ID)
	if r.Status != store.RunInterrupted {
		t.Errorf("run = %+v", r)
	}
}

func TestShutdownInterruptsRuns(t *testing.T) {
	e := newEnv(t)
	e.provider(stallProvider{})
	a := e.agent("slow", withModel("stall"))
	res := e.post(a.ID, "go")
	e.waitStatus(res.RunID, store.RunRunning)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.m.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	r, _ := e.st.GetRun(context.Background(), res.RunID)
	if r.Status != store.RunInterrupted {
		t.Errorf("run = %+v", r)
	}
}

func TestDeleteAgentCancelsRuns(t *testing.T) {
	e := newEnv(t)
	e.provider(stallProvider{})
	a := e.agent("slow", withModel("stall"))
	res := e.post(a.ID, "go")
	e.waitStatus(res.RunID, store.RunRunning)
	if err := e.m.DeleteAgent(context.Background(), a.ID, false); err != nil {
		t.Fatal(err)
	}
	r, _ := e.st.GetRun(context.Background(), res.RunID)
	if r.Status != store.RunCancelled {
		t.Errorf("run = %+v", r)
	}
}

func TestSetGrantHumanAndGraph(t *testing.T) {
	e := newEnv(t)
	e.addServer("box", "", "demo", registry.ToolInfo{Name: "echo"})
	p := e.agent("p", func(in *CreateAgentInput) {
		in.Grants = []store.Grant{{Label: "box", Project: "", Server: "demo", Allowed: true}}
	})
	kid, _ := e.m.CreateAgent(context.Background(), CreateAgentInput{ParentID: p.ID, Name: "k"})
	if _, err := e.m.SetGrant(context.Background(), store.Grant{AgentID: kid.ID, Label: "nas", Project: "", Server: "files", Allowed: true}); err != nil {
		t.Fatal(err)
	}
	g, err := e.m.Graph(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var orphan, conn bool
	for _, gg := range g.Grants {
		if gg.AgentID == kid.ID && gg.Server == "files" {
			orphan = gg.Orphaned && gg.Source == store.GrantHuman
		}
		if gg.AgentID == p.ID && gg.Server == "demo" {
			conn = gg.Connected
		}
	}
	if !orphan || !conn || len(g.Servers) != 1 || len(g.Agents) != 2 || len(g.Edges) != 1 {
		t.Errorf("graph = %+v", g)
	}
}

// A chat's agent is pinned to one client, so it sees that client's tools
// without the client prefix, and the harness's without the server prefix
// either; every other server keeps its prefix.
func TestPinnedAgentSeesShortToolNamesAndCanCallThem(t *testing.T) {
	e := newEnv(t)
	e.addServer("box", "", "harness", registry.ToolInfo{Name: "run_command"})
	e.addServer("box", "", "fetch", registry.ToolInfo{Name: "fetch"})
	prov := e.scripted("p", llm.CallTool("run_command", nil), llm.CallTool("fetch__fetch", nil), llm.Say("done"))
	a := e.agent("worker", chain(withModel("p"), func(in *CreateAgentInput) {
		in.Grants = []store.Grant{{Label: "box", Project: "*", Server: "*", Allowed: true}}
	}))
	res := e.post(a.ID, "go")
	if run := e.waitRun(res.RunID); run.Status != store.RunDone {
		t.Fatalf("run = %+v", run)
	}

	var names []string
	for _, tl := range prov.Calls()[0].Tools {
		names = append(names, tl.Name)
	}
	if got := strings.Join(names, ","); got != "fetch__fetch,run_command" {
		t.Errorf("tool names offered = %s", got)
	}
	rows := e.callRows(store.CallFilter{Source: store.SourceAgent})
	if len(rows) != 2 {
		t.Fatalf("call rows: %+v", rows)
	}
	for _, r := range rows {
		if r.Status != store.StatusOK || r.Label != "box" {
			t.Errorf("call did not reach the pinned client: %+v", r)
		}
	}
}

func TestPostAndBranchClearDraft(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.scripted("p", llm.Say("one"), llm.Say("two"))
	a := e.agent("a", withModel("p"))
	chat := e.chatOf(a.ID)

	if _, err := e.st.SetDraft(ctx, chat, "half-typed"); err != nil {
		t.Fatal(err)
	}
	r, err := e.m.Post(ctx, chat, PostInput{Content: say("hi")})
	if err != nil {
		t.Fatal(err)
	}
	e.waitRun(r.RunID)
	if d, _ := e.st.GetDraft(ctx, chat); d.UpdatedAt != nil {
		t.Fatalf("draft survived Post: %+v", d)
	}

	if _, err := e.st.SetDraft(ctx, chat, "edit in progress"); err != nil {
		t.Fatal(err)
	}
	br, err := e.m.Branch(ctx, chat, BranchInput{FromMessageID: r.MessageID, Content: say("hi again")})
	if err != nil {
		t.Fatal(err)
	}
	e.waitRun(br.RunID)
	if d, _ := e.st.GetDraft(ctx, chat); d.UpdatedAt != nil {
		t.Fatalf("draft survived Branch: %+v", d)
	}
}

// A provider that reports the prompt filled its context window must reach the
// run's usage, where the console reads it.
func TestRunUsageCarriesContextTruncation(t *testing.T) {
	e := newEnv(t)
	turn := llm.Say("ok")
	turn.Usage = &llm.Usage{InputTokens: 4096, OutputTokens: 3, ContextWindow: 4096, Truncated: true}
	e.scripted("p", turn)
	a := e.agent("worker", withModel("p"))
	res := e.post(a.ID, "go")
	run := e.waitRun(res.RunID)
	var usage map[string]any
	_ = json.Unmarshal(run.Usage, &usage)
	if usage["truncated"] != true || usage["contextWindow"] != float64(4096) {
		t.Errorf("usage = %v", usage)
	}
}

func TestAllPendingApprovals(t *testing.T) {
	e, a, run := approvalEnv(t, 3600)
	list, err := e.m.AllPendingApprovals(context.Background())
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %+v err=%v", list, err)
	}
	p := list[0]
	chat, _ := e.m.st.GetChat(context.Background(), p.ChatID)
	if chat == nil || p.ChatTitle != chat.Title {
		t.Fatalf("chat title = %+v", p)
	}
	if p.RunID != run || p.AgentID != a.ID || p.AgentName != "careful" || p.ChatID == "" ||
		p.Tool != "box__harness__danger" || p.Arguments["x"] == nil || p.ExpiresAt == "" {
		t.Fatalf("detail = %+v", p)
	}
	if err := e.m.Approve(context.Background(), run, p.CallID, true, ""); err != nil {
		t.Fatal(err)
	}
	e.waitRun(run)
	if list, _ := e.m.AllPendingApprovals(context.Background()); len(list) != 0 {
		t.Fatalf("after approve: %+v", list)
	}
}

func TestAllPendingApprovalsTimeout(t *testing.T) {
	e, _, run := approvalEnv(t, 1)
	if list, _ := e.m.AllPendingApprovals(context.Background()); len(list) != 1 {
		t.Fatalf("pending: %+v", list)
	}
	e.waitRunFor(run, 8*time.Second)
	list, err := e.m.AllPendingApprovals(context.Background())
	if err != nil || list == nil || len(list) != 0 {
		t.Fatalf("after timeout: %#v %v", list, err)
	}
}
