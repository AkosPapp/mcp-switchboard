package agents

import (
	"encoding/json"
	"fmt"
	"strings"

	"crypto/sha1"
	"encoding/hex"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/config"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/llm"
)

// ModelConfig is the agent's `model` column (spec.md 5.7).
type ModelConfig struct {
	Provider    string        `json:"provider"`
	Model       string        `json:"model"`
	Temperature *float64      `json:"temperature,omitempty"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Thinking    *llm.Thinking `json:"thinking,omitempty"`
}

// defaultMaxTokens is used when a model config names no max_tokens.
const defaultMaxTokens = 8192

func parseModel(raw json.RawMessage) (ModelConfig, error) {
	var mc ModelConfig
	if len(raw) == 0 || string(raw) == "null" {
		return mc, nil
	}
	if err := json.Unmarshal(raw, &mc); err != nil {
		return mc, fmt.Errorf("model is not a JSON object: %w", err)
	}
	return mc, nil
}

// options converts to provider options.
func (mc ModelConfig) options(system string) llm.Options {
	mt := mc.MaxTokens
	if mt <= 0 {
		mt = defaultMaxTokens
	}
	return llm.Options{Model: mc.Model, MaxTokens: mt, Temperature: mc.Temperature, Thinking: mc.Thinking, System: system}
}

// mergeBudget is "agent.budget merged with the hub-wide defaults, whichever is
// lower" (B4). Absent or non-positive agent keys leave the default; a
// non-positive default leaves the agent's value.
func mergeBudget(def config.AgentBudget, raw json.RawMessage) config.AgentBudget {
	var own config.AgentBudget
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &own)
	}
	lower := func(d, a int64) int64 {
		switch {
		case a <= 0:
			return d
		case d <= 0:
			return a
		}
		return min(d, a)
	}
	return config.AgentBudget{
		MaxTurns:              int(lower(int64(def.MaxTurns), int64(own.MaxTurns))),
		MaxToolCalls:          int(lower(int64(def.MaxToolCalls), int64(own.MaxToolCalls))),
		MaxTokens:             lower(def.MaxTokens, own.MaxTokens),
		MaxCostMicros:         lower(def.MaxCostMicros, own.MaxCostMicros),
		MaxWallSeconds:        int(lower(int64(def.MaxWallSeconds), int64(own.MaxWallSeconds))),
		MaxLifetimeCostMicros: lower(def.MaxLifetimeCostMicros, own.MaxLifetimeCostMicros),
	}
}

// providerName maps a tool name to one every provider accepts: no '.', only
// [A-Za-z0-9_-], at most 64 characters. Deterministic, so history and catalog
// always agree; collisions are detected per catalog.
func providerName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	s := b.String()
	if len(s) > 64 {
		sum := sha1.Sum([]byte(name))
		s = s[:55] + "_" + hex.EncodeToString(sum[:4])
	}
	return s
}

// blocksText concatenates text blocks.
func blocksText(bs []llm.Block) string {
	var sb strings.Builder
	for _, b := range bs {
		if b.Type == llm.BlockText {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

func textBlocks(s string) []llm.Block { return []llm.Block{{Type: llm.BlockText, Text: s}} }

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return b
}

func strPtr(s string) *string { return &s }
