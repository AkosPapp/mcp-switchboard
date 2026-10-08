package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/events"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/llm"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/push"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

// Bridge chats mirror an external session (the opencode plugin) into the hub:
// the console can read the transcript and type into it, but nothing here ever
// runs - the brain is outside. These messages arrive through Service methods
// called by the API routes (POST /api/chats/{id}/bridge/*) and through the
// normal POST /messages path, which Manager.Post short-circuits for this kind.

const (
	bridgeSenderName = "opencode"
	// bridgeMaxChars caps one mirrored line; opencode parts are bounded well
	// below this, so hitting it means a runaway tool dump - keep the chat
	// readable rather than reject the whole POST.
	bridgeMaxChars = 200_000
)

// bridgeFrame nudges the per-chat stream so open console tabs refetch. A
// bridge write carries no run, so the frame has no run id (no resume
// position - an offline tab just refetches, same as any cold open).
func (m *Manager) bridgeFrame(chatID string) {
	if m.stream != nil {
		m.stream.Publish(chatID, "", "chat_message", map[string]any{"chatId": chatID})
	}
}

// chatKindFor picks the chat kind a createAgent withChat record gets.
func chatKindFor(in CreateAgentInput) string {
	if in.bridge {
		return store.ChatKindBridge
	}
	return store.ChatKindHuman
}

// bridgeOf loads chatID and insists it is a bridge chat.
func (m *Manager) bridgeOf(ctx context.Context, chatID string) (*store.Chat, error) {
	chat, err := m.st.GetChat(ctx, chatID)
	if err != nil {
		return nil, err
	}
	if chat == nil {
		return nil, fmt.Errorf("%w: chat %s", ErrNotFound, chatID)
	}
	if chat.Kind != store.ChatKindBridge {
		return nil, fmt.Errorf("%w: chat %s is not a bridge chat", ErrInvalid, chatID)
	}
	return chat, nil
}

// postBridge handles a console send into a bridge chat: store the message and
// stop. The plugin polls GET /messages (or watches the chat event) and feeds
// the text to the external session; scheduling a hub agent here would run a
// second brain on the same window.
func (m *Manager) postBridge(ctx context.Context, chat *store.Chat, in PostInput) (*PostResult, error) {
	// Thread onto the active leaf the way the plugin will thread its reply:
	// as roots both messages would be single-message paths past the next
	// append, invisible to GET /messages (and to the plugin's own polling)
	// the moment anything else lands.
	parent := strPtrIf(in.ParentID)
	if parent == nil {
		parent = leafPtr(chat)
	}
	msg := store.Message{ID: store.NewID(), ChatID: chat.ID, Role: store.RoleUser, Content: mustJSON(in.Content), ParentID: parent}
	if in.IdempotencyKey != "" {
		existing, claimed, err := m.st.ClaimIdempotencyKey(ctx, in.IdempotencyKey, msg.ID)
		if err != nil {
			return nil, err
		}
		if !claimed {
			return &PostResult{MessageID: existing, Deduplicated: true}, nil
		}
	}
	saved, err := m.st.AppendMessageClearingDraft(ctx, msg)
	if err != nil {
		return nil, err
	}
	m.publish(events.Event{Type: events.TypeChat, ChatID: chat.ID})
	m.bridgeFrame(chat.ID)
	return &PostResult{MessageID: saved.ID}, nil
}

// BridgeAppend is one mirrored transcript line from the external side.
type BridgeAppend struct {
	Role   string      // "assistant" | "user"
	Text   string      // plain text; tool activity folded in by the caller
	Source string      // display name, default "opencode"
	Blocks []llm.Block // optional rich content (wins over Text)
}

// AppendBridge implements Service: the external engine publishing its side of
// the conversation. No run scheduling, ever - kind was checked.
func (m *Manager) AppendBridge(ctx context.Context, chatID string, in BridgeAppend) (*store.Message, error) {
	chat, err := m.bridgeOf(ctx, chatID)
	if err != nil {
		return nil, err
	}
	var role string
	switch in.Role {
	case "user":
		role = store.RoleUser
	case "assistant", "tool", "reasoning":
		role = store.RoleAssistant
	case "":
		return nil, fmt.Errorf("%w: role is required", ErrInvalid)
	default:
		return nil, fmt.Errorf("%w: unknown bridge role %q", ErrInvalid, in.Role)
	}
	content := in.Blocks
	if len(content) == 0 {
		text := in.Text
		if len(text) > bridgeMaxChars {
			text = text[:bridgeMaxChars] + "\n…"
		}
		if strings.TrimSpace(text) == "" {
			return nil, fmt.Errorf("%w: text is required", ErrInvalid)
		}
		content = []llm.Block{{Type: llm.BlockText, Text: text}}
	}
	source := strings.TrimSpace(in.Source)
	if source == "" {
		source = bridgeSenderName
	}
	sender, err := json.Marshal(senderMeta{SenderName: source, ChatTitle: source, Kind: "bridge"})
	if err != nil {
		return nil, err
	}
	msg := store.Message{
		ID: store.NewID(), ChatID: chat.ID, Role: role,
		Content: mustJSON(content), Sender: sender, ParentID: leafPtr(chat),
	}
	saved, err := m.st.AppendMessage(ctx, msg)
	if err != nil {
		return nil, err
	}
	m.publish(events.Event{Type: events.TypeChat, ChatID: chat.ID})
	m.bridgeFrame(chat.ID)
	return &saved, nil
}

// BridgeQuestion asks the user (phone push, same channel as run errors) and
// reports whether push is even configured; the question text itself is
// mirrored into the chat separately by the caller so the console shows it.
func (m *Manager) BridgeQuestion(ctx context.Context, chatID, text string) (pushed bool, err error) {
	chat, err := m.bridgeOf(ctx, chatID)
	if err != nil {
		return false, err
	}
	if m.push == nil || !m.push.Enabled() {
		return false, nil
	}
	body := text
	if len(body) > 480 {
		body = body[:480] + "…"
	}
	errs := m.push.Send(ctx, push.Payload{Title: chat.Title, Body: body, ChatID: chat.ID, Kind: push.KindQuestion})
	for range errs {
	}
	return true, nil
}

// strPtrIf returns a pointer to s unless s is empty.
func strPtrIf(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// leafPtr points at the chat's active leaf, or nil for an empty thread.
func leafPtr(chat *store.Chat) *string {
	if chat.ActiveLeafID == nil || *chat.ActiveLeafID == "" {
		return nil
	}
	return chat.ActiveLeafID
}
