package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

func TestMarkdownExportLabelsInjectedMessages(t *testing.T) {
	human := store.Message{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"hi"}]`)}
	injected := store.Message{
		Role:    "user",
		Content: json.RawMessage(`[{"type":"text","text":"found it"}]`),
		Sender:  json.RawMessage(`{"chatId":"c1","chatTitle":"researcher","kind":"reply"}`),
	}
	got := renderMarkdown(store.Chat{Title: "t"}, []store.Message{human, injected})
	if !strings.Contains(got, "## User\n") {
		t.Errorf("a human message keeps the User heading:\n%s", got)
	}
	if !strings.Contains(got, `## Reply from chat "researcher"`) {
		t.Errorf("an injected message names its sender:\n%s", got)
	}
}
