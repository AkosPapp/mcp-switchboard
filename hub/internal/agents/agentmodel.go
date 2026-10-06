package agents

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/events"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/protocol"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

// The manually-created-agent model (docs/AGENT_MODEL_API.md): an agent picks a
// profile (live reference) or its own prompt, and one client or none.

// resolveAgent applies the live profile reference. While agent.ProfileID names
// an existing profile, the profile's current systemPrompt, model, capabilities,
// approval and budget are the agent's effective values: the returned agent is a
// copy carrying them, and they are written back onto the stored row so that
// deleting the profile later (which only NULLs the reference) leaves the agent
// with what it last had. Without a reference, or when the profile is gone, the
// agent is returned unchanged and the returned profile is nil.
//
// Sub-agents (origin "spawn") are the exception: they inherit the profile
// reference for display, but their prompt, model, capabilities, approval and
// budget are the values they were spawned with. A spawn call may narrow those
// (A12), and a live profile value would silently undo the narrowing.
func (m *Manager) resolveAgent(ctx context.Context, agent *store.Agent) (*store.Agent, *store.Profile) {
	if agent == nil || agent.ProfileID == nil || agent.Origin == store.OriginSpawn {
		return agent, nil
	}
	prof, err := m.st.GetProfile(ctx, *agent.ProfileID)
	if err != nil {
		m.log.Warn("could not read an agent's profile; using the agent's own values", "agent", agent.ID, "error", err)
		return agent, nil
	}
	if prof == nil {
		return agent, nil
	}
	return m.applyProfile(ctx, agent, prof), prof
}

func (m *Manager) applyProfile(ctx context.Context, agent *store.Agent, prof *store.Profile) *store.Agent {
	eff := *agent
	eff.SystemPrompt = prof.SystemPrompt
	eff.Model = prof.Model
	if len(eff.Model) == 0 || string(eff.Model) == "null" {
		eff.Model = m.firstConfiguredModel()
	}
	eff.Capabilities, eff.Approval = prof.Capabilities, prof.Approval
	eff.Budget = prof.Budget
	if len(eff.Budget) == 0 {
		eff.Budget = []byte("{}")
	}
	if eff.SystemPrompt != agent.SystemPrompt || !bytes.Equal(eff.Model, agent.Model) ||
		eff.Capabilities != agent.Capabilities || eff.Approval != agent.Approval ||
		!bytes.Equal(eff.Budget, agent.Budget) {
		caps := eff.Capabilities
		if _, err := m.st.UpdateAgent(ctx, agent.ID, store.AgentPatch{
			SystemPrompt: &eff.SystemPrompt, Model: eff.Model, Capabilities: &caps,
			Approval: &eff.Approval, Budget: eff.Budget,
		}); err != nil {
			m.log.Warn("could not copy a profile's values onto its agent", "agent", agent.ID, "error", err)
		} else {
			m.publish(events.Event{Type: events.TypeAgent, AgentID: agent.ID})
		}
	}
	return &eff
}

// syncProfileAgents copies an edited profile onto every live agent that
// references it, so stored agent JSON never lags the profile.
func (m *Manager) syncProfileAgents(ctx context.Context, prof *store.Profile) {
	all, err := m.st.ListAgents(ctx, store.AgentFilter{})
	if err != nil {
		return
	}
	for i := range all {
		if all[i].ProfileID != nil && *all[i].ProfileID == prof.ID && all[i].Origin != store.OriginSpawn {
			m.applyProfile(ctx, &all[i], prof)
		}
	}
}

// turnPlan is what one model turn is built from: the effective agent, its
// catalog and the system prompt. The run loop and the read-only endpoints
// (GET /api/chats/{id}/tools, .../system-prompt) all go through planTurn, so
// what they show is what the loop does.
type turnPlan struct {
	agent   *store.Agent // effective (live profile applied)
	profile *store.Profile
	cat     *catalog
	system  string
}

