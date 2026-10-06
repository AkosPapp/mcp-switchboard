// Context compression (I11): when the prompt outgrows the chat's context
// budget, the oldest part of the active path is replaced by a model-written
// summary that stands in for it. The summary lives on the chat row (chats
// .summary / .summarize_upto_msg) and is applied in turn building, so nothing
// is deleted: the messages stay in the DAG (branches, search, export are
// unaffected); only the model's view of the conversation is replaced from the
// marker message backwards.
//
// Triggers: the /compact slash command (an explicit user turn), and before a
// turn whose estimated prompt reaches compactRatio of the effective window
// (once per run; the estimate uses the same pessimistic chars-per-token
// assumption as llm.fitToWindow).
package agents

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/llm"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

const (
	// compactRatio is the fraction of the effective context window at which a
	// turn is preceded by compression. 0.85 leaves room for the summary itself
	// plus the next completion.
	compactRatio = 85
	// compactKeepMin / compactKeepFrac: compression keeps a summary plus
	// enough tail messages to continue; never fewer than compactKeepMin
	// messages, and as many tail messages as fit within compactKeepFrac of
	// the window.
	compactKeepMin  = 8
	compactKeepFrac = 60
	// compactMsgChars caps each message's share of the summariser
	// transcript; compactTranscriptChars caps the whole transcript.
	compactMsgChars        = 1200
	compactTranscriptChars = 160_000
	// compactSummaryMaxChars bounds the generated summary itself.
	compactSummaryMaxChars = 6000
)

// effectiveWindow is the prompt-token budget for a chat's turns: the smaller
// of the model's window (when known) and the chat's own limit (I4); 0 = no
// known limit.
func effectiveWindow(chat *store.Chat, spec llm.ModelSpec) int64 {
	var w int64
	if spec.ContextWindow > 0 {
		w = int64(spec.ContextWindow)
	}
	if chat != nil && chat.ContextLimit > 0 && (w == 0 || chat.ContextLimit < w) {
		w = chat.ContextLimit
	}
	return w
}

// estimateTokens is a deliberately pessimistic prompt-size estimate: all
// textual content, system prompt, and tool schemas, at one token per four
// characters. It only decides when to compact, never what is sent.
func estimateTokens(system string, tools []llm.Tool, msgs []llm.Message) int64 {
	n := utf8.RuneCountInString(system)
	if len(tools) > 0 {
		if b, err := json.Marshal(tools); err == nil {
			n += len(b) / 2 // tool schemas compress well; count half their bytes
		}
	}
	for _, msg := range msgs {
		n += utf8.RuneCountInString(llm.BlocksText(msg.Content))
		for _, c := range msg.ToolCalls {
			n += len(c.Name)
			if b, err := json.Marshal(c.Arguments); err == nil {
				n += len(b)
			}
		}
		for _, r := range msg.ToolResults {
			n += utf8.RuneCountInString(llm.BlocksText(r.Content))
		}
	}
	return int64(n) / 4
}

// applyChatSummary rewrites an active path for model consumption: when the
// chat has a compression summary, everything up to and including the marker
// message is replaced by one synthetic message carrying the summary. If the
// marker is not on the current branch (the user branched below it), the
// summary is still prepended — it describes a shared part of the past — and
// nothing is dropped (the conservative choice).
func applyChatSummary(path []store.Message, chat *store.Chat) []store.Message {
	if chat == nil || strings.TrimSpace(chat.Summary) == "" {
		return path
	}
	idx := -1
	for i, msg := range path {
		if msg.ID == chat.SummarizeUptoMsg {
			idx = i
		}
	}
	syn := store.Message{
		ChatID:  chat.ID,
		Role:    store.RoleUser,
		Content: mustJSON([]llm.Block{{Type: llm.BlockText, Text: "[Summary of the earlier part of this conversation, compressed by the hub]\n\n" + chat.Summary}}),
	}
	if idx >= 0 && idx < len(path)-1 {
		return append([]store.Message{syn}, path[idx+1:]...)
	}
	return append([]store.Message{syn}, path...)
}

