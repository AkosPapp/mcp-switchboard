package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/events"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

// Profiles (spec 5.2a, docs/PROFILES_API.md): reusable templates for the
// working agent a chat is created with. A profile is not an agent and carries
// no grants.

const (
	seedProfileName   = "Assistant"
	seedProfilePrompt = "You are a helpful assistant."
)

// seedProfiles creates the default Assistant profile when there are none.
func (m *Manager) seedProfiles(ctx context.Context) error {
	n, err := m.st.CountProfiles(ctx)
	if err != nil || n > 0 {
		return err
	}
	_, err = m.st.CreateProfile(ctx, store.Profile{
		Name: seedProfileName, SystemPrompt: seedProfilePrompt, Approval: store.ApprovalDestructive, IsDefault: true,
	})
	if err == nil {
		m.log.Info("seeded the default agent profile", "name", seedProfileName)
	}
	return err
}

func validApproval(a string) bool {
	return a == store.ApprovalNever || a == store.ApprovalDestructive || a == store.ApprovalAlways
}

// normModel validates a profile/chat model override: nil or "null" is "none".
func normModel(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("%w: model must be an object {provider, model}", ErrInvalid)
	}
	return raw, nil
}

func normBudget(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return json.RawMessage("{}"), nil
	}
	var obj map[string]float64
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("%w: budget must be an object of numbers", ErrInvalid)
	}
	return raw, nil
}

func (m *Manager) ListProfiles(ctx context.Context) ([]store.Profile, error) {
	return m.st.ListProfiles(ctx)
}

func (m *Manager) GetProfile(ctx context.Context, id string) (*store.Profile, error) {
	p, err := m.st.GetProfile(ctx, id)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, fmt.Errorf("%w: profile %s", ErrNotFound, id)
	}
	return p, nil
}

func (m *Manager) CreateProfile(ctx context.Context, in ProfileInput) (*store.Profile, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, fmt.Errorf("%w: name is required", ErrInvalid)
	}
	if strings.TrimSpace(in.SystemPrompt) == "" {
		return nil, fmt.Errorf("%w: systemPrompt is required", ErrInvalid)
	}
	approval := in.Approval
	if approval == "" {
		approval = store.ApprovalDestructive
	}
	if !validApproval(approval) {
		return nil, fmt.Errorf("%w: approval must be never, destructive or always", ErrInvalid)
	}
	model, err := normModel(in.Model)
	if err != nil {
		return nil, err
	}
	budget, err := normBudget(in.Budget)
	if err != nil {
		return nil, err
	}
	n, err := m.st.CountProfiles(ctx)
	if err != nil {
		return nil, err
	}
	p, err := m.st.CreateProfile(ctx, store.Profile{
		Name: name, Description: in.Description, SystemPrompt: in.SystemPrompt, Model: model,
		Capabilities: in.Capabilities, Approval: approval, Budget: budget, IsDefault: n == 0,
	})
	if err != nil {
		return nil, err
	}
	m.publish(events.Event{Type: events.TypeProfile})
	return &p, nil
}

func (m *Manager) UpdateProfile(ctx context.Context, id string, in ProfileUpdate) (*store.Profile, error) {
	cur, err := m.GetProfile(ctx, id)
	if err != nil {
		return nil, err
	}
	patch := store.ProfilePatch{Description: in.Description, SystemPrompt: in.SystemPrompt, Approval: in.Approval}
	if in.Name != nil {
		n := strings.TrimSpace(*in.Name)
		if n == "" {
			return nil, fmt.Errorf("%w: name must not be empty", ErrInvalid)
		}
		patch.Name = &n
	}
	if in.SystemPrompt != nil && strings.TrimSpace(*in.SystemPrompt) == "" {
		return nil, fmt.Errorf("%w: systemPrompt must not be empty", ErrInvalid)
	}
	if in.Approval != nil && !validApproval(*in.Approval) {
		return nil, fmt.Errorf("%w: approval must be never, destructive or always", ErrInvalid)
	}
	if len(in.Model) > 0 {
		model, err := normModel(in.Model)
		if err != nil {
			return nil, err
		}
		if model == nil {
			patch.ClearModel = true
		} else {
			patch.Model = model
		}
	}
	if len(in.Budget) > 0 {
		if patch.Budget, err = normBudget(in.Budget); err != nil {
			return nil, err
		}
	}
	if in.CanSpawn != nil || in.CanMessage != nil {
		caps := cur.Capabilities
		if in.CanSpawn != nil {
			caps.CanSpawn = *in.CanSpawn
		}
		if in.CanMessage != nil {
			caps.CanMessage = *in.CanMessage
		}
		patch.Capabilities = &caps
	}
	if in.IsDefault != nil && !*in.IsDefault && cur.IsDefault {
		return nil, fmt.Errorf("%w: a profile stops being the default only when another one is made the default", ErrInvalid)
	}
	p, err := m.st.UpdateProfile(ctx, id, patch)
	if err != nil {
		return nil, err
	}
	if in.IsDefault != nil && *in.IsDefault && !cur.IsDefault {
		if p, err = m.st.SetDefaultProfile(ctx, id); err != nil {
			return nil, err
		}
	}
	m.syncProfileAgents(ctx, &p) // agents referencing it follow the edit
	m.publish(events.Event{Type: events.TypeProfile})
	return &p, nil
}

func (m *Manager) DeleteProfile(ctx context.Context, id string) error {
	p, err := m.GetProfile(ctx, id)
	if err != nil {
		return err
	}
	n, err := m.st.CountProfiles(ctx)
	if err != nil {
		return err
	}
	switch {
	case n <= 1:
		return fmt.Errorf("%w: the only profile cannot be deleted", ErrConflict)
	case p.IsDefault:
		return fmt.Errorf("%w: the default profile cannot be deleted; make another profile the default first", ErrConflict)
	}
	if err := m.st.DeleteProfile(ctx, id); err != nil {
		return err
	}
	m.publish(events.Event{Type: events.TypeProfile})
	m.publish(events.Event{Type: events.TypeAgent})
	return nil
}

// ------------------------------------------------------------ chat creation

func (m *Manager) firstConfiguredModel() json.RawMessage {
	if ms := m.llm.Models(); len(ms) > 0 {
		return mustJSON(ModelConfig{Provider: ms[0].Provider, Model: ms[0].Model})
	}
	return json.RawMessage("{}")
}

func isNameTaken(err error) bool { return errors.Is(err, store.ErrNameTaken) }
