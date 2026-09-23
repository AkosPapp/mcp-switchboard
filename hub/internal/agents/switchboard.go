package agents

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/calls"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/events"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/registry"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

// The built-in switchboard.* tools of spec.md 5.5. They are served only at
// /mcp/agent/{id} and to the run loop, never at any other scope, and are
// handled in-process (logged as calls: label "switchboard", server
// "orchestrator").

type callCtx struct {
	agent *store.Agent
	rs    *runState // nil when called through /mcp/agent/{id}
}

type sbTool struct {
	name    string
	desc    string
	schema  map[string]any
	ann     *mcp.ToolAnnotations
	visible func(store.Capabilities) bool
	run     func(m *Manager, ctx context.Context, cc *callCtx, args map[string]any) (any, error)
}

var (
	sbTools  []*sbTool
	sbByName = map[string]*sbTool{}
)

func bp(b bool) *bool { return &b }

func obj(required []string, props map[string]any) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func typ(t, desc string) map[string]any { return map[string]any{"type": t, "description": desc} }

// Visibility (M1): with neither capability a chat gets no switchboard tools.
func canSpawn(c store.Capabilities) bool   { return c.CanSpawn }
func canMessage(c store.Capabilities) bool { return c.CanMessage }
func canEither(c store.Capabilities) bool  { return c.CanSpawn || c.CanMessage }

func init() {
	grantItem := obj([]string{"label", "server"}, map[string]any{
		"label": typ("string", "host label or *"), "project": typ("string", "project, \"\" for none, or *"),
		"server": typ("string", "server name or *"), "allowed": typ("boolean", "default true"),
	})
	write := &mcp.ToolAnnotations{DestructiveHint: bp(true), OpenWorldHint: bp(false)}
	noApproval := &mcp.ToolAnnotations{DestructiveHint: bp(false), OpenWorldHint: bp(false)}
	sbTools = []*sbTool{
		{name: "switchboard.chat.spawn",
			desc: "Create a child chat of the current chat. Grants, capabilities and approval are bounded by your own (you can never give more than you hold); omit them to pass on what you have. An optional first message is delivered to the child as its task, and its final answer comes back to you as a message.",
			schema: obj([]string{"title", "system_prompt"}, map[string]any{
				"title":         typ("string", "what the child chat is for; shown as its title"),
				"model":         map[string]any{"type": "object", "description": "{provider, model}; defaults to yours"},
				"system_prompt": typ("string", "the child chat's system prompt"),
				"grants":        map[string]any{"type": "array", "items": grantItem},
				"budget":        map[string]any{"type": "object", "description": "per-run budget overrides"},
				"capabilities": map[string]any{"type": "object", "description": "{can_spawn, can_message}; at most what you hold",
					"properties": map[string]any{"can_spawn": map[string]any{"type": "boolean"}, "can_message": map[string]any{"type": "boolean"}}},
				"approval": map[string]any{"type": "string", "enum": []string{"never", "destructive", "always"}, "description": "at least as strict as yours"},
				"message":  typ("string", "the child's first task, delivered as a message from this chat")}),
			ann:     noApproval,
			visible: canSpawn, run: (*Manager).toolSpawn},
		{name: "switchboard.chat.send",
			desc: "Send a message to another chat you can reach: your parent, a child, or one connected to you (see chat.list). It is delivered into that chat as a message from this chat and wakes it. Always fire-and-forget: there is no wait and no synchronous reply. If the recipient wants to answer, it sends a message back to you the same way.",
			schema: obj([]string{"to", "message"}, map[string]any{
				"to": typ("string", "recipient's name, as returned by chat.list"), "message": typ("string", "the message")}),
			ann:     noApproval,
			visible: canMessage, run: (*Manager).toolSend},
		{name: "switchboard.chat.list",
			desc:    "List every chat you can currently message: your parent and children, and every chat joined to you by an edge.",
			schema:  obj(nil, map[string]any{}),
			ann:     &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: bp(false)},
			visible: canEither, run: (*Manager).toolList},
		{name: "switchboard.chat.stop",
			desc:    "Cancel the runs of one of your descendant chats.",
			schema:  obj([]string{"chat_id"}, map[string]any{"chat_id": typ("string", "descendant chat id")}),
			ann:     write,
			visible: canSpawn, run: (*Manager).toolStop},
		{name: "switchboard.mcp.list_tools",
			desc:    "List the tools you may call right now, grouped by MCP server, with each server's connection state.",
			schema:  obj(nil, map[string]any{}),
			ann:     &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: bp(false)},
			visible: canEither, run: (*Manager).toolListTools},
	}
	for _, t := range sbTools {
		sbByName[t.name] = t
	}
}

