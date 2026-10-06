package llm

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"
)

func toolMsg(id, body string) Message {
	return Message{Role: RoleTool, ToolResults: []ToolResult{{ToolCallID: id, Content: []Block{{Type: BlockText, Text: body}}}}}
}

func callMsg(id, name string) Message {
	return Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: id, Name: name, Arguments: map[string]any{"path": "."}}}}
}

func resultText(m Message) string { return blocksText(m.ToolResults[0].Content) }

func TestFitToWindowLeavesAFittingPromptAlone(t *testing.T) {
	msgs := []Message{TextMessage(RoleUser, "hi"), callMsg("1", "fs_read"), toolMsg("1", "small")}
	got := fitToWindow(msgs, nil, "sys", 32768)
	if &got[0] != &msgs[0] {
		t.Error("a fitting prompt must be returned as is")
	}
	if fitToWindow(msgs, nil, "sys", 0)[2].ToolResults[0].Content[0].Text != "small" {
		t.Error("an unknown window must change nothing")
	}
}

// The reported failure: two 200 KB listings in a 32k window. The user message
// must survive, old results shrink first, and the caller's messages stay intact.
func TestFitToWindowShrinksOldResultsAndKeepsTheUserMessage(t *testing.T) {
	huge := strings.Repeat("entry ", 35000) // ~210k chars
	msgs := []Message{
		TextMessage(RoleUser, "read the structure of the repo"),
		callMsg("1", "dir_list"), toolMsg("1", huge),
		callMsg("2", "dir_list"), toolMsg("2", huge),
	}
	orig := resultText(msgs[2])
	got := fitToWindow(msgs, nil, "You are helpful.", 32768)

	if got[0].Text() != "read the structure of the repo" {
		t.Errorf("the user message changed: %q", got[0].Text())
	}
	if resultText(msgs[2]) != orig || resultText(msgs[4]) != orig {
		t.Fatal("the caller's messages were modified")
	}
	if n := utf8.RuneCountInString(resultText(got[2])); n > shrunkResultChars+200 {
		t.Errorf("the old result is still %d chars", n)
	}
	total := 0
	for _, m := range got {
		total += messageChars(m)
	}
	if total > 32768*85/100*charsPerToken {
		t.Errorf("the prompt is still %d chars, over the budget", total)
	}
	if !strings.Contains(resultText(got[4]), "omitted") {
		t.Error("the latest result should also have been shortened, it alone overflows")
	}
}

func TestFitToWindowSparesTheLatestResultWhenOldOnesSuffice(t *testing.T) {
	old := strings.Repeat("o", 100000)
	latest := strings.Repeat("n", 6000)
	msgs := []Message{
		TextMessage(RoleUser, "task"),
		callMsg("1", "f"), toolMsg("1", old),
		callMsg("2", "f"), toolMsg("2", latest),
	}
	got := fitToWindow(msgs, nil, "", 32768)
	if resultText(got[4]) != latest {
		t.Error("the latest result was shortened although shrinking the old one was enough")
	}
	if !strings.Contains(resultText(got[2]), "omitted") {
		t.Error("the old result was not shortened")
	}
}

func TestOllamaRequestNeverLosesTheUserMessage(t *testing.T) {
	f, srv := newFakeOllama(t)
	p := NewOpenAICompatible("", srv.URL+"/v1")
	huge := strings.Repeat("x", 300000)
	ch, err := p.Complete(context.Background(), []Message{
		TextMessage(RoleUser, "please list the repo"),
		callMsg("1", "fs_read"), toolMsg("1", huge),
	}, []Tool{{Name: "fs_read", InputSchema: map[string]any{"type": "object"}}}, Options{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	Collect(ch)
	msgs, _ := f.sent()["messages"].([]any)
	var sawUser bool
	var toolLen int
	for _, m := range msgs {
		mm := m.(map[string]any)
		if mm["role"] == "user" && mm["content"] == "please list the repo" {
			sawUser = true
		}
		if mm["role"] == "tool" {
			toolLen = len(mm["content"].(string))
		}
	}
	if !sawUser {
		t.Error("the request no longer contains the user message")
	}
	if toolLen == 0 || toolLen > 100000 {
		t.Errorf("the tool result sent was %d bytes; the 32k window cannot hold the original", toolLen)
	}
}

func TestHTTPErrorHintsAtContextOverflow(t *testing.T) {
	e := &HTTPError{Status: 400, Body: `{"error":"Jinja Exception: No user query found in messages."}`}
	if !strings.Contains(e.Error(), "LLM_OLLAMA_NUM_CTX") {
		t.Errorf("no hint: %s", e.Error())
	}
	plain := &HTTPError{Status: 500, Body: "boom"}
	if strings.Contains(plain.Error(), "hint") {
		t.Error("unrelated errors must not get the hint")
	}
}
