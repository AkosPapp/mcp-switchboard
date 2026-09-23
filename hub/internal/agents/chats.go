package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/events"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/llm"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

// Chats are the unit (docs/CHAT_MODEL_API.md): creating, rebinding and deleting
// a chat, and the messages chats inject into each other. The agents row stays as
// the chat's internal execution record, 1:1 for everything created from now on.

const defaultChatTitle = "New chat"

// Kinds of an injected message (Message.sender.kind).
const (
	SenderMessage = "message" // switchboard.chat.send
	SenderReply   = "reply"   // a returned answer
	SenderSpawn   = "spawn"   // the first task of a spawned child
)

// senderMeta is messages.sender.
type senderMeta struct {
	ChatID    string `json:"chatId"`
	ChatTitle string `json:"chatTitle"`
	Kind      string `json:"kind"`
}

func parseSender(raw json.RawMessage) (senderMeta, bool) {
	var s senderMeta
	if len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &s) != nil || s.ChatID == "" {
		return s, false
	}
	return s, true
}

// injectedPreamble is the one line that opens a message another chat injected,
// as the model reads it. It is added when a provider request is built, never
// stored: the message keeps the raw text, and the console shows it beside a
// sender badge. This is the ONLY place the wording lives.
func injectedPreamble(s senderMeta) string {
	title := strings.TrimSpace(s.ChatTitle)
	if title == "" {
		title = "untitled"
	}
	switch s.Kind {
	case SenderReply:
		return fmt.Sprintf("[Reply from chat %q (id %s).]", title, s.ChatID)
	case SenderSpawn:
		return fmt.Sprintf("[Task from your parent chat %q (id %s). Your final answer is delivered back to it.]", title, s.ChatID)
	}
	return fmt.Sprintf("[Message from chat %q (id %s). Reply with switchboard.chat.send to that id.]", title, s.ChatID)
}

// withPreamble returns blocks with the preamble in front of the first text block
// (or as a block of its own); the input is not modified.
func withPreamble(blocks []llm.Block, preamble string) []llm.Block {
	out := make([]llm.Block, 0, len(blocks)+1)
	if len(blocks) > 0 && blocks[0].Type == llm.BlockText {
		first := blocks[0]
		first.Text = preamble + "\n\n" + first.Text
		return append(append(out, first), blocks[1:]...)
	}
	return append(append(out, llm.Block{Type: llm.BlockText, Text: preamble}), blocks...)
}

// agentNameFor is the (unique among siblings) name of a chat's execution record.
func agentNameFor(title, id string) string {
	t := firstLine(title, 40)
	if t == "" {
		t = "chat"
	}
	return t + " · " + id[max(len(id)-6, 0):]
}

