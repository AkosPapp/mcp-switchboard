package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/config"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/events"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/llm"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

// runState is the in-memory half of a run.
type runState struct {
	id, agentID, chatID string
	trigger             string
	triggeredBy         *string
	project             string
	ctx                 context.Context
	cancel              context.CancelCauseFunc
	done                chan struct{}
	budget              config.AgentBudget
	sub                 submission
	reply               *replyRoute
	enqueuedAt          time.Time
	createdAt           time.Time
	wallTimer           *time.Timer

	// Owned by the run's own goroutine (before it starts: the submitter).
	materialized bool

	// leaf is the message the next append goes under. leafMu serialises the
	// concurrent tool-result appends of one turn.
	leafMu sync.Mutex
	leaf   *string

	// Guarded by Manager.mu.
	started         bool
	finished        bool
	holdsSlot       bool
	acquiring       bool
	waiters         int
	approvalWaiters int
	currentTool     string
	approvals       map[string]*pendingApproval

	// usageMu guards the running totals.
	usageMu    sync.Mutex
	turns      int
	toolCalls  int
	usage      llm.Usage
	costMicros int64
	lastText   string
	budgetHit  string
}

type runOutcome struct {
	status       string
	finishReason string
	err          string
	limit        string
}

func (rs *runState) addUsage(u llm.Usage, cost int64) {
	rs.usageMu.Lock()
	rs.usage = rs.usage.Add(u)
	rs.costMicros += cost
	rs.turns++
	rs.usageMu.Unlock()
}

func (rs *runState) tokens() int64 {
	u := rs.usage
	return int64(u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheWriteTokens)
}

func (rs *runState) usageJSON(limit string, elapsed time.Duration) json.RawMessage {
	rs.usageMu.Lock()
	defer rs.usageMu.Unlock()
	m := map[string]any{
		"turns": rs.turns, "toolCalls": rs.toolCalls,
		"inputTokens": rs.usage.InputTokens, "outputTokens": rs.usage.OutputTokens,
		"cacheReadTokens": rs.usage.CacheReadTokens, "cacheWriteTokens": rs.usage.CacheWriteTokens,
		"tokens": rs.tokens(), "costMicros": rs.costMicros, "wallSeconds": elapsed.Seconds(),
	}
	if rs.usage.ContextWindow > 0 {
		m["contextWindow"] = rs.usage.ContextWindow
	}
	// The provider dropped the start of a prompt that filled the window: the
	// console warns about it rather than letting the model just seem forgetful.
	if rs.usage.Truncated {
		m["truncated"] = true
	}
	if limit != "" {
		m["limit"] = limit
	}
	return mustJSON(m)
}

// causeOutcome maps how a run's context ended to its outcome.
func causeOutcome(rs *runState) runOutcome {
	cause := context.Cause(rs.ctx)
	switch {
	case errors.Is(cause, errWall):
		return runOutcome{status: store.RunDone, finishReason: "budget", limit: "max_wall_seconds"}
	case errors.Is(cause, errShutdown):
		return runOutcome{status: store.RunInterrupted, finishReason: "interrupted", err: "the hub shut down while this run was in progress"}
	}
	return runOutcome{status: store.RunCancelled, finishReason: "cancelled"}
}

