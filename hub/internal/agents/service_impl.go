package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/events"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/llm"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/registry"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

// ------------------------------------------------------------------- agents

// CreateAgent implements Service (the /api/agents surface, kept for the Graph
// view and API callers). It also creates the record's first chat, a human chat
// (so the console has something to open; find it with ListChats{AgentID}); a
// child's chat nests under its parent's most recently active chat.
func (m *Manager) CreateAgent(ctx context.Context, in CreateAgentInput) (*store.Agent, error) {
	a, _, err := m.createAgent(ctx, in, false)
	return a, err
}

func (m *Manager) createAgent(ctx context.Context, in CreateAgentInput, spawned bool) (*store.Agent, *store.Chat, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, nil, fmt.Errorf("%w: name is required", ErrInvalid)
	}
	var parent *store.Agent
	if in.ParentID != "" {
		p, err := m.st.GetAgent(ctx, in.ParentID)
		if err != nil {
			return nil, nil, err
		}
		if p == nil || p.DeletedAt != nil {
			return nil, nil, fmt.Errorf("%w: parent agent %s", ErrNotFound, in.ParentID)
		}
		parent = p
		if m.set.AgentMaxDepth > 0 && p.Depth+1 > m.set.AgentMaxDepth {
			return nil, nil, fmt.Errorf("%w: depth %d would exceed AGENT_MAX_DEPTH=%d", ErrLimit, p.Depth+1, m.set.AgentMaxDepth)
		}
		n, err := m.st.CountLiveChildren(ctx, p.ID)
		if err != nil {
			return nil, nil, err
		}
		if m.set.AgentMaxChildren > 0 && n >= m.set.AgentMaxChildren {
			return nil, nil, fmt.Errorf("%w: agent already has %d live children (AGENT_MAX_CHILDREN)", ErrLimit, n)
		}
	}

	var prof *store.Profile
	if in.ProfileID != "" {
		p, err := m.GetProfile(ctx, in.ProfileID)
		if err != nil {
			return nil, nil, err
		}
		prof = p
	}
	if in.ClientSet && in.ClientLabel != nil {
		if err := validClientLabel(*in.ClientLabel); err != nil {
			return nil, nil, err
		}
	}
	// The initial copy of what a profile provides; resolveAgent keeps it live.
	if prof != nil {
		own := in.Model
		in.SystemPrompt, in.Model, in.Budget = prof.SystemPrompt, prof.Model, prof.Budget
		in.Capabilities, in.Approval = prof.Capabilities, prof.Approval
		if len(own) > 0 && string(own) != "null" {
			in.Model = own // an explicit model at creation beats the profile's initial copy
		}
	}

	model := in.Model
	if len(model) == 0 || string(model) == "null" {
		switch {
		case parent != nil && len(parent.Model) > 0 && prof == nil:
			model = parent.Model
		default:
			model = json.RawMessage("{}")
			if ms := m.llm.Models(); len(ms) > 0 {
				model = mustJSON(ModelConfig{Provider: ms[0].Provider, Model: ms[0].Model})
			}
		}
	}
	if _, err := parseModel(model); err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	budget := in.Budget
	if len(budget) == 0 {
		budget = json.RawMessage("{}")
	}

	var grants []store.Grant
	var clientGrants []store.Grant // manual agents: exactly {client,*,*}, or none
	if in.ClientSet {
		clientGrants = []store.Grant{}
		if in.ClientLabel != nil {
			clientGrants = append(clientGrants, store.Grant{Label: *in.ClientLabel, Project: store.Wildcard, Server: store.Wildcard, Allowed: true, Source: store.GrantExplicit})
		}
	}
	if parent == nil {
		if in.ClientSet {
			grants = clientGrants
		} else if in.Grants != nil {
			for _, g := range in.Grants {
				g.Source = store.GrantHuman
				grants = append(grants, g)
			}
		} else {
			for _, g := range m.set.AgentDefaultGrants {
				grants = append(grants, store.Grant{Label: g.Label, Project: g.Project, Server: g.Server, Allowed: true, Source: store.GrantExplicit})
			}
		}
	} else {
		pg, err := m.st.ListGrants(ctx, parent.ID)
		if err != nil {
			return nil, nil, err
		}
		req := in.Grants
		if in.ClientSet {
			req = clientGrants
		}
		grants = deriveChildGrants(pg, req) // A12
	}
	for _, g := range grants {
		if g.Label == "" || g.Server == "" {
			return nil, nil, fmt.Errorf("%w: a grant needs label and server (use * for any)", ErrInvalid)
		}
	}

	approval := in.Approval
	switch approval {
	case "":
		approval = store.ApprovalNever
		if holdsHarness(grants) {
			approval = store.ApprovalDestructive // W1, decided once, here
		}
	case store.ApprovalNever, store.ApprovalDestructive, store.ApprovalAlways:
	default:
		return nil, nil, fmt.Errorf("%w: approval must be never, destructive or always", ErrInvalid)
	}
	autoWake := true
	if in.AutoWake != nil {
		autoWake = *in.AutoWake
	}
	a := store.Agent{
		Name: name, Description: in.Description, Model: model, SystemPrompt: in.SystemPrompt,
		Budget: budget, Capabilities: in.Capabilities, Approval: approval, AutoWake: autoWake, Status: store.AgentIdle,
	}
	if in.ParentID != "" {
		a.ParentID = &in.ParentID
	}
	a.Origin = store.OriginChat // a root made without a client: the legacy kind
	switch {
	case parent != nil:
		a.Origin = store.OriginSpawn
	case in.ClientSet:
		a.Origin = store.OriginManual
	}
	if in.origin != "" {
		a.Origin = in.origin
	}
	a.ClientLabel = clientLabelOf(grants)
	switch {
	case prof != nil:
		a.ProfileID = &prof.ID
	case parent != nil:
		a.ProfileID = parent.ProfileID // sub-agents inherit the profile (5.5)
	}
	if in.Project != "" {
		p := in.Project
		a.Project = &p
	}
	// A8: store.CreateAgent opens the parent<->child edge in the same
	// transaction as the agent row, both directions being the same row (D14),
	// so chat.send works between them from the start without an explicit
	// set_edge call.
	created, err := m.st.CreateAgent(ctx, a, grants)
	if err != nil {
		return nil, nil, err
	}

	chat := store.Chat{AgentID: created.ID, Kind: store.ChatKindHuman, Title: in.ChatTitle, ProfileID: created.ProfileID, ClientLabel: created.ClientLabel}
	if parent != nil {
		parentChat := in.ParentChatID
		if spawned {
			chat.ProfileID, chat.ClientLabel = m.inheritedChatScope(ctx, parent, in.ParentChatID)
		}
		if parentChat == "" {
			if pc, err := m.primaryChat(ctx, parent, false); err == nil && pc != nil {
				parentChat = pc.ID
			}
		}
		if parentChat != "" {
			chat.ParentChatID = &parentChat
		}
	}
	// A manual agent gets no automatic chat: the console creates its chats
	// (POST /api/chats {agentId}), as many as the user wants.
	var c store.Chat
	if !in.ClientSet || in.withChat {
		var err error
		if c, err = m.st.CreateChat(ctx, chat); err != nil {
			return nil, nil, err
		}
	}
	if m.met != nil {
		m.met.CountSpawn(projectOf(&created))
	}
	m.syncAgentGauge()
	m.emit("agent_spawned", map[string]any{"agentId": created.ID, "parentAgentId": in.ParentID, "depth": created.Depth, "project": projectOf(&created), "spawnedByAgent": spawned})
	m.publish(events.Event{Type: events.TypeGraph})
	m.publish(events.Event{Type: events.TypeAgent, AgentID: created.ID})
	return &created, &c, nil
}