func argStr(a map[string]any, k string) string { s, _ := a[k].(string); return s }

func argBool(a map[string]any, k string) (val, present bool) {
	b, ok := a[k].(bool)
	return b, ok
}

func eventChat(rs *runState) events.Event {
	return events.Event{Type: events.TypeChat, ChatID: rs.chatID, AgentID: rs.agentID}
}

// isDescendant reports whether id is below ancestor and still live.
func (m *Manager) isDescendant(ctx context.Context, ancestor, id string) (*store.Agent, bool) {
	desc, err := m.st.Descendants(ctx, ancestor)
	if err != nil || !slices.Contains(desc, id) {
		return nil, false
	}
	a, err := m.st.GetAgent(ctx, id)
	if err != nil || a == nil || a.DeletedAt != nil {
		return nil, false
	}
	return a, true
}

func denied(format string, a ...any) error {
	return fmt.Errorf("%w: %s", calls.ErrDenied, fmt.Sprintf(format, a...))
}

// callerChat is the chat the calling record acts in: the chat of the run it is
// in, else (a call through /mcp/agent/{id}) its most recently active chat.
func (m *Manager) callerChat(ctx context.Context, cc *callCtx) (*store.Chat, error) {
	if cc.rs != nil {
		if c, err := m.st.GetChat(ctx, cc.rs.chatID); err != nil {
			return nil, err
		} else if c != nil {
			return c, nil
		}
	}
	c, err := m.primaryChat(ctx, cc.agent, true)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// ------------------------------------------------------------------- spawn

func (m *Manager) toolSpawn(ctx context.Context, cc *callCtx, args map[string]any) (any, error) {
	title := strings.TrimSpace(argStr(args, "title"))
	if title == "" {
		return nil, fmt.Errorf("%w: title is required", ErrInvalid)
	}
	parentChat, err := m.callerChat(ctx, cc)
	if err != nil {
		return nil, err
	}
	in := CreateAgentInput{
		ParentID: cc.agent.ID, Name: agentNameFor(title, store.NewID()), ChatTitle: title,
		SystemPrompt: argStr(args, "system_prompt"), ParentChatID: parentChat.ID,
	}
	if cc.agent.Project != nil {
		in.Project = *cc.agent.Project
	}
	if v, ok := args["model"].(map[string]any); ok {
		in.Model = mustJSON(v)
	}

	// The child inherits the parent's capabilities and approval; an explicit
	// value may only narrow them (A12 applied to M1 and W1).
	in.Capabilities, in.Approval = cc.agent.Capabilities, cc.agent.Approval
	if v, ok := args["capabilities"].(map[string]any); ok {
		want := store.Capabilities{}
		want.CanSpawn, _ = v["can_spawn"].(bool)
		want.CanMessage, _ = v["can_message"].(bool)
		if (want.CanSpawn && !cc.agent.Capabilities.CanSpawn) || (want.CanMessage && !cc.agent.Capabilities.CanMessage) {
			return nil, denied("you cannot give a child capabilities you do not hold")
		}
		in.Capabilities = want
	}
	if v := argStr(args, "approval"); v != "" {
		if !validApproval(v) {
			return nil, fmt.Errorf("%w: approval must be never, destructive or always", ErrInvalid)
		}
		if approvalRank(v) < approvalRank(cc.agent.Approval) {
			return nil, denied("you cannot give a child a laxer approval mode (%s) than your own (%s)", v, cc.agent.Approval)
		}
		in.Approval = v
	}
	if v, ok := args["budget"].(map[string]any); ok {
		in.Budget = mustJSON(v)
	}
	if raw, ok := args["grants"].([]any); ok {
		in.Grants = []store.Grant{}
		for _, it := range raw {
			g, ok := it.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%w: grants must be objects", ErrInvalid)
			}
			allowed, present := argBool(g, "allowed")
			if !present {
				allowed = true
			}
			in.Grants = append(in.Grants, store.Grant{Label: argStr(g, "label"), Project: argStr(g, "project"), Server: argStr(g, "server"), Allowed: allowed})
		}
	}
	in.withChat = true
	agent, chat, err := m.createAgent(ctx, in, true)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"chat_id": chat.ID}
	if msg := argStr(args, "message"); msg != "" {
		// The child's answer comes back into this chat as a reply message; nobody waits.
		route := &replyRoute{senderID: cc.agent.ID, recipient: agent.ID, senderChat: parentChat.ID, recipientChat: chat.ID}
		if _, _, err := m.deliverChatMessage(ctx, cc.agent, parentChat, agent, chat, msg, SenderSpawn, route); err != nil {
			out["note"] = "the chat was created, but its first message could not be delivered: " + err.Error()
		}
	}
	return out, nil
}

