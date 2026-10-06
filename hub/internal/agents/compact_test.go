package agents

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/llm"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

func TestMergeModelConfigLayering(t *testing.T) {
	mc := ModelConfig{Provider: "openrouter", Model: "anthropic/claude", MaxTokens: 4096}
	mergeModelConfig(&mc, json.RawMessage(`{"model":"qwen-flash","effort":"high"}`))
	if mc.Provider != "openrouter" || mc.Model != "qwen-flash" || mc.MaxTokens != 4096 || mc.Effort != "high" {
		t.Fatalf("merge = %+v", mc)
	}
	// A per-message override wins over the chat preference (same function,
	// applied last) and unparseable or null layers are ignored.
	mergeModelConfig(&mc, json.RawMessage("{"))
	mergeModelConfig(&mc, json.RawMessage("null"))
	if mc.Model != "qwen-flash" {
		t.Fatalf("bad layer changed the model: %+v", mc)
	}
	mergeModelConfig(&mc, json.RawMessage(`{"model":"haiku"}`))
	if mc.Model != "haiku" || mc.Effort != "high" {
		t.Fatalf("override = %+v", mc)
	}
}

func TestEffectiveModelConfigChatEffort(t *testing.T) {
	m := &Manager{}
	agent := &store.Agent{Model: json.RawMessage(`{"provider":"ollama","model":"gpt-oss"}`)}
	chat := &store.Chat{ModelPref: json.RawMessage(`{"provider":"ollama","model":"qwen3"}`), Effort: "low"}
	mc := m.effectiveModelConfig(t.Context(), chat, agent, nil)
	if mc.Provider != "ollama" || mc.Model != "qwen3" || mc.Effort != "low" {
		t.Fatalf("effective = %+v", mc)
	}
	// A per-message effort beats the chat's.
	rs := &runState{sub: submission{modelOverride: json.RawMessage(`{"effort":"none"}`)}}
	mc = m.effectiveModelConfig(t.Context(), chat, agent, rs)
	if mc.Effort != "none" {
		t.Fatalf("override effort = %+v", mc)
	}
}

func TestEffectiveWindowTakesTheSmallerCap(t *testing.T) {
	spec := llm.ModelSpec{ContextWindow: 32768}
	if w := effectiveWindow(&store.Chat{}, spec); w != 32768 {
		t.Fatalf("no limit = %d", w)
	}
	if w := effectiveWindow(&store.Chat{ContextLimit: 8192}, spec); w != 8192 {
		t.Fatalf("chat limit = %d", w)
	}
	if w := effectiveWindow(&store.Chat{ContextLimit: 1_000_000}, spec); w != 32768 {
		t.Fatalf("limit above the window = %d", w)
	}
	if w := effectiveWindow(&store.Chat{ContextLimit: 4096}, llm.ModelSpec{}); w != 4096 {
		t.Fatalf("unknown window = %d", w)
	}
}

func TestApplyChatSummary(t *testing.T) {
	path := make([]store.Message, 5)
	for i := range path {
		path[i] = store.Message{ID: string(rune('m' + rune(i))), Role: store.RoleUser}
	}
	chat := &store.Chat{ID: "c", Summary: "the gist", SummarizeUptoMsg: "o"} // up to the 2nd message
	got := applyChatSummary(path, chat)
	if len(got) != 3 || got[0].Role != store.RoleUser || !strings.Contains(string(got[0].Content), "the gist") {
		t.Fatalf("dropping summary = %d msgs first=%q", len(got), got[0].Content)
	}
	if got[1].ID != "p" || got[2].ID != "q" {
		t.Fatalf("tail after the marker = %v", got)
	}
	// The marker is off this branch: keep everything, prepend the summary.
	chat.SummarizeUptoMsg = "zz"
	got = applyChatSummary(path, chat)
	if len(got) != 6 || got[1].ID != "m" {
		t.Fatalf("conservative summary = %d first kept %v", len(got), got[1].ID)
	}
	// No summary: the path comes back untouched.
	chat.Summary = ""
	if got := applyChatSummary(path, chat); len(got) != 5 {
		t.Fatalf("no summary changed the path: %d", len(got))
	}
}

func TestCompactCommandMatching(t *testing.T) {
	for text, want := range map[string]bool{
		"/compact":        true,
		"  /compact  \n":  true,
		"please /compact": false,
		"/compact now":    false,
		"/Compact":        false,
		"":                false,
	} {
		if got := compactCommand(text); got != want {
			t.Errorf("compactCommand(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestEstimateTokensGrowsWithThePrompt(t *testing.T) {
	base := estimateTokens("system", nil, []llm.Message{{Role: "user", Content: mustBlocks(t, "hello")}})
	bigger := estimateTokens("system", nil, []llm.Message{
		{Role: "user", Content: mustBlocks(t, "hello")},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{Name: "big_tool", Arguments: map[string]any{"q": strings.Repeat("x", 400)}}}},
	})
	if !(bigger > base) {
		t.Fatalf("estimate %d should exceed %d", bigger, base)
	}
}

func mustBlocks(t *testing.T, text string) []llm.Block {
	t.Helper()
	return []llm.Block{{Type: llm.BlockText, Text: text}}
}