// inheritedChatScope is the (profile, client) a spawned child's chat records:
// those of the chat the spawn came from, else of the parent's oldest human chat
// that has a client. The client restriction itself is enforced by the grants
// (A12), not by this record.
func (m *Manager) inheritedChatScope(ctx context.Context, parent *store.Agent, chatID string) (profile, client *string) {
	if chatID != "" {
		if c, err := m.st.GetChat(ctx, chatID); err == nil && c != nil {
			return c.ProfileID, c.ClientLabel
		}
	}
	chats, err := m.st.ListChats(ctx, store.ChatFilter{AgentID: parent.ID, Kind: store.ChatKindHuman, Limit: store.MaxLimit})
	if err == nil {
		for i := len(chats) - 1; i >= 0; i-- { // oldest first
			if chats[i].ClientLabel != nil {
				return chats[i].ProfileID, chats[i].ClientLabel
			}
		}
	}
	return parent.ProfileID, nil
}

// UpdateAgent implements Service.
func (m *Manager) UpdateAgent(ctx context.Context, id string, patch store.AgentPatch) (*store.Agent, error) {
	if patch.Approval != nil {
		switch *patch.Approval {
		case store.ApprovalNever, store.ApprovalDestructive, store.ApprovalAlways:
		default:
			return nil, fmt.Errorf("%w: approval must be never, destructive or always", ErrInvalid)
		}
	}
	if len(patch.Model) > 0 {
		if _, err := parseModel(patch.Model); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
	}
	if patch.SetClient && patch.ClientLabel != nil {
		if err := validClientLabel(*patch.ClientLabel); err != nil {
			return nil, err
		}
	}
	if patch.SetProfile && patch.ProfileID != nil {
		if _, err := m.GetProfile(ctx, *patch.ProfileID); err != nil {
			return nil, err
		}
	}
	a, err := m.st.UpdateAgent(ctx, id, patch)
	if err != nil {
		return nil, err
	}
	out := &a
	if patch.SetProfile {
		out, _ = m.resolveAgent(ctx, out) // copy the new profile's values now
	}
	m.publish(events.Event{Type: events.TypeAgent, AgentID: id})
	m.publish(events.Event{Type: events.TypeGraph})
	return out, nil
}

