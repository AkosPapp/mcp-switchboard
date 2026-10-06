// Model and preference resolution shared by the turn loop and the compactor
// (I2: profile model = default, chat choice wins, message override wins over
// that; I12: effort rides the same resolution).
package agents

import (
	"context"
	"encoding/json"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

// chatForRun returns the chat a run belongs to (nil for runs without one —
// an agent reached over /mcp with no chat). Read fresh at each use, so a
// preference toggled in the console applies from the next turn/call.
func (m *Manager) chatForRun(ctx context.Context, rs *runState) *store.Chat {
	if rs == nil || rs.chatID == "" {
		return nil
	}
	c, err := m.st.GetChat(ctx, rs.chatID)
	if err != nil || c == nil {
		return nil
	}
	return c
}

// mergeModelConfig overlays the non-empty fields of raw (a model JSON, as
// stored in chats.model_pref or carried by a message) onto mc. Invalid or
// empty input is ignored — a bad preference must not break the turn; the
// agent's config still runs.
func mergeModelConfig(mc *ModelConfig, raw json.RawMessage) {
	if len(raw) == 0 || string(raw) == "null" {
		return
	}
	o, err := parseModel(raw)
	if err != nil {
		return
	}
	if o.Provider != "" {
		mc.Provider = o.Provider
	}
	if o.Model != "" {
		mc.Model = o.Model
	}
	if o.Temperature != nil {
		mc.Temperature = o.Temperature
	}
	if o.MaxTokens > 0 {
		mc.MaxTokens = o.MaxTokens
	}
	if o.Thinking != nil {
		mc.Thinking = o.Thinking
	}
	if o.Effort != "" {
		mc.Effort = o.Effort
	}
}

// effectiveModelConfig resolves the model a chat's turn will use: the agent's
// (profile-resolved) config, with the chat's preference and the run's
// per-message override layered on, and the chat's effort on top of that.
// Unparseable layers are skipped rather than failing the caller.
func (m *Manager) effectiveModelConfig(ctx context.Context, chat *store.Chat, agent *store.Agent, rs *runState) ModelConfig {
	mc, err := parseModel(agent.Model)
	if err != nil {
		return ModelConfig{}
	}
	if chat != nil {
		mergeModelConfig(&mc, chat.ModelPref)
	}
	if rs != nil && len(rs.sub.modelOverride) > 0 {
		mergeModelConfig(&mc, rs.sub.modelOverride)
	}
	if chat != nil && chat.Effort != "" && mc.Effort == "" {
		mc.Effort = chat.Effort
	}
	return mc
}

// contextTokensLocked is what the context meter shows as "used": the last
// turn's prompt size when the provider reported one, else the hub's own
// estimate of the last built prompt (I5 — silent providers must still show a
// live meter). Callers hold rs.usageMu.
func (rs *runState) contextTokensLocked() int64 {
	if rs.lastPrompt > 0 {
		return int64(rs.lastPrompt)
	}
	return rs.estPrompt
}
