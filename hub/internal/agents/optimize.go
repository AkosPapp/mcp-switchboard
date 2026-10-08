package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/llm"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

// /optimize_skills — the self-improvement unlock (docs/improvements.md follow-up).
//
// A chat normally cannot touch the things that shape it: no other
// conversations, no prompts, no skills. Running /optimize_skills flips the
// chat's unlock flag (migration 015) and the switchboard.optimize.* tools
// appear in its catalog; /optimize_skills off locks it again. The tools edit
// live, shared state (the prompt every chat follows!), so every one of them
// re-checks the flag at run time and the command's reply tells the model, in
// full, how to use the unlock: study the history, find the harness's weak
// spots and bugs, report before touching anything.

const optimizeCmdName = "switchboard.optimize"

// optimizeCommand matches the whole-message command; the second result is the
// mode ("on"/"off").
func optimizeCommand(text string) (mode string, ok bool) {
	t := strings.TrimSpace(text)
	if t == "/optimize_skills" {
		return "on", true
	}
	if t == "/optimize_skills off" {
		return "off", true
	}
	return "", false
}

// optimizeBrief is the default meta-prompt the command installs: it is sent
// back as the hub's answer, so it is in the transcript (visible to the user)
// and in the model's next turn context.
const optimizeBrief = `Harness optimization unlocked for this chat: the switchboard.optimize.* tools are now in your tool list (they disappear again with "/optimize_skills off" or a new chat).

Use them in this order:

1. GATHER EVIDENCE, not vibes. Read this chat's history first (switchboard.chat.search, switchboard.calls.stats), then widen: optimize.chat_read on one or two of your OTHER recent chats from the same project (optimize.chats_list finds them). Look for the recurring patterns:
   - tool calls that failed, were retried with near-identical arguments, or timed out;
   - steps where you rediscovered something a note, a skill or a prompt line should already have told you;
   - a prompt or skill that was wrong, stale, missing a rule, or verbose enough that you ignored parts of it;
   - anything that looks like a real BUG in the hub, harness, client or console (wrong counts, a tool contradicting its own description, lost state) as opposed to a model mistake.
2. REPORT FIRST. Tell the user, concretely and cited ("in chat X, on <date>, call Y failed with Z because…"):
   - the weaknesses found, ranked, each with the proposed change to a prompt, a skill, or the harness itself;
   - for harness code changes: which repo, which file/function, and how the change would have altered the cited call — but code is OUT OF SCOPE for these tools; they edit prompts and skills only.
   - every bug: what you observed, the minimal reproduction, where it lives, severity.
3. EDIT ONLY WITH ASSENT. Prompts and skills are live shared state: an edit here changes every other chat that follows them. Propose the exact new text, wait for the user's OK in this chat, then apply with switchboard.prompt_set / switchboard.skill_set / switchboard.skill_delete, and say what you changed.
4. Keep changes small, dated and reversible: preserve what works, rewrite only the part that hurt (except a delete, which the user asked for by name).

You are allowed to improve yourself here — but it is the USER's harness and prompts; report and ask beat act.`

// optimizeCommandTurn answers /optimize_skills like /compact: hub-side, no
// conversation-model turn, the answer is an assistant message on the path.
func (m *Manager) optimizeCommandTurn(ctx context.Context, rs *runState, mode string) runOutcome {
	c, err := m.st.GetChat(ctx, rs.chatID)
	if err != nil || c == nil {
		return runOutcome{status: store.RunError, finishReason: "error", err: "chat vanished during /optimize_skills"}
	}
	on := mode == "on"
	if _, err := m.st.UpdateChat(ctx, rs.chatID, store.ChatPatch{Optimize: &on}); err != nil {
		return runOutcome{status: store.RunError, finishReason: "error", err: err.Error()}
	}
	text := "Self-modification tools locked again — the switchboard.optimize.* tools are gone from this chat's tool list. Run /optimize_skills to unlock."
	if on {
		text = optimizeBrief
		if c.Optimize {
			text = "Already unlocked; the tools stay available.\n\n" + optimizeBrief
		}
	}
	assistant := store.Message{
		ChatID:  rs.chatID,
		Role:    store.RoleAssistant,
		Content: mustJSON([]llm.Block{{Type: llm.BlockText, Text: text}}),
	}
	rs.leafMu.Lock()
	assistant.ParentID = rs.leaf
	pctx, pcancel := detached()
	saved, err := m.st.AppendMessageWithUsage(pctx, assistant)
	pcancel()
	if err == nil {
		rs.leaf = &saved.ID
		sctx, scancel := detached()
		_ = m.st.SetActiveLeaf(sctx, rs.chatID, saved.ID)
		scancel()
		m.frame(rs, "message_done", saved)
	} else {
		m.log.Warn("optimize reply could not be appended", "chatId", rs.chatID, "err", err)
	}
	m.publish(eventChat(rs))
	return runOutcome{status: store.RunDone, finishReason: "stop"}
}