// DeleteAgent implements Service (A1).
func (m *Manager) DeleteAgent(ctx context.Context, id string, hard bool) error {
	a, err := m.st.GetAgent(ctx, id)
	if err != nil {
		return err
	}
	if a == nil {
		return fmt.Errorf("%w: agent %s", ErrNotFound, id)
	}
	desc, err := m.st.Descendants(ctx, id)
	if err != nil {
		return err
	}
	m.cancelAgents(append(desc, id), errCancelled)
	if hard {
		_, err = m.st.HardDeleteAgent(ctx, id)
	} else {
		_, err = m.st.SoftDeleteAgent(ctx, id)
	}
	if err != nil {
		return err
	}
	m.syncAgentGauge()
	m.publish(events.Event{Type: events.TypeGraph})
	m.publish(events.Event{Type: events.TypeAgent, AgentID: id})
	return nil
}

// SetGrant implements Service: a human edit (A13, A14).
func (m *Manager) SetGrant(ctx context.Context, g store.Grant) ([]store.Grant, error) {
	if g.Label == "" || g.Server == "" {
		return nil, fmt.Errorf("%w: label and server are required (use * for any)", ErrInvalid)
	}
	if a, err := m.st.GetAgent(ctx, g.AgentID); err != nil {
		return nil, err
	} else if a == nil || a.DeletedAt != nil {
		return nil, fmt.Errorf("%w: agent %s", ErrNotFound, g.AgentID)
	}
	g.Source = store.GrantHuman
	revoked, err := m.st.SetGrant(ctx, g)
	if err != nil {
		return nil, err
	}
	m.afterGrant(g, revoked)
	return revoked, nil
}

// SetEdge implements Service.
func (m *Manager) SetEdge(ctx context.Context, from, to string, allowed bool) (*store.Edge, error) {
	for _, id := range []string{from, to} {
		a, err := m.st.GetAgent(ctx, id)
		if err != nil {
			return nil, err
		}
		if a == nil || a.DeletedAt != nil {
			return nil, fmt.Errorf("%w: agent %s", ErrNotFound, id)
		}
	}
	e, err := m.st.SetEdge(ctx, from, to, allowed)
	if err != nil {
		return nil, err
	}
	m.publish(events.Event{Type: events.TypeGraph})
	return &e, nil
}

