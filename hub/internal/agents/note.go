package agents

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

// Persistent per-chat notes (capability pass, Oct 2026): the agent's own
// memory of what it LEARNS about a project — conventions, environment traps,
// decisions — kept in the chats row and re-injected into every later system
// prompt. Compaction summarizes a conversation; notes outlive it.

const (
	noteReadName   = "switchboard.note.read"
	noteAppendName = "switchboard.note.append"
	noteMaxEntry   = 2000
	noteMaxBody    = 16000
)

var noteAppendAnn = &mcp.ToolAnnotations{DestructiveHint: bp(false), OpenWorldHint: bp(false)}

var noteReadTool = &sbTool{
	name:    noteReadName,
	desc:    "Read this chat's persistent notes: the memories you (or an earlier run of you) appended with switchboard.note.append. They are also injected into your system prompt, so you normally never need this — use it to re-read the exact text. Example: note.read {}",
	schema:  obj(nil, map[string]any{}),
	ann:     &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: bp(false)},
	visible: func(store.Capabilities) bool { return true },
	run: func(m *Manager, ctx context.Context, cc *callCtx, _ map[string]any) (any, error) {
		chatID, err := noteChatID(cc)
		if err != nil {
			return nil, err
		}
		c, err := m.st.GetChat(ctx, chatID)
		if err != nil || c == nil {
			return nil, fmt.Errorf("%w: chat not found", ErrInvalid)
		}
		if c.Notes == "" {
			return map[string]any{"notes": "", "note": "no notes yet; append one whenever you learn something a future session would waste time rediscovering"}, nil
		}
		return map[string]any{"notes": c.Notes}, nil
	},
}

var noteAppendTool = &sbTool{
	name: noteAppendName,
	desc: "Append one durable note to this chat's persistent memory (re-injected into your system prompt on every later turn, and it survives context compression). " +
		"Write what you WISH you had known when you started and had to find out the hard way: environment traps, correct commands and where they run, file-layout surprises, user decisions. " +
		"One note = one fact, a short imperative line; never store secrets, and do not restate what the instruction files already say. " +
		`Example: {"entry": "run the web tests with 'npx vitest run' from hub/web — repo-root runs miss the client config"}`,
	schema: obj([]string{"entry"}, map[string]any{
		"entry": map[string]any{"type": "string", "maxLength": 2000, "description": "the fact, one or two lines, written for a future session of you"},
	}),
	ann:     noteAppendAnn,
	visible: func(store.Capabilities) bool { return true },
	run: func(m *Manager, ctx context.Context, cc *callCtx, args map[string]any) (any, error) {
		chatID, err := noteChatID(cc)
		if err != nil {
			return nil, err
		}
		entry := strings.TrimSpace(argStr(args, "entry"))
		if entry == "" {
			return nil, fmt.Errorf("%w: entry must not be empty", ErrInvalid)
		}
		if utf8.RuneCountInString(entry) > noteMaxEntry {
			return nil, fmt.Errorf("%w: an entry is at most %d characters", ErrInvalid, noteMaxEntry)
		}
		c, err := m.st.GetChat(ctx, chatID)
		if err != nil || c == nil {
			return nil, fmt.Errorf("%w: chat not found", ErrInvalid)
		}
		next := entry
		if c.Notes != "" {
			next = c.Notes + "\n- " + entry
		} else {
			next = "- " + entry
		}
		if utf8.RuneCountInString(next) > noteMaxBody {
			return nil, fmt.Errorf("%w: notes would exceed %d characters — shorten the entry, or PATCH /api/chats/%s to rewrite the block", ErrInvalid, noteMaxBody, chatID)
		}
		if _, err := m.st.UpdateChat(ctx, chatID, store.ChatPatch{Notes: &next}); err != nil {
			return nil, err
		}
		if cc.rs != nil {
			m.publish(eventChat(cc.rs))
		}
		return map[string]any{"ok": true, "entries": strings.Count(next, "\n- ") + 1}, nil
	},
}

func init() {
	registerExtraTool(noteReadTool, nil)
	registerExtraTool(noteAppendTool, nil)
}

func noteChatID(cc *callCtx) (string, error) {
	if cc == nil || cc.rs == nil || cc.rs.chatID == "" {
		return "", fmt.Errorf("%w: notes belong to a chat, and this call has no chat context", ErrInvalid)
	}
	return cc.rs.chatID, nil
}

// notePrompt renders the chat's notes for injection into the turn plan.
func notePrompt(chat *store.Chat) string {
	if chat == nil || strings.TrimSpace(chat.Notes) == "" {
		return ""
	}
	return "## Notes (your own memories for this chat)\n\n" +
		"Appended by you across sessions of this chat with switchboard.note.append — they survived every " +
		"context compression. Trust them as things you verified yourself, re-check anything cheaply " +
		"checkable that a new session could re-learn, and append a note the moment you learn something a " +
		"future session would waste turns rediscovering.\n\n" + chat.Notes
}