// planTurn builds the plan for an already resolved agent (see resolveAgent).
func (m *Manager) planTurn(ctx context.Context, eff *store.Agent, prof *store.Profile, chat *store.Chat) (*turnPlan, error) {
	cat, err := m.buildCatalog(ctx, eff, chat)
	if err != nil {
		return nil, err
	}
	system := systemPromptFor(eff)
	for _, injectable := range []string{
		skillsPrompt(m.autoSkills(ctx)), m.instructionsPrompt(cat), m.environmentBriefPrompt(cat), notePrompt(chat),
	} {
		if injectable == "" {
			continue
		}
		if system != "" {
			system += "\n\n"
		}
		system += injectable
	}
	return &turnPlan{agent: eff, profile: prof, cat: cat, system: system}, nil
}

// instructionsPrompt renders the instruction files the hub saw from the client
// host(s) whose tools this agent can actually reach this turn (P1-A: a repo's
// AGENTS.md is the highest-value content on the machine and was never loaded).
// All found files are injected — the client cannot predict which paths the
// agent will touch — ordered root-first per host, each labelled with its path,
// with the refine/conflict rule stated so nested files are not silent
// last-wins. The text goes through planTurn's documented extension point, so
// GET /api/chats/{id}/system-prompt shows exactly what the model saw.
func (m *Manager) instructionsPrompt(cat *catalog) string {
	wanted := map[string]bool{}
	for _, t := range cat.tools {
		if t.Ref.Label != "" {
			wanted[t.Ref.Label] = true
		}
	}
	if len(wanted) == 0 {
		return ""
	}
	type host struct {
		label string
		files []protocol.InstructionFile
	}
	var hosts []host
	for _, conn := range m.reg.Connections() {
		if !wanted[conn.Label] {
			continue
		}
		if files := conn.Instructions(); len(files) > 0 {
			hosts = append(hosts, host{conn.Label, files})
		}
	}
	if len(hosts) == 0 {
		return ""
	}
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].label < hosts[j].label })
	var b strings.Builder
	b.WriteString("## Instruction files (from the client host)\n\n" +
		"Text files the project carries as agent instructions (AGENTS.md, CLAUDE.md, ...), read from the " +
		"client host that serves the tools marked with that host below, verbatim, and re-read whenever they " +
		"change. Follow them as the user's own instructions for that repository. When several apply, they " +
		"are listed root-first and a file deeper in the tree refines the ones above it for work under its " +
		"directory: on a conflict, the deeper file wins for that subtree.\n")
	for _, h := range hosts {
		for _, f := range h.files {
			fmt.Fprintf(&b, "\n### %s:%s\n\n%s\n", h.label, f.Path, f.Content)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// briefStaleAfter is how old a client's environment brief may get before the
// hub marks it stale instead of letting the agent trust it silently: the
// client refreshes every ~15 s, so this is a generous line under "the client
// is gone and did not say goodbye".
const briefStaleAfter = 5 * time.Minute

// environmentBriefPrompt renders the W8 environment brief: what the agent
// needs to know about where its tools actually run before its first tool call.
// Hub facts come from config (only the hub knows the effective call deadline
// and push state); host facts come from each client whose tools this agent can
// reach, verbatim, stamped with their age — a brief older than
// briefStaleAfter is flagged, never silently trusted.
func (m *Manager) environmentBriefPrompt(cat *catalog) string {
	wanted := map[string]bool{}
	for _, t := range cat.tools {
		if t.Ref.Label != "" {
			wanted[t.Ref.Label] = true
		}
	}
	if len(wanted) == 0 {
		return ""
	}
	type host struct {
		label string
		brief string
		age   time.Duration
	}
	var hosts []host
	for _, conn := range m.reg.Connections() {
		if !wanted[conn.Label] {
			continue
		}
		brief, at := conn.Brief()
		if brief == "" {
			continue
		}
		hosts = append(hosts, host{conn.Label, brief, time.Since(at)})
	}
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].label < hosts[j].label })
	var b strings.Builder
	b.WriteString("## Environment brief\n\n" +
		"Where the tools in this list actually run. Collected by the client at connect and on every refresh " +
		"(never mid-question), and by the hub from its own config. Network reachability is NOT probed — " +
		"addresses and resolvers are local facts only. A stale brief is marked as such: treat it as a hint, " +
		"re-check cheaply before relying on it.\n")
	fmt.Fprintf(&b, "\n### hub\n\nHub tool-call deadline: %s (CALL_TIMEOUT) — a call that runs longer is cancelled by the hub; harness-side waits (wait_for_start/wait_for_poll) exist for longer work. User push notifications: ",
		formatSeconds(m.set.CallTimeout))
	if m.set.PushEnabled() {
		b.WriteString("configured.")
	} else {
		b.WriteString("not configured (a finished watch cannot reach the user unasked; use switchboard_user_ask in-turn).")
	}
	if len(hosts) == 0 {
		b.WriteString("\n\nNo client host brief was sent (older client, or brief disabled with --no-env-brief).")
	}
	for _, h := range hosts {
		fmt.Fprintf(&b, "\n### %s\n\n", h.label)
		b.WriteString(h.brief)
		if h.age > briefStaleAfter {
			fmt.Fprintf(&b, "\n\n[STALE: last refreshed %s ago — the client may be gone or the facts changed; re-verify before acting]", roundAge(h.age))
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatSeconds(s float64) string {
	if s == float64(int64(s)) {
		return fmt.Sprintf("%ds", int64(s))
	}
	return fmt.Sprintf("%.1fs", s)
}

func roundAge(d time.Duration) string {
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%.0fh", d.Hours())
	case d >= time.Minute:
		return fmt.Sprintf("%.0fm", d.Minutes())
	default:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
}

// systemPromptFor is THE function that produces the system prompt sent to the
// model for a turn. Today the hub adds nothing of its own: the text is exactly
// the effective agent's systemPrompt (the profile's, while a profile is
// referenced), verbatim - no sub-agent or tool notes are prepended or appended
// (tools travel as the request's tool list, not in the prompt). If the hub
// ever adds text, it goes here so GET /api/chats/{id}/system-prompt reports it.
func systemPromptFor(agent *store.Agent) string { return agent.SystemPrompt }

// ChatSystemPrompt implements Service.
func (m *Manager) ChatSystemPrompt(ctx context.Context, chatID string) (*SystemPromptView, error) {
	chat, agent, err := m.liveAgentForChat(ctx, chatID)
	if err != nil {
		return nil, err
	}
	eff, prof := m.resolveAgent(ctx, agent)
	plan, err := m.planTurn(ctx, eff, prof, chat)
	if err != nil {
		return nil, err
	}
	v := &SystemPromptView{SystemPrompt: plan.system, Source: "agent", ToolCount: len(plan.cat.tools)}
	switch {
	case plan.system == "":
		v.Source = "none"
	case prof != nil:
		v.Source = "profile"
	}
	if eff.ProfileID != nil {
		v.ProfileID = eff.ProfileID
	}
	if prof != nil {
		name := prof.Name
		v.ProfileName = &name
	} else if eff.ProfileID != nil {
		if p, err := m.st.GetProfile(ctx, *eff.ProfileID); err == nil && p != nil {
			name := p.Name
			v.ProfileName = &name
		}
	}
	if mc, err := parseModel(eff.Model); err == nil && (mc.Provider != "" || mc.Model != "") {
		v.Model = &ModelRef{Provider: mc.Provider, Model: mc.Model}
		if def, derr := parseModel(m.firstConfiguredModel()); derr == nil {
			v.ModelIsDefault = def.Provider == mc.Provider && def.Model == mc.Model
		}
	}
	return v, nil
}

// clientLabelOf is the one client a grant set is bound to (nil for none, for a
// wildcard, or for several clients).
func clientLabelOf(grants []store.Grant) *string {
	if l, ok := soleClient(grants); ok {
		return &l
	}
	return nil
}

func validClientLabel(l string) error {
	if strings.TrimSpace(l) == "" || l == store.Wildcard {
		return fmt.Errorf("%w: clientLabel must name exactly one client (not empty, not *)", ErrInvalid)
	}
	return nil
}