// Graph implements Service.
func (m *Manager) Graph(ctx context.Context) (*GraphView, error) {
	agents, err := m.st.ListAgents(ctx, store.AgentFilter{})
	if err != nil {
		return nil, err
	}
	edges, err := m.st.ListEdges(ctx, store.EdgeFilter{})
	if err != nil {
		return nil, err
	}
	live := map[registry.ServerRef]*GraphServer{}
	var liveRefs []registry.ServerRef
	for _, e := range m.reg.IterServers() {
		ref := e.Channel.Ref()
		s := live[ref]
		if s == nil {
			s = &GraphServer{Label: ref.Label, Project: ref.Project, Server: ref.Server}
			live[ref] = s
			liveRefs = append(liveRefs, ref)
		}
		if e.Channel.Ready() {
			s.Connected = true
			s.ToolCount += len(e.Channel.Tools())
		}
	}
	view := &GraphView{Agents: []GraphAgent{}, Edges: edges, Grants: []GraphGrant{}, Servers: []GraphServer{}}
	if view.Edges == nil {
		view.Edges = []store.Edge{}
	}
	sort.Slice(liveRefs, func(i, j int) bool { return liveRefs[i].String() < liveRefs[j].String() })
	for _, ref := range liveRefs {
		view.Servers = append(view.Servers, *live[ref])
	}

	byID := map[string]store.Agent{}
	grantsOf := map[string][]store.Grant{}
	for _, a := range agents {
		byID[a.ID] = a
		gs, err := m.st.ListGrants(ctx, a.ID)
		if err != nil {
			return nil, err
		}
		grantsOf[a.ID] = gs
	}
	m.mu.Lock()
	current := map[string]string{}
	for id, runs := range m.byAgent {
		for _, rs := range runs {
			if rs.currentTool != "" {
				current[id] = rs.currentTool
			}
		}
	}
	m.mu.Unlock()
	for _, a := range agents {
		unread, _ := m.st.CountInbox(ctx, a.ID)
		view.Agents = append(view.Agents, GraphAgent{Agent: a, UnreadMail: unread, CurrentTool: current[a.ID]})
		for _, g := range grantsOf[a.ID] {
			gg := GraphGrant{Grant: g}
			pat := patOf(g)
			for _, ref := range liveRefs {
				if live[ref].Connected && (registry.ServerRef{Label: pat.Label, Project: pat.Project, Server: pat.Server}).Matches(ref) {
					gg.Connected = true
					break
				}
			}
			if g.Allowed && g.Source == store.GrantHuman && a.ParentID != nil {
				gg.Orphaned = true
				for _, pg := range grantsOf[*a.ParentID] {
					if pg.Allowed && covers(patOf(pg), pat) {
						gg.Orphaned = false
						break
					}
				}
			}
			view.Grants = append(view.Grants, gg)
		}
	}
	return view, nil
}

// ------------------------------------------------------------------- chats

func firstLine(s string, n int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if r := []rune(s); len(r) > n {
		s = string(r[:n]) + "…"
	}
	return s
}

func (m *Manager) liveAgentForChat(ctx context.Context, chatID string) (*store.Chat, *store.Agent, error) {
	chat, err := m.st.GetChat(ctx, chatID)
	if err != nil {
		return nil, nil, err
	}
	if chat == nil {
		return nil, nil, fmt.Errorf("%w: chat %s", ErrNotFound, chatID)
	}
	agent, err := m.st.GetAgent(ctx, chat.AgentID)
	if err != nil {
		return nil, nil, err
	}
	if agent == nil || agent.DeletedAt != nil {
		return nil, nil, fmt.Errorf("%w: the chat's agent is deleted", ErrNotFound)
	}
	return chat, agent, nil
}

// Post implements Service.
func (m *Manager) Post(ctx context.Context, chatID string, in PostInput) (*PostResult, error) {
	if len(in.Content) == 0 {
		return nil, fmt.Errorf("%w: content is required", ErrInvalid)
	}
	chat, agent, err := m.liveAgentForChat(ctx, chatID)
	if err != nil {
		return nil, err
	}
	runID := store.NewID()
	if in.IdempotencyKey != "" {
		existing, claimed, err := m.st.ClaimIdempotencyKey(ctx, in.IdempotencyKey, runID)
		if err != nil {
			return nil, err
		}
		if !claimed {
			// R5: hand back the original run.
			res := &PostResult{RunID: existing, Deduplicated: true}
			if path, err := m.st.ActivePath(ctx, chatID); err == nil {
				for _, msg := range path {
					if msg.RunID != nil && *msg.RunID == existing && msg.ParentID != nil {
						res.MessageID = *msg.ParentID
						break
					}
				}
			}
			return res, nil
		}
	}
	msg := &store.Message{ID: store.NewID(), ChatID: chatID, Role: store.RoleUser, Content: mustJSON(in.Content)}
	explicit := false
	if in.ParentID != "" {
		parent, err := m.st.GetMessage(ctx, in.ParentID)
		if err != nil {
			return nil, err
		}
		if parent == nil || parent.ChatID != chatID {
			return nil, fmt.Errorf("%w: parentId is not a message of this chat", ErrInvalid)
		}
		msg.ParentID, explicit = &in.ParentID, true
	}
	if chat.Title == "" || (chat.Title == defaultChatTitle && chat.ActiveLeafID == nil) {
		if t := firstLine(blocksText(in.Content), 80); t != "" {
			_, _ = m.st.UpdateChat(ctx, chatID, store.ChatPatch{Title: &t})
		}
	}
	run, err := m.submit(ctx, submission{
		agent: agent, chat: chat, trigger: store.TriggerHuman, msg: msg, parentExplicit: explicit,
		modelOverride: in.Model, runID: runID,
	})
	if err != nil {
		return nil, err
	}
	return &PostResult{MessageID: msg.ID, RunID: run.ID}, nil
}