func approvalRank(a string) int {
	switch a {
	case store.ApprovalAlways:
		return 2
	case store.ApprovalDestructive:
		return 1
	}
	return 0
}

// -------------------------------------------------------------------- send

// replyRoute carries "the recipient's final assistant text for the run
// triggered by that message" back to whoever sent it: to the waiting caller
// when there is one, else injected into the sender's own chat as a message.
type replyRoute struct {
	senderID      string
	recipient     string
	senderChat    string
	recipientChat string
	mu            sync.Mutex
	waiter        chan replyMsg // nil: nobody waits
	detached      bool
}

type replyMsg struct {
	text   string
	status string
	err    string
}

func (r *replyRoute) offer(msg replyMsg) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.waiter == nil || r.detached {
		return false
	}
	r.waiter <- msg // buffered 1, offered at most once
	return true
}

func (r *replyRoute) detach() {
	r.mu.Lock()
	r.detached = true
	r.mu.Unlock()
}

// deliverChatMessage injects text into toChat (owned by `to`) as a user-role
// message carrying sender metadata for fromChat, and wakes `to` exactly like a
// human message would (B2; R4 queueing and the deferred append apply). A record
// with auto_wake off accumulates it in its mailbox instead; the run is then nil.
func (m *Manager) deliverChatMessage(ctx context.Context, from *store.Agent, fromChat *store.Chat, to *store.Agent, toChat *store.Chat, text, kind string, route *replyRoute) (string, *store.Run, error) {
	msgID := store.NewID()
	msg := &store.Message{
		ID: msgID, ChatID: toChat.ID, Role: store.RoleUser, Content: mustJSON(textBlocks(text)),
		Sender: mustJSON(senderMeta{ChatID: fromChat.ID, ChatTitle: fromChat.Title, SenderName: from.Name, Kind: kind}),
	}
	if !to.AutoWake {
		if err := m.appendPending(ctx, submission{chat: toChat, msg: msg}, nil); err != nil {
			return "", nil, err
		}
		fromID := from.ID
		if _, err := m.st.EnqueueInbox(ctx, store.InboxItem{AgentID: to.ID, FromAgentID: &fromID, ChatID: toChat.ID, MessageID: msgID}); err != nil {
			return "", nil, err
		}
		m.publish(events.Event{Type: events.TypeGraph})
		m.publish(events.Event{Type: events.TypeAgent, AgentID: to.ID})
		return msgID, nil, nil
	}
	trigger := store.TriggerAgentMessage
	if kind == SenderSpawn {
		trigger = store.TriggerSpawn
	}
	fromID := from.ID
	run, err := m.submit(ctx, submission{agent: to, chat: toChat, trigger: trigger, triggeredBy: &fromID, msg: msg, reply: route, enqueuedAt: time.Now()})
	return msgID, run, err
}

func (m *Manager) toolSend(ctx context.Context, cc *callCtx, args map[string]any) (any, error) {
	caller := cc.agent
	toName, message := argStr(args, "to"), argStr(args, "message")
	if toName == "" || message == "" {
		return nil, fmt.Errorf("%w: to and message are required", ErrInvalid)
	}
	// A8/A9: only a chat reachable from the caller (its parent, a child, or one
	// connected by an allowed edge) can be named — the same set chat.list shows
	// (X8: resolved by the caller's own identity, never trusted from the
	// arguments). Absence means denied, not merely "not found".
	reach, err := m.reachable(ctx, caller)
	if err != nil {
		return nil, err
	}
	var to *store.Agent
	for _, r := range reach {
		if r.agent.Name == toName {
			to = r.agent
			break
		}
	}
	if to == nil {
		return nil, denied("no chat named %q is reachable; call switchboard.chat.list", toName)
	}
	toChat, err := m.primaryChat(ctx, to, true)
	if err != nil {
		return nil, err
	}
	outChat, err := m.callerChat(ctx, cc) // whose message this is, for sender metadata
	if err != nil {
		return nil, err
	}

	// Always fire-and-forget (spec.md 5.5): no wait, no synchronous reply, no
	// reply route. The one exception, a spawned child's final answer coming
	// back to its spawning chat (B8), is a property of chat.spawn, not of
	// chat.send, and goes through deliverReply instead.
	msgID, run, err := m.deliverChatMessage(ctx, caller, outChat, to, toChat, message, SenderMessage, nil)
	if err != nil {
		return nil, err
	}
	if m.met != nil {
		m.met.CountAgentMessage(projectOf(caller))
	}
	m.emit("agent_message", map[string]any{"fromAgentId": caller.ID, "toAgentId": to.ID, "fromChatId": outChat.ID, "toChatId": toChat.ID, "messageId": msgID})

	out := map[string]any{"message_id": msgID}
	switch {
	case run == nil:
		out["note"] = "the recipient does not auto-wake; the message is in its mailbox"
	case run.Status == store.RunDone && run.FinishReason != nil && *run.FinishReason == "budget":
		out["note"] = "the recipient has exhausted its lifetime budget; no run was started"
	}
	return out, nil
}