// summariseSystem is the system prompt of the compaction call itself.
const summariseSystem = "You compress a conversation so another AI assistant can continue it from your summary alone. " +
	"Preserve, tersely and completely: what the user is trying to achieve; decisions taken and why; file paths, commands, " +
	"branch names, identifiers and numbers that were produced or referenced; open loops and promises not yet fulfilled; " +
	"anything the user corrected or insisted on. Do not editorialize, do not propose next steps, output only the summary."

// compactNow writes a new compression summary for the chat: the oldest part of
// the active path (beyond a continuation-sized tail) is summarised by the
// chat's own model and recorded via SetChatSummary. ok=false (with no write)
// when the path is too short to be worth compressing or the model call fails.
func (m *Manager) compactNow(ctx context.Context, rs *runState, mc ModelConfig) (summary string, dropped int, ok bool) {
	path, err := m.st.ActivePath(ctx, rs.chatID)
	if err != nil || len(path) <= compactKeepMin+1 {
		return "", 0, false
	}
	window := m.windowFor(ctx, rs)
	// Tail to keep: the last compactKeepMin messages, minus more while the
	// tail alone still exceeds its share of the window (then compaction
	// would not help and we bail out).
	keep := compactKeepMin
	for keep < len(path)-2 {
		tail := path[len(path)-keep:]
		if estimateTokens("", nil, toLLMMessages(tail, m.set.ToolResultMaxChars)) <= int64(window)*compactKeepFrac/100 {
			break
		}
		keep++
	}
	split := len(path) - keep
	if split < 2 {
		return "", 0, false
	}
	prefix := path[:split]
	transcript := compactTranscript(prefix)
	if transcript == "" {
		return "", 0, false
	}
	prov, have := m.llm.Provider(mc.Provider)
	if !have {
		return "", 0, false
	}
	opts := mc.options(summariseSystem)
	opts.Thinking = nil // a summary does not need reasoning at its full budget
	opts.Effort = ""
	msgs := []llm.Message{{Role: llm.RoleUser, Content: []llm.Block{{Type: llm.BlockText,
		Text: "Compress the conversation transcript below.\n\n" + transcript}}}}
	ch, err := prov.Complete(ctx, msgs, nil, opts)
	if err != nil {
		return "", 0, false
	}
	var sb strings.Builder
	for d := range ch {
		switch d.Type {
		case llm.DeltaError:
			return "", 0, false
		case llm.DeltaText:
			sb.WriteString(d.Text)
			if sb.Len() > compactSummaryMaxChars*2 { // the model ignored the bound
				break
			}
		}
	}
	summary = strings.TrimSpace(truncateRunes(sb.String(), compactSummaryMaxChars))
	if summary == "" || ctx.Err() != nil {
		return "", 0, false
	}
	if err := m.st.SetChatSummary(ctx, rs.chatID, summary, prefix[len(prefix)-1].ID); err != nil {
		return "", 0, false
	}
	return summary, len(prefix), true
}

// compactTranscript flattens messages into "role: text" lines for the
// summariser, bounded per message and overall.
func compactTranscript(msgs []store.Message) string {
	var sb strings.Builder
	truncated := false
	for _, sm := range msgs {
		var blocks []llm.Block
		_ = json.Unmarshal(sm.Content, &blocks)
		text := llm.BlocksText(blocks)
		if text == "" && len(sm.ToolCalls) == 0 {
			continue
		}
		text = truncateRunes(strings.TrimSpace(text), compactMsgChars)
		role := sm.Role
		switch {
		case len(sm.ToolCalls) > 0 && text == "":
			text = "(tool calls only)"
		case len(sm.ToolCalls) > 0:
			text += " (plus tool calls)"
		}
		if text == "" {
			continue
		}
		if sb.Len()+len(text) > compactTranscriptChars {
			truncated = true
			break
		}
		sb.WriteString(role)
		sb.WriteString(": ")
		sb.WriteString(text)
		sb.WriteString("\n")
	}
	if truncated {
		sb.WriteString("… (the oldest turns were dropped from the transcript; summarise what is above)\n")
	}
	return sb.String()
}