// execute is the run loop of spec.md 5.3.
func (m *Manager) execute(rs *runState) runOutcome {
	ctx := rs.ctx
	if err := m.materialize(ctx, rs); err != nil {
		return runOutcome{status: store.RunError, finishReason: "error", err: err.Error()}
	}
	if err := m.acquire(ctx, rs, false); err != nil {
		return causeOutcome(rs)
	}
	m.mu.Lock()
	rs.holdsSlot = true
	rs.started = true
	m.mu.Unlock()
	if rs.budget.MaxWallSeconds > 0 {
		rs.wallTimer = time.AfterFunc(time.Duration(rs.budget.MaxWallSeconds)*time.Second-time.Since(rs.createdAt), func() { rs.cancel(errWall) })
	}

	started := time.Now().UTC()
	running := store.RunRunning
	sctx, scancel := detached()
	_, _ = m.st.UpdateRun(sctx, rs.id, store.RunPatch{Status: &running, StartedAt: &started})
	scancel()
	m.refreshAgentStatus(rs.agentID, "")
	if rs.reply != nil || rs.trigger == store.TriggerAgentMessage {
		if !rs.enqueuedAt.IsZero() && m.met != nil {
			m.met.ObserveDelivery(rs.project, time.Since(rs.enqueuedAt).Seconds())
		}
	}
	m.frame(rs, "run_started", map[string]any{"runId": rs.id, "chatId": rs.chatID, "agentId": rs.agentID, "trigger": rs.trigger})
	m.emit("run_started", map[string]any{"agentId": rs.agentID, "runId": rs.id, "chatId": rs.chatID, "trigger": rs.trigger})
	m.publish(events.Event{Type: events.TypeChat, ChatID: rs.chatID, AgentID: rs.agentID})

	for turn := 0; ; turn++ {
		if ctx.Err() != nil {
			return causeOutcome(rs)
		}
		agent, err := m.st.GetAgent(ctx, rs.agentID)
		if err != nil {
			return runOutcome{status: store.RunError, finishReason: "error", err: err.Error()}
		}
		if agent == nil || agent.DeletedAt != nil {
			return runOutcome{status: store.RunCancelled, finishReason: "cancelled", err: "the agent was deleted"}
		}
		agent, _ = m.resolveAgent(ctx, agent) // live profile reference, every turn
		if lim := m.budgetLimit(ctx, rs, agent); lim != "" {
			return runOutcome{status: store.RunDone, finishReason: "budget", limit: lim}
		}

		res, err := m.turn(rs, agent)
		if err != nil {
			return runOutcome{status: store.RunError, finishReason: "error", err: describeErr(err)}
		}
		switch res.kind {
		case turnCancelled:
			return causeOutcome(rs)
		case turnFailed:
			return runOutcome{status: store.RunError, finishReason: "error", err: res.errText}
		case turnDone:
			fr := res.finish
			if fr == "" {
				fr = llm.FinishStop
			}
			return runOutcome{status: store.RunDone, finishReason: fr}
		}

		m.execTools(rs, agent, res)
		if rs.budgetHit != "" {
			return runOutcome{status: store.RunDone, finishReason: "budget", limit: rs.budgetHit}
		}
	}
}

// budgetLimit returns the name of an exhausted per-run or lifetime limit.
func (m *Manager) budgetLimit(ctx context.Context, rs *runState, agent *store.Agent) string {
	b := rs.budget
	rs.usageMu.Lock()
	turns, calls, tokens, cost := rs.turns, rs.toolCalls, rs.tokens(), rs.costMicros
	rs.usageMu.Unlock()
	switch {
	case b.MaxTurns > 0 && turns >= b.MaxTurns:
		return "max_turns"
	case b.MaxToolCalls > 0 && calls >= b.MaxToolCalls:
		return "max_tool_calls"
	case b.MaxTokens > 0 && tokens >= b.MaxTokens:
		return "max_tokens"
	case b.MaxCostMicros > 0 && cost >= b.MaxCostMicros:
		return "max_cost_micros"
	}
	// B5: also between turns, not only when a run is enqueued, so a run that
	// is itself what spends the last of the lifetime budget stops now.
	if _, exceeded, err := m.st.LifetimeExceeded(ctx, agent.ID, b.MaxLifetimeCostMicros); err == nil && exceeded {
		return "max_lifetime_cost_micros"
	}
	return ""
}

type turnKind int

const (
	turnTools turnKind = iota
	turnDone
	turnCancelled
	turnFailed
)

type turnResult struct {
	kind    turnKind
	finish  string
	errText string
	msg     store.Message
	calls   []llm.ToolCall // real (unmapped) names
	cat     *catalog
}