// primaryChat is the chat a message for the record lands in when no chat is
// named: its most recently active human chat, else any other live chat. With
// create, a record without any gets a fresh one.
func (m *Manager) primaryChat(ctx context.Context, agent *store.Agent, create bool) (*store.Chat, error) {
	m.chatMu.Lock()
	defer m.chatMu.Unlock()
	chats, err := m.st.ListChats(ctx, store.ChatFilter{AgentID: agent.ID, Limit: store.MaxLimit})
	if err != nil {
		return nil, err
	}
	var other *store.Chat
	for i := range chats { // most recently updated first
		if chats[i].Kind == store.ChatKindHuman {
			return &chats[i], nil
		}
		if other == nil {
			other = &chats[i]
		}
	}
	if other != nil {
		return other, nil
	}
	if !create {
		return nil, nil
	}
	c, err := m.st.CreateChat(ctx, store.Chat{AgentID: agent.ID, Kind: store.ChatKindHuman, Title: defaultChatTitle,
		ProfileID: agent.ProfileID, ClientLabel: agent.ClientLabel})
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// CreateChat implements Service.
func (m *Manager) CreateChat(ctx context.Context, in CreateChatInput) (*store.Chat, error) {
	label := strings.TrimSpace(in.ClientLabel)
	if label == store.Wildcard {
		return nil, fmt.Errorf("%w: clientLabel must name exactly one client (not empty, not *)", ErrInvalid)
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		title = defaultChatTitle
	}
	ci := CreateAgentInput{
		Name: agentNameFor(title, store.NewID()), ChatTitle: title, Model: in.Model,
		ParentChatID: in.ParentChatID, ClientSet: true, withChat: true, origin: store.OriginChat,
	}
	if label != "" {
		ci.ClientLabel = &label
	}
	if in.ParentChatID != "" {
		_, parent, err := m.liveAgentForChat(ctx, in.ParentChatID)
		if err != nil {
			return nil, err
		}
		ci.ParentID, ci.origin = parent.ID, store.OriginSpawn
	}
	if _, err := normModel(in.Model); err != nil {
		return nil, err
	}
	// Prompt source: profile, own text, or (nothing said at all) the default profile.
	switch {
	case in.ProfileID != "":
		ci.ProfileID = in.ProfileID
	case in.ProfileNone || in.SystemPrompt != nil:
		if in.SystemPrompt != nil {
			ci.SystemPrompt = *in.SystemPrompt
		}
	default:
		def, err := m.st.GetDefaultProfile(ctx)
		if err != nil {
			return nil, err
		}
		if def != nil {
			ci.ProfileID = def.ID
		}
	}
	_, chat, err := m.createAgent(ctx, ci, false)
	if err != nil {
		return nil, err
	}
	m.publish(events.Event{Type: events.TypeChat, ChatID: chat.ID, AgentID: chat.AgentID})
	return chat, nil
}

// UpdateChat implements Service.
func (m *Manager) UpdateChat(ctx context.Context, id string, in ChatUpdate) (*store.Chat, error) {
	chat, agent, err := m.liveAgentForChat(ctx, id)
	if err != nil {
		return nil, err
	}
	var patch store.AgentPatch
	profile := agent.ProfileID
	if in.ProfileSet {
		if in.ProfileID != nil && strings.TrimSpace(*in.ProfileID) == "" {
			return nil, fmt.Errorf("%w: profileId must not be empty", ErrInvalid)
		}
		patch.SetProfile, patch.ProfileID = true, in.ProfileID
		profile = in.ProfileID
	}
	if in.SystemPrompt != nil && profile == nil {
		patch.SystemPrompt = in.SystemPrompt
	}
	if in.ClientSet {
		patch.SetClient, patch.ClientLabel = true, in.ClientLabel
	}
	if patch.SetProfile || patch.SetClient || patch.SystemPrompt != nil {
		updated, err := m.UpdateAgent(ctx, agent.ID, patch)
		if err != nil {
			return nil, err
		}
		if patch.SetProfile && patch.ProfileID != nil && updated.Origin == store.OriginSpawn {
			// A sub-chat follows its profile only once, when it is (re)bound.
			if prof, err := m.GetProfile(ctx, *patch.ProfileID); err == nil {
				m.applyProfile(ctx, updated, prof)
			}
		}
		if err := m.mirrorChatScope(ctx, agent.ID, patch); err != nil {
			return nil, err
		}
	}
	fresh, err := m.st.GetChat(ctx, chat.ID)
	if err != nil || fresh == nil {
		return nil, fmt.Errorf("re-reading chat %s: %w", id, firstError(err, ErrNotFound))
	}
	return fresh, nil
}

func firstError(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// mirrorChatScope copies a rebinding onto the mirrored profile/client fields of
// every chat of the record, and clears the client of descendants' chats that the
// change narrowed away (A14).
func (m *Manager) mirrorChatScope(ctx context.Context, agentID string, patch store.AgentPatch) error {
	apply := func(id string, cp store.ChatPatch) error {
		chats, err := m.st.ListChats(ctx, store.ChatFilter{AgentID: id, IncludeArchived: true, Limit: store.MaxLimit})
		if err != nil {
			return err
		}
		for _, c := range chats {
			if _, err := m.st.UpdateChat(ctx, c.ID, cp); err != nil {
				return err
			}
			m.publish(events.Event{Type: events.TypeChat, ChatID: c.ID, AgentID: id})
		}
		return nil
	}
	cp := store.ChatPatch{SetProfile: patch.SetProfile, ProfileID: patch.ProfileID, SetClient: patch.SetClient, ClientLabel: patch.ClientLabel}
	if cp.SetProfile || cp.SetClient {
		if err := apply(agentID, cp); err != nil {
			return err
		}
	}
	if patch.SetClient {
		desc, err := m.st.Descendants(ctx, agentID)
		if err != nil {
			return err
		}
		for _, d := range desc {
			if a, err := m.st.GetAgent(ctx, d); err == nil && a != nil && a.ClientLabel == nil {
				if err := apply(d, store.ChatPatch{SetClient: true}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// DeleteChat implements Service.
func (m *Manager) DeleteChat(ctx context.Context, id string) (int, error) {
	chat, err := m.st.GetChat(ctx, id)
	if err != nil {
		return 0, err
	}
	if chat == nil {
		return 0, fmt.Errorf("%w: chat %s", ErrNotFound, id)
	}
	// The subtree, so its runs stop before anything is deleted under them.
	ids, queue := []string{id}, []string{id}
	for len(queue) > 0 {
		kids, err := m.st.ListChats(ctx, store.ChatFilter{ParentChatID: queue[0], IncludeArchived: true, Limit: store.MaxLimit})
		if err != nil {
			return 0, err
		}
		queue = queue[1:]
		for _, k := range kids {
			ids = append(ids, k.ID)
			queue = append(queue, k.ID)
		}
		if len(ids) > 100000 {
			break
		}
	}
	m.cancelChats(ids, errCancelled)
	del, err := m.st.DeleteChatCascade(ctx, id)
	if err != nil {
		return 0, err
	}
	m.syncAgentGauge()
	m.publish(events.Event{Type: events.TypeGraph})
	for _, c := range del.ChatIDs {
		m.publish(events.Event{Type: events.TypeChat, ChatID: c})
	}
	return len(del.ChatIDs), nil
}

// cancelChats cancels every in-memory run on the given chats and waits for them
// (bounded), so a delete does not race their final writes.
func (m *Manager) cancelChats(ids []string, cause error) {
	set := map[string]bool{}
	for _, id := range ids {
		set[id] = true
	}
	var victims []*runState
	m.mu.Lock()
	for _, rs := range m.runs {
		if set[rs.chatID] {
			victims = append(victims, rs)
		}
	}
	m.mu.Unlock()
	for _, rs := range victims {
		m.cancelRunState(rs, cause)
	}
	deadline := time.After(5 * time.Second)
	for _, rs := range victims {
		select {
		case <-rs.done:
		case <-deadline:
			return
		}
	}
}