// windowFor is the chat's effective prompt budget for estimates: the smaller
// of its own limit and the agent's model window.
func (m *Manager) windowFor(ctx context.Context, rs *runState) int64 {
	var chat *store.Chat
	if rs.chatID != "" {
		chat, _ = m.st.GetChat(ctx, rs.chatID)
	}
	mc := m.modelFor(ctx, chat, rs.agentID)
	var w int64
	if chat != nil && chat.ContextLimit > 0 {
		w = chat.ContextLimit
	}
	if spec, _, known := m.llm.Lookup(mc.Provider, mc.Model); known && spec.ContextWindow > 0 {
		if w == 0 || int64(spec.ContextWindow) < w {
			w = int64(spec.ContextWindow)
		}
	}
	return w
}

// /compact: the command turn. Detection is an exact match on the last user
// message (so "/compact me" stays a normal prompt) — the message itself stays
// in the transcript, the reply records what happened.
func compactCommand(text string) bool {
	return strings.TrimSpace(text) == "/compact"
}

// compactCommandTurn answers a /compact submission: it compresses (or reports
// there is nothing to compress) and replies as the assistant, ending the run
// without calling the chat model for the conversation.
func (m *Manager) compactCommandTurn(ctx context.Context, rs *runState) runOutcome {
	agent, err := m.st.GetAgent(ctx, rs.agentID)
	if err != nil || agent == nil {
		return runOutcome{status: store.RunError, finishReason: "error", err: "agent vanished during /compact"}
	}
	agent, _ = m.resolveAgent(ctx, agent)
	chat, err := m.st.GetChat(ctx, rs.chatID)
	if err != nil || chat == nil {
		return runOutcome{status: store.RunError, finishReason: "error", err: "chat vanished during /compact"}
	}
	mc := m.effectiveModelConfig(ctx, chat, agent, rs)
	summary, dropped, ok := m.compactNow(ctx, rs, mc)
	text := "Nothing to compress yet — this conversation still fits its context budget."
	if ok {
		text = "Context compressed: the " + itoa(dropped) +
			" oldest messages are replaced by the summary below on every further turn.\n\n" + summary
	} else if ctx.Err() != nil {
		return causeOutcome(rs)
	}
	assistant := store.Message{
		ChatID:  rs.chatID,
		Role:    store.RoleAssistant,
		Content: mustJSON([]llm.Block{{Type: llm.BlockText, Text: text}}),
		Model:   mustJSON(mc),
	}
	rs.leafMu.Lock()
	assistant.ParentID = rs.leaf
	pctx, pcancel := detached()
	saved, err := m.st.AppendMessageWithUsage(pctx, assistant)
	pcancel()
	rs.leafMu.Unlock()
	if err == nil {
		rs.leaf = &saved.ID
		sctx, scancel := detached()
		_ = m.st.SetActiveLeaf(sctx, rs.chatID, saved.ID)
		scancel()
		m.frame(rs, "message_done", saved)
	} else {
		m.log.Warn("compact reply could not be appended", "chatId", rs.chatID, "err", err)
	}
	// Usage of the summary call is deliberately not attributed to the run:
	// compaction is maintenance, not conversation turns.
	m.publish(eventChat(rs))
	return runOutcome{status: store.RunDone, finishReason: "stop"}
}

// lastUserText returns the text of the newest user message on the active path.
func (m *Manager) lastUserText(ctx context.Context, chatID string) string {
	path, err := m.st.ActivePath(ctx, chatID)
	if err != nil {
		return ""
	}
	for i := len(path) - 1; i >= 0; i-- {
		if path[i].Role != store.RoleUser {
			continue
		}
		var blocks []llm.Block
		_ = json.Unmarshal(path[i].Content, &blocks)
		return llm.BlocksText(blocks)
	}
	return ""
}

// modelFor is the effective model config for estimates when the turn's own
// resolution result is not at hand.
func (m *Manager) modelFor(ctx context.Context, chat *store.Chat, agentID string) ModelConfig {
	agent, err := m.st.GetAgent(ctx, agentID)
	if err != nil || agent == nil {
		return ModelConfig{}
	}
	return m.effectiveModelConfig(ctx, chat, agent, nil)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func itoa(v int) string { return strconv.Itoa(v) }