// turn is one model round-trip: catalog, stream, persist.
func (m *Manager) turn(rs *runState, agent *store.Agent) (turnResult, error) {
	ctx := rs.ctx
	plan, err := m.planTurn(ctx, agent, nil) // agent is already effective (resolveAgent)
	if err != nil {
		return turnResult{}, err
	}
	cat := plan.cat
	path, err := m.st.ActivePath(ctx, rs.chatID)
	if err != nil {
		return turnResult{}, err
	}
	msgs := toLLMMessages(path)

	mc, err := parseModel(agent.Model)
	if err != nil {
		return turnResult{kind: turnFailed, errText: err.Error()}, nil
	}
	if len(rs.sub.modelOverride) > 0 {
		if o, err := parseModel(rs.sub.modelOverride); err == nil {
			if o.Provider != "" {
				mc.Provider = o.Provider
			}
			if o.Model != "" {
				mc.Model = o.Model
			}
			if o.Temperature != nil {
				mc.Temperature = o.Temperature
			}
			if o.MaxTokens > 0 {
				mc.MaxTokens = o.MaxTokens
			}
			if o.Thinking != nil {
				mc.Thinking = o.Thinking
			}
		}
	}
	prov, ok := m.llm.Provider(mc.Provider)
	if !ok {
		return turnResult{kind: turnFailed, errText: fmt.Sprintf("no model provider %q is configured", mc.Provider)}, nil
	}
	spec, _, _ := m.llm.Lookup(mc.Provider, mc.Model)

	toolDefs := cat.llmTools()
	turnOpts := mc.options(plan.system)
	began := time.Now()
	ch, err := prov.Complete(ctx, msgs, toolDefs, turnOpts)
	if err != nil {
		if ctx.Err() != nil {
			return turnResult{kind: turnCancelled}, nil
		}
		if m.met != nil {
			m.met.CountLLMError(mc.Provider, mc.Model, "request")
		}
		return turnResult{kind: turnFailed, errText: describeErr(err)}, nil
	}

	msgID := store.NewID()
	var (
		blocks   []llm.Block
		calls    []llm.ToolCall
		usage    llm.Usage
		finish   string
		streamEr error
	)
	appendText := func(typ string, d llm.Delta) {
		// Keyed on ContentIndex, not just type: two distinct blocks of the
		// same type back to back (e.g. two separate thinking blocks around a
		// tool call) must not merge into one just because appendText last
		// saw the same typ.
		if n := len(blocks); n > 0 && blocks[n-1].Type == typ && blocks[n-1].Index == d.ContentIndex {
			blocks[n-1].Text += d.Text
		} else {
			blocks = append(blocks, llm.Block{Type: typ, Text: d.Text, Index: d.ContentIndex})
		}
		if d.Signature != "" && typ == llm.BlockThinking {
			blocks[len(blocks)-1].Signature = d.Signature
		}
	}
	for d := range ch {
		switch d.Type {
		case llm.DeltaText:
			appendText(llm.BlockText, d)
			m.frame(rs, "delta", map[string]any{"messageId": msgID, "contentIndex": d.ContentIndex, "text": d.Text})
		case llm.DeltaThinking:
			appendText(llm.BlockThinking, d)
			m.frame(rs, "delta", map[string]any{"messageId": msgID, "contentIndex": d.ContentIndex, "text": d.Text, "kind": "thinking"})
		case llm.DeltaToolCall:
			if d.ToolCall == nil {
				continue
			}
			tc := *d.ToolCall
			if real, ok := cat.byProv[tc.Name]; ok {
				tc.Name = real.Name
			}
			if tc.Arguments == nil {
				tc.Arguments = map[string]any{}
			}
			tc.Index = d.ContentIndex
			calls = append(calls, tc)
			m.frame(rs, "tool_call", map[string]any{"messageId": msgID, "callId": tc.ID, "name": tc.Name, "arguments": tc.Arguments})
		case llm.DeltaUsage:
			if d.Usage != nil {
				usage = usage.Add(*d.Usage)
			}
		case llm.DeltaDone:
			finish = d.FinishReason
		case llm.DeltaError:
			streamEr = d.Err
			if streamEr == nil {
				streamEr = errors.New("the model stream failed")
			}
		}
	}
	latency := time.Since(began)

	cost, _ := spec.Cost(usage) // unknown price => 0 (L5)
	inTok := usage.InputTokens + usage.CacheReadTokens + usage.CacheWriteTokens
	cancelled := ctx.Err() != nil
	res := turnResult{cat: cat, finish: finish}
	switch {
	case cancelled:
		res.kind = turnCancelled
		finish, calls = "cancelled", nil // a dangling tool_use would poison the transcript
		if errors.Is(context.Cause(ctx), errWall) {
			finish = "budget"
		}
	case streamEr != nil:
		res.kind = turnFailed
		res.errText = describeErr(streamEr)
		finish, calls = llm.FinishError, nil
		if m.met != nil {
			m.met.CountLLMError(mc.Provider, mc.Model, "stream")
		}
	case finish == llm.FinishToolUse && len(calls) > 0:
		res.kind = turnTools
	default:
		res.kind = turnDone
		if finish == "" {
			finish = llm.FinishStop
		}
		calls = nil
	}
	res.finish = finish

	if m.met != nil {
		m.met.ObserveTurn(rs.project, mc.Provider, mc.Model, usage.InputTokens, usage.OutputTokens,
			usage.CacheReadTokens, usage.CacheWriteTokens, cost, latency.Seconds())
	}
	rs.addUsage(usage, cost)

	// Nothing to persist when nothing was produced (e.g. cancelled before the
	// first token): an empty assistant message would break the next request.
	if len(blocks) == 0 && len(calls) == 0 {
		if res.kind == turnDone {
			blocks = textBlocks("")
		} else {
			return res, nil
		}
	}
	fr := finish
	systemPrompt := plan.system
	msg := store.Message{
		ID: msgID, ChatID: rs.chatID, Role: store.RoleAssistant, Content: mustJSON(blocks),
		TokenInput: int64(inTok), TokenOutput: int64(usage.OutputTokens), CostMicros: cost,
		// Model, SystemPrompt and Tools together are the exact turn a JSON
		// export needs to reproduce this turn byte for byte later, even if
		// the chat's profile/grants have since changed (spec.md 7.2 export).
		LatencyMs: latency.Milliseconds(), Model: mustJSON(map[string]any{
			"provider": mc.Provider, "model": mc.Model, "max_tokens": turnOpts.MaxTokens,
			"temperature": turnOpts.Temperature, "thinking": turnOpts.Thinking,
		}),
		SystemPrompt: &systemPrompt, Tools: mustJSON(toolDefs),
		FinishReason: &fr, RunID: &rs.id,
	}
	if len(calls) > 0 {
		msg.ToolCalls = mustJSON(calls)
	}
	rs.leafMu.Lock()
	msg.ParentID = rs.leaf
	pctx, pcancel := detached()
	saved, err := m.st.AppendMessageWithUsage(pctx, msg)
	pcancel()
	if err == nil {
		rs.leaf = &saved.ID
	}
	rs.leafMu.Unlock()
	if err != nil {
		return res, fmt.Errorf("persisting the assistant message: %w", err)
	}
	if t := blocksText(blocks); t != "" {
		rs.usageMu.Lock()
		rs.lastText = t
		rs.usageMu.Unlock()
	}
	res.msg, res.calls = saved, calls
	m.frame(rs, "message_done", saved)
	m.publish(events.Event{Type: events.TypeChat, ChatID: rs.chatID, AgentID: rs.agentID})
	return res, nil
}