// Branch implements Service (A4): no history is copied; the new node is a
// sibling and the leaf moves.
func (m *Manager) Branch(ctx context.Context, chatID string, in BranchInput) (*PostResult, error) {
	chat, agent, err := m.liveAgentForChat(ctx, chatID)
	if err != nil {
		return nil, err
	}
	from, err := m.st.GetMessage(ctx, in.FromMessageID)
	if err != nil {
		return nil, err
	}
	if from == nil || from.ChatID != chatID {
		return nil, fmt.Errorf("%w: fromMessageId is not a message of this chat", ErrInvalid)
	}
	m.mu.Lock()
	busy := m.chatActive[chatID] != nil
	m.mu.Unlock()
	if busy {
		return nil, fmt.Errorf("%w: a run is active on this chat; cancel it or wait", ErrInvalid)
	}
	switch {
	case from.Role == store.RoleUser && len(in.Content) > 0:
		// Edit and resend: a user sibling, then a run.
		msg := &store.Message{ID: store.NewID(), ChatID: chatID, ParentID: from.ParentID, Role: store.RoleUser, Content: mustJSON(in.Content)}
		run, err := m.submit(ctx, submission{agent: agent, chat: chat, trigger: store.TriggerHuman, msg: msg, parentExplicit: true, modelOverride: in.Model})
		if err != nil {
			return nil, err
		}
		return &PostResult{MessageID: msg.ID, RunID: run.ID}, nil
	case from.Role == store.RoleAssistant && len(in.Content) == 0:
		// Regenerate: the leaf moves to the assistant's parent; the run adds a sibling.
		if from.ParentID == nil {
			return nil, fmt.Errorf("%w: cannot regenerate a root message", ErrInvalid)
		}
		run, err := m.submit(ctx, submission{agent: agent, chat: chat, trigger: store.TriggerHuman, setLeaf: *from.ParentID, modelOverride: in.Model})
		if err != nil {
			return nil, err
		}
		return &PostResult{MessageID: from.ID, RunID: run.ID}, nil
	case from.Role == store.RoleUser:
		// Copy point: answer this user message again.
		run, err := m.submit(ctx, submission{agent: agent, chat: chat, trigger: store.TriggerHuman, setLeaf: from.ID, modelOverride: in.Model})
		if err != nil {
			return nil, err
		}
		return &PostResult{MessageID: from.ID, RunID: run.ID}, nil
	case from.Role == store.RoleAssistant:
		// Edited assistant text: a sibling only, no run.
		msg, err := m.st.AppendMessage(ctx, store.Message{ChatID: chatID, ParentID: from.ParentID, Role: store.RoleAssistant, Content: mustJSON(in.Content)})
		if err != nil {
			return nil, err
		}
		m.publish(events.Event{Type: events.TypeChat, ChatID: chatID})
		return &PostResult{MessageID: msg.ID}, nil
	}
	return nil, fmt.Errorf("%w: cannot branch from a %s message", ErrInvalid, from.Role)
}

// CancelRun implements Service (R3).
func (m *Manager) CancelRun(ctx context.Context, runID string) error {
	m.mu.Lock()
	rs := m.runs[runID]
	m.mu.Unlock()
	if rs != nil {
		m.cancelRunState(rs, errCancelled)
		return nil
	}
	run, err := m.st.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	if run == nil {
		return fmt.Errorf("%w: run %s", ErrNotFound, runID)
	}
	switch run.Status {
	case store.RunQueued, store.RunRunning, store.RunWaiting:
		// Not owned by this process (stale): just record it.
		st, fr, now := store.RunCancelled, "cancelled", time.Now().UTC()
		_, err = m.st.UpdateRun(ctx, runID, store.RunPatch{Status: &st, FinishReason: &fr, FinishedAt: &now})
	}
	return err
}

// Models implements Service.
func (m *Manager) Models() []llm.ModelSpec { return m.llm.Models() }

// RefreshModels triggers the lazy model-discovery refresh, waiting at most
// wait (the caller then serves whatever is cached).
func (m *Manager) RefreshModels(ctx context.Context, wait time.Duration) {
	m.llm.RefreshModels(ctx, wait)
}

// ActiveRuns implements Service: runs currently queued, running or waiting in this process.
func (m *Manager) ActiveRuns() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.runs)
}