func ifText(prefix, s string) string {
	if s == "" {
		return ""
	}
	return prefix + s
}

// deliverReply is called when a run that was triggered by a chat message ends.
// The answer goes to the waiting caller, else into the sender's own chat.
func (m *Manager) deliverReply(rs *runState, out runOutcome) {
	rs.usageMu.Lock()
	text := rs.lastText
	rs.usageMu.Unlock()
	route := rs.reply
	if route.offer(replyMsg{text: text, status: out.status, err: out.err}) {
		return
	}
	ctx, cancel := detached()
	defer cancel()
	from, err := m.st.GetAgent(ctx, route.recipient)
	if err != nil || from == nil {
		return
	}
	to, err := m.st.GetAgent(ctx, route.senderID)
	if err != nil || to == nil || to.DeletedAt != nil {
		return
	}
	chat, err := m.st.GetChat(ctx, route.senderChat)
	if err != nil || chat == nil {
		return
	}
	fromChat, err := m.st.GetChat(ctx, rs.chatID)
	if err != nil || fromChat == nil {
		return
	}
	if text == "" {
		text = fmt.Sprintf("(no reply text; the run ended %s%s)", out.status, ifText(": ", out.err))
	}
	// A reply carries no reply route of its own: answering a reply with a
	// reply would ping-pong without either model choosing to.
	if _, _, err := m.deliverChatMessage(ctx, from, fromChat, to, chat, text, SenderReply, nil); err != nil {
		m.log.Warn("could not deliver a reply", "to", to.ID, "error", err)
	}
}

// -------------------------------------------------------------------- list

// reachableRow is one chat the caller can currently message.
type reachableRow struct {
	agent    *store.Agent
	relation string
	depth    int
}

// reachable computes every chat the caller can currently message: its parent
// and children (relation parent/child, with depth) plus every chat joined to
// it by an allowed edge (relation connected), keyed by agent id. A chat that
// is both, e.g. a child the caller also holds an edge to (A8 makes that the
// common case), is listed once as parent/child, not duplicated as connected.
// This is the one place both chat.list and chat.send's name lookup draw from,
// so a name always resolves to something chat.list would also show.
func (m *Manager) reachable(ctx context.Context, agent *store.Agent) (map[string]*reachableRow, error) {
	seen := map[string]*reachableRow{}
	add := func(a *store.Agent, relation string, depth int) {
		if a == nil || a.DeletedAt != nil {
			return
		}
		if _, ok := seen[a.ID]; ok {
			return // parent/child (added first) wins over connected
		}
		seen[a.ID] = &reachableRow{agent: a, relation: relation, depth: depth}
	}

	if agent.ParentID != nil {
		parent, err := m.st.GetAgent(ctx, *agent.ParentID)
		if err != nil {
			return nil, err
		}
		add(parent, "parent", agent.Depth-1)
	}
	children, err := m.st.ListAgents(ctx, store.AgentFilter{ParentID: agent.ID})
	if err != nil {
		return nil, err
	}
	for i := range children {
		add(&children[i], "child", children[i].Depth)
	}
	edges, err := m.st.ListEdges(ctx, store.EdgeFilter{From: agent.ID})
	if err != nil {
		return nil, err
	}
	for _, e := range edges {
		if !e.Allowed {
			continue
		}
		a, err := m.st.GetAgent(ctx, e.To)
		if err != nil {
			return nil, err
		}
		add(a, "connected", 0)
	}
	return seen, nil
}