// execTools runs a turn's tool calls concurrently (bounded) and persists one
// tool message per result, each the moment it lands (R6).
func (m *Manager) execTools(rs *runState, agent *store.Agent, res turnResult) {
	sem := make(chan struct{}, m.parallel)
	var wg sync.WaitGroup
	for _, tc := range res.calls {
		wg.Add(1)
		go func(tc llm.ToolCall) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			var out toolOutcome
			switch {
			case rs.ctx.Err() != nil:
				out = toolErr("cancelled: the run was stopped before this call started")
			case !rs.reserveToolCall():
				out = toolErr("budget exhausted: max_tool_calls reached; this call was not made")
			default:
				m.setCurrentTool(rs, tc.Name)
				out = m.execTool(rs.ctx, agent, rs, tc.Name, tc.Arguments, tc.ID)
				m.setCurrentTool(rs, "")
			}
			m.persistToolResult(rs, tc, out)
		}(tc)
	}
	wg.Wait()
}

func (rs *runState) reserveToolCall() bool {
	rs.usageMu.Lock()
	defer rs.usageMu.Unlock()
	if rs.budget.MaxToolCalls > 0 && rs.toolCalls >= rs.budget.MaxToolCalls {
		rs.budgetHit = "max_tool_calls"
		return false
	}
	rs.toolCalls++
	return true
}

func (m *Manager) setCurrentTool(rs *runState, name string) {
	m.mu.Lock()
	rs.currentTool = name
	m.mu.Unlock()
	m.publish(events.Event{Type: events.TypeAgent, AgentID: rs.agentID})
}

// storedResult is one element of messages.tool_results (5.2).
type storedResult struct {
	ToolCallID string         `json:"tool_call_id"`
	CallID     string         `json:"call_id,omitempty"`
	Result     map[string]any `json:"result,omitempty"`
	Error      string         `json:"error,omitempty"`
}

func (m *Manager) persistToolResult(rs *runState, tc llm.ToolCall, out toolOutcome) {
	sr := storedResult{ToolCallID: tc.ID, CallID: out.CallID, Result: out.Result}
	if out.IsError {
		sr.Error = out.Text()
		if sr.Error == "" {
			sr.Error = "the tool reported an error"
		}
	}
	if sr.Result == nil {
		sr.Result = map[string]any{"content": []any{map[string]any{"type": "text", "text": out.Text()}}}
	}
	msg := store.Message{
		ChatID: rs.chatID, Role: store.RoleTool, Content: mustJSON(out.Blocks),
		ToolResults: mustJSON([]storedResult{sr}), RunID: &rs.id,
	}
	rs.leafMu.Lock()
	msg.ParentID = rs.leaf
	pctx, pcancel := detached()
	saved, err := m.st.AppendMessage(pctx, msg)
	pcancel()
	if err == nil {
		rs.leaf = &saved.ID
	}
	rs.leafMu.Unlock()
	if err != nil {
		m.log.Warn("could not persist a tool result", "run", rs.id, "error", err)
		return
	}
	m.frame(rs, "tool_result", map[string]any{"callId": tc.ID, "name": tc.Name, "isError": out.IsError, "message": saved})
	m.publish(events.Event{Type: events.TypeChat, ChatID: rs.chatID, AgentID: rs.agentID})
}

