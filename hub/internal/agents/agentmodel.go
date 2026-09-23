package agents

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/events"
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
func (m *Manager) planTurn(ctx context.Context, eff *store.Agent, prof *store.Profile) (*turnPlan, error) {
	cat, err := m.buildCatalog(ctx, eff)
	if err != nil {
		return nil, err
	}
	system := systemPromptFor(eff)
	if listing := skillsPrompt(m.autoSkills(ctx)); listing != "" {
		if system != "" {
			system += "\n\n"
		}
		system += listing
	}
	return &turnPlan{agent: eff, profile: prof, cat: cat, system: system}, nil
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
	_, agent, err := m.liveAgentForChat(ctx, chatID)
	if err != nil {
		return nil, err
	}
	eff, prof := m.resolveAgent(ctx, agent)
	plan, err := m.planTurn(ctx, eff, prof)
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