// toolList returns one flat list of every chat the caller can currently
// message. There is no scope argument (spec.md 5.5): this is deliberately the
// only way a chat discovers who it can talk to.
func (m *Manager) toolList(ctx context.Context, cc *callCtx, _ map[string]any) (any, error) {
	seen, err := m.reachable(ctx, cc.agent)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		r := seen[id]
		chat, err := m.primaryChat(ctx, r.agent, true)
		if err != nil {
			return nil, err
		}
		row := map[string]any{"name": r.agent.Name, "chat_id": chat.ID, "title": chat.Title, "status": r.agent.Status, "relation": r.relation}
		if r.relation != "connected" {
			row["depth"] = r.depth
		}
		out = append(out, row)
	}
	return out, nil
}

// -------------------------------------------------------------------- stop

// chatRecord resolves a chat id to the live execution record behind it.
func (m *Manager) chatRecord(ctx context.Context, chatID string) (*store.Chat, *store.Agent, bool) {
	if chatID == "" {
		return nil, nil, false
	}
	chat, agent, err := m.liveAgentForChat(ctx, chatID)
	if err != nil {
		return nil, nil, false
	}
	return chat, agent, true
}

func (m *Manager) toolStop(ctx context.Context, cc *callCtx, args map[string]any) (any, error) {
	id := argStr(args, "chat_id")
	_, rec, ok := m.chatRecord(ctx, id)
	if !ok {
		return nil, denied("chat %q is not a descendant of yours", id)
	}
	target, ok := m.isDescendant(ctx, cc.agent.ID, rec.ID)
	if !ok {
		return nil, denied("chat %q is not a descendant of yours", id)
	}
	ids, err := m.st.Descendants(ctx, target.ID)
	if err != nil {
		return nil, err
	}
	n := m.cancelAgents(append(ids, target.ID), errCancelled)
	return map[string]any{"cancelled": n}, nil
}

// ---------------------------------------------------------- edges and grants
//
// Both edges and grants stay mutable only from a trusted human operator (the
// console's Graph view and its REST API, spec.md 7.2): there is no
// switchboard.* self-service any more. A parent/child edge is still opened
// automatically on spawn (A8), which is all switchboard.chat.send needs.

func (m *Manager) afterGrant(g store.Grant, revoked []store.Grant) {
	m.emit("grant_changed", map[string]any{"agentId": g.AgentID, "label": g.Label, "project": g.Project, "server": g.Server, "allowed": g.Allowed, "source": g.Source, "revoked": len(revoked)})
	m.publish(events.Event{Type: events.TypeGraph})
	m.publish(events.Event{Type: events.TypeAgent, AgentID: g.AgentID})
}

// toolListTools lists the tools the caller may call right now, grouped by
// server, named exactly as the caller would compose them (resolveAgentTool's
// inverse): a chat pinned to one client drops the redundant label (naming.go).
func (m *Manager) toolListTools(ctx context.Context, cc *callCtx, _ map[string]any) (any, error) {
	grants, err := m.st.ListGrants(ctx, cc.agent.ID)
	if err != nil {
		return nil, err
	}
	rules := agentRules(grants)
	_, pinned := soleClient(grants)
	type row struct {
		Label, Project, Server string
		connected              bool
		tools                  []map[string]any
	}
	seen := map[registry.ServerRef]*row{}
	for _, e := range m.reg.IterServers() {
		ref := e.Channel.Ref()
		if ok, _ := Resolve(rules, ref); !ok {
			continue
		}
		r := seen[ref]
		if r == nil {
			r = &row{Label: ref.Label, Project: ref.Project, Server: ref.Server, tools: []map[string]any{}}
			seen[ref] = r
		}
		if e.Channel.Ready() {
			r.connected = true
			for _, ti := range e.Channel.Tools() {
				name, ok := agentToolName(pinned, ref.Label, ref.Project, ref.Server, ti.Name)
				if !ok {
					continue
				}
				r.tools = append(r.tools, map[string]any{"name": name, "description": ti.Description})
			}
		}
	}
	// Held-but-offline grants (A15) are listed too, as not connected.
	for _, g := range grants {
		if !g.Allowed || g.Label == "*" || g.Server == "*" || g.Project == "*" {
			continue
		}
		ref := registry.ServerRef{Label: g.Label, Project: g.Project, Server: g.Server}
		if ok, _ := Resolve(rules, ref); ok && seen[ref] == nil {
			seen[ref] = &row{Label: ref.Label, Project: ref.Project, Server: ref.Server, tools: []map[string]any{}}
		}
	}
	rows := make([]*row, 0, len(seen))
	for _, r := range seen {
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		return a.Label+"\x00"+a.Project+"\x00"+a.Server < b.Label+"\x00"+b.Project+"\x00"+b.Server
	})
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, map[string]any{"label": r.Label, "project": r.Project, "server": r.Server, "connected": r.connected, "tools": r.tools})
	}
	return out, nil
}