// ----------------------------------------------------------- message conversion

// toLLMMessages turns the active path into provider-neutral messages, mapping
// tool names to provider-safe ones and repairing tool_use/tool_result pairing
// (a crash or cancel can leave a call without a result).
func toLLMMessages(path []store.Message) []llm.Message {
	var out []llm.Message
	for _, sm := range path {
		var blocks []llm.Block
		_ = json.Unmarshal(sm.Content, &blocks)
		switch sm.Role {
		case store.RoleAssistant:
			am := llm.Message{Role: llm.RoleAssistant, Content: blocks}
			var calls []llm.ToolCall
			if len(sm.ToolCalls) > 0 && string(sm.ToolCalls) != "null" {
				_ = json.Unmarshal(sm.ToolCalls, &calls)
			}
			for i := range calls {
				calls[i].Name = providerName(calls[i].Name)
			}
			am.ToolCalls = calls
			out = append(out, am)
		case store.RoleTool:
			var rs []storedResult
			_ = json.Unmarshal(sm.ToolResults, &rs)
			tm := llm.Message{Role: llm.RoleTool}
			for _, r := range rs {
				content := blocksFromResult(r.Result)
				if len(content) == 0 && r.Error != "" {
					content = textBlocks(r.Error)
				}
				tm.ToolResults = append(tm.ToolResults, llm.ToolResult{ToolCallID: r.ToolCallID, Content: content, IsError: r.Error != ""})
			}
			out = append(out, tm)
		case store.RoleSystem:
			out = append(out, llm.Message{Role: llm.RoleSystem, Content: blocks})
		default:
			if meta, ok := parseSender(sm.Sender); ok {
				blocks = withPreamble(blocks, injectedPreamble(meta)) // a message another chat injected
			}
			out = append(out, llm.Message{Role: llm.RoleUser, Content: blocks})
		}
	}
	return repairToolPairs(out)
}

// repairToolPairs makes sure every assistant tool call is answered by a tool
// message directly after it, inserting an error result where none exists, and
// drops tool results that answer nothing.
func repairToolPairs(in []llm.Message) []llm.Message {
	var out []llm.Message
	for i := 0; i < len(in); i++ {
		msg := in[i]
		if msg.Role == llm.RoleTool {
			// A tool message not consumed below answers nothing.
			continue
		}
		out = append(out, msg)
		if msg.Role != llm.RoleAssistant || len(msg.ToolCalls) == 0 {
			continue
		}
		want := map[string]bool{}
		for _, c := range msg.ToolCalls {
			want[c.ID] = true
		}
		j := i + 1
		for ; j < len(in) && in[j].Role == llm.RoleTool; j++ {
			var keep []llm.ToolResult
			for _, r := range in[j].ToolResults {
				if want[r.ToolCallID] {
					keep = append(keep, r)
					delete(want, r.ToolCallID)
				}
			}
			if len(keep) > 0 {
				out = append(out, llm.Message{Role: llm.RoleTool, ToolResults: keep})
			}
		}
		for _, c := range msg.ToolCalls {
			if want[c.ID] {
				out = append(out, llm.Message{Role: llm.RoleTool, ToolResults: []llm.ToolResult{{
					ToolCallID: c.ID, IsError: true,
					Content: textBlocks("the call was interrupted before a result was recorded"),
				}}})
			}
		}
		i = j - 1
	}
	return out
}

// blocksFromResult renders an MCP result map's content array as blocks.
func blocksFromResult(res map[string]any) []llm.Block {
	items, _ := res["content"].([]any)
	var out []llm.Block
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		switch m["type"] {
		case "text":
			s, _ := m["text"].(string)
			out = append(out, llm.Block{Type: llm.BlockText, Text: s})
		case "image":
			data, _ := m["data"].(string)
			mt, _ := m["mimeType"].(string)
			out = append(out, llm.Block{Type: llm.BlockImage, Data: data, MediaType: mt})
		default:
			out = append(out, llm.Block{Type: llm.BlockText, Text: string(mustJSON(m))})
		}
	}
	return out
}