// ---------------------------------------------------------------- tools ----

func optimizeGate(t *store.Chat) bool { return t != nil && t.Optimize }

var optAnnRead = &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: bp(false)}
var optAnnWrite = &mcp.ToolAnnotations{DestructiveHint: bp(false), OpenWorldHint: bp(false)}

// optChat resolves the calling chat and refuses unless the unlock is on: the
// catalog hides these tools otherwise, and this is the second door.
func (m *Manager) optChat(ctx context.Context, cc *callCtx) (*store.Chat, error) {
	if cc == nil || cc.rs == nil || cc.rs.chatID == "" {
		return nil, fmt.Errorf("%w: these tools belong to a chat run", ErrInvalid)
	}
	c, err := m.st.GetChat(ctx, cc.rs.chatID)
	if err != nil || c == nil {
		return nil, fmt.Errorf("%w: chat not found", ErrInvalid)
	}
	if !c.Optimize {
		return nil, denied("this chat has not run /optimize_skills; the tools are only offered after the command")
	}
	return c, nil
}

var optimizeChatsListTool = &sbTool{
	name: optimizeCmdName + ".chats_list",
	desc: "List this hub's recent live chats (id, title, client, when last active) so you can pick which conversations to review with optimize.chat_read. Read-only. Example: optimize.chats_list {}",
	schema: obj(nil, map[string]any{
		"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 50},
	}),
	ann: optAnnRead, visible: func(store.Capabilities) bool { return true }, needChat: optimizeGate,
	run: func(m *Manager, ctx context.Context, cc *callCtx, args map[string]any) (any, error) {
		if _, err := m.optChat(ctx, cc); err != nil {
			return nil, err
		}
		limit := 25
		if f, ok := args["limit"].(float64); ok && f >= 1 && f <= 50 {
			limit = int(f)
		}
		chats, err := m.st.ListChats(ctx, store.ChatFilter{Limit: limit})
		if err != nil {
			return nil, err
		}
		out := make([]map[string]any, 0, len(chats))
		for _, c := range chats {
			if c.ArchivedAt != nil {
				continue
			}
			out = append(out, map[string]any{
				"chat_id": c.ID, "title": c.Title, "client": c.ClientLabel,
				"kind": c.Kind, "updated_at": store.FormatTime(c.UpdatedAt),
				"here": c.ID == cc.rs.chatID,
			})
		}
		return map[string]any{"chats": out}, nil
	},
}

const optimizeReadMax = 40000

var optimizeChatReadTool = &sbTool{
	name: optimizeCmdName + ".chat_read",
	desc: "Read a chat's active path (role: text per message, newest last, tool results omitted) for reviewing your own past work. Chat ids come from optimize.chats_list. Read-only; output capped, `truncated` says when. Example: optimize.chat_read {chat_id: \"01a0…\"}",
	schema: obj([]string{"chat_id"}, map[string]any{
		"chat_id": typ("string", "chat id from optimize.chats_list"),
	}),
	ann: optAnnRead, visible: func(store.Capabilities) bool { return true }, needChat: optimizeGate,
	run: func(m *Manager, ctx context.Context, cc *callCtx, args map[string]any) (any, error) {
		if _, err := m.optChat(ctx, cc); err != nil {
			return nil, err
		}
		id := argStr(args, "chat_id")
		path, err := m.st.ActivePath(ctx, id)
		if err != nil {
			return nil, err
		}
		var b strings.Builder
		trunc := false
		for _, msg := range path {
			var blocks []llm.Block
			_ = json.Unmarshal(msg.Content, &blocks)
			text := strings.TrimSpace(llm.BlocksText(blocks))
			if text == "" {
				continue
			}
			step := fmt.Sprintf("%s: %s\n", msg.Role, shortenRunes(text, 1200))
			if utf8.RuneCountInString(b.String())+utf8.RuneCountInString(step) > optimizeReadMax {
				trunc = true
				break
			}
			b.WriteString(step)
		}
		return map[string]any{"chat_id": id, "transcript": b.String(), "truncated": trunc}, nil
	},
}

func init() {
	for _, t := range []*sbTool{
		optimizeChatsListTool, optimizeChatReadTool,
	} {
		registerExtraTool(t, nil)
	}
}

// shortenRunes caps a string by runes with an ellipsis.
func shortenRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
