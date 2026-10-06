package agents

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/llm"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

func text(s string) []llm.Block { return []llm.Block{{Type: llm.BlockText, Text: s}} }

func TestCapBlocksLeavesSmallAndUncappedResultsAlone(t *testing.T) {
	in := text(strings.Repeat("a", 100))
	if got := capBlocks(in, 100); got[0].Text != in[0].Text {
		t.Error("a result at the limit was changed")
	}
	big := text(strings.Repeat("a", 5000))
	if got := capBlocks(big, 0); got[0].Text != big[0].Text {
		t.Error("maxChars 0 must mean no cap")
	}
}

func TestCapBlocksKeepsHeadAndTailWithAMarker(t *testing.T) {
	src := "START-" + strings.Repeat("x", 10000) + "-END"
	in := text(src)
	got := capBlocks(in, 1000)
	s := got[0].Text
	if !strings.HasPrefix(s, "START-") || !strings.HasSuffix(s, "-END") {
		t.Errorf("head or tail lost: %q ... %q", s[:12], s[len(s)-8:])
	}
	if !strings.Contains(s, "characters omitted") || !strings.Contains(s, "context window") {
		t.Errorf("no marker: %s", s[:200])
	}
	if n := utf8.RuneCountInString(s); n > 1400 {
		t.Errorf("capped text is still %d runes", n)
	}
	if in[0].Text != src {
		t.Error("the input was modified")
	}
}

func TestCapBlocksIsRuneSafeAndSkipsImages(t *testing.T) {
	src := strings.Repeat("héllo wörld ", 2000)
	got := capBlocks([]llm.Block{{Type: llm.BlockText, Text: src}, {Type: llm.BlockImage, Data: "AAAA", MediaType: "image/png"}}, 500)
	if !utf8.ValidString(got[0].Text) {
		t.Error("cut inside a multi-byte character")
	}
	if got[1].Data != "AAAA" {
		t.Error("image block was touched")
	}
}

func TestCapBlocksSharesBudgetAcrossBlocks(t *testing.T) {
	got := capBlocks([]llm.Block{
		{Type: llm.BlockText, Text: strings.Repeat("a", 800)},
		{Type: llm.BlockText, Text: strings.Repeat("b", 5000)},
	}, 1000)
	if got[0].Text != strings.Repeat("a", 800) {
		t.Error("a block that fits within the budget was shortened")
	}
	if !strings.Contains(got[1].Text, "omitted") {
		t.Error("the overflowing block was not shortened")
	}
}

func TestToLLMMessagesCapsToolResultsButNotTheStoredOnes(t *testing.T) {
	huge := strings.Repeat("entry ", 20000)
	stored := mustJSON([]storedResult{{
		ToolCallID: "c1",
		Result:     map[string]any{"content": []any{map[string]any{"type": "text", "text": huge}}},
	}})
	path := []store.Message{
		{Role: store.RoleUser, Content: mustJSON(text("list the repo"))},
		{Role: store.RoleAssistant, Content: mustJSON([]llm.Block{}), ToolCalls: mustJSON([]llm.ToolCall{{ID: "c1", Name: "dir_list"}})},
		{Role: store.RoleTool, ToolResults: stored},
	}
	capped := toLLMMessages(path, 5000)
	got := blocksTextOf(capped[2].ToolResults[0].Content)
	if len(got) > 6000 || !strings.Contains(got, "characters omitted") {
		t.Errorf("model saw %d chars: %.80s", len(got), got)
	}
	if !strings.Contains(string(path[2].ToolResults), huge[:1000]) {
		t.Error("the stored result must stay whole")
	}
	whole := blocksTextOf(toLLMMessages(path, 0)[2].ToolResults[0].Content)
	if whole != huge {
		t.Errorf("cap 0 must pass the result through, got %d chars", len(whole))
	}
}

func blocksTextOf(bs []llm.Block) string {
	var out string
	for _, b := range bs {
		if b.Type == llm.BlockText {
			out += b.Text
		}
	}
	return out
}
