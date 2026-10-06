package llm

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

var textCallTools = []Tool{
	{Name: "file_read", InputSchema: map[string]any{"type": "object", "properties": map[string]any{
		"path": map[string]any{"type": "string"}, "offset": map[string]any{"type": "integer"}, "binary": map[string]any{"type": "boolean"}}}},
	{Name: "switchboard_todo_write", InputSchema: map[string]any{"type": "object", "properties": map[string]any{
		"todos": map[string]any{"type": "array"}}}},
	{Name: "run_command"},
}

// The exact text qwen3.5 produced inside its reasoning block in the chat that
// prompted this fallback.
const qwenThinking = "Let me explore the main directories to understand the structure better. I'll look at the README.md first.\n\n<tool_call>\n<function=file_read>\n<parameter=path>\nREADME.md\n</parameter>\n</function>\n</tool_call>"

func TestRecoverQwenXMLFromThinking(t *testing.T) {
	calls := recoverTextToolCalls("", qwenThinking, textCallTools)
	if len(calls) != 1 || calls[0].Name != "file_read" || calls[0].Arguments["path"] != "README.md" {
		t.Fatalf("calls = %+v", calls)
	}
	if calls[0].ID == "" {
		t.Error("recovered call has no id")
	}
}

func TestRecoverPrefersVisibleTextOverThinking(t *testing.T) {
	text := "<tool_call><function=run_command><parameter=command>ls</parameter></function></tool_call>"
	calls := recoverTextToolCalls(text, qwenThinking, textCallTools)
	if len(calls) != 1 || calls[0].Name != "run_command" {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestRecoverTypesFollowTheSchema(t *testing.T) {
	src := "<tool_call><function=file_read><parameter=path>123</parameter><parameter=offset>40</parameter><parameter=binary>true</parameter></function></tool_call>"
	c := recoverTextToolCalls(src, "", textCallTools)
	if len(c) != 1 {
		t.Fatalf("calls = %+v", c)
	}
	if c[0].Arguments["path"] != "123" { // declared string stays a string
		t.Errorf("path = %#v", c[0].Arguments["path"])
	}
	if c[0].Arguments["offset"] != float64(40) || c[0].Arguments["binary"] != true {
		t.Errorf("args = %#v", c[0].Arguments)
	}
}

func TestRecoverJSONValueAndDottedNames(t *testing.T) {
	src := `<tool_call><function=switchboard.todo.write><parameter=todos>[{"content":"a","status":"pending"}]</parameter></function></tool_call>`
	c := recoverTextToolCalls(src, "", textCallTools)
	if len(c) != 1 || c[0].Name != "switchboard_todo_write" {
		t.Fatalf("calls = %+v", c)
	}
	if arr, ok := c[0].Arguments["todos"].([]any); !ok || len(arr) != 1 {
		t.Errorf("todos = %#v", c[0].Arguments["todos"])
	}
}

func TestRecoverHermesJSON(t *testing.T) {
	src := `ok <tool_call>{"name": "file_read", "arguments": {"path": "a.txt"}}</tool_call>`
	c := recoverTextToolCalls(src, "", textCallTools)
	if len(c) != 1 || c[0].Name != "file_read" || c[0].Arguments["path"] != "a.txt" {
		t.Fatalf("calls = %+v", c)
	}
}

func TestRecoverMultipleAndUnterminated(t *testing.T) {
	src := "<tool_call><function=file_read><parameter=path>a</parameter></function></tool_call>\n" +
		"<tool_call><function=file_read><parameter=path>b"
	c := recoverTextToolCalls(src, "", textCallTools)
	if len(c) != 2 || c[0].Arguments["path"] != "a" || c[1].Arguments["path"] != "b" {
		t.Fatalf("calls = %+v", c)
	}
	if c[0].ID == c[1].ID {
		t.Error("ids must differ")
	}
}

func TestRecoverIgnoresUnofferedToolsAndProse(t *testing.T) {
	for _, src := range []string{
		"<tool_call><function=rm_rf><parameter=path>/</parameter></function></tool_call>",
		`<tool_call>{"name": "delete_everything", "arguments": {}}</tool_call>`,
		"The format is <function=NAME> with parameters, for example.",
		"just an answer",
		"",
	} {
		if c := recoverTextToolCalls(src, src, textCallTools); len(c) != 0 {
			t.Errorf("%q recovered %+v", src, c)
		}
	}
	if c := recoverTextToolCalls(qwenThinking, "", nil); c != nil {
		t.Errorf("no tools offered, recovered %+v", c)
	}
}

// End to end through the Ollama stream: reasoning holds the call, done_reason
// is stop, and the run must still see a tool call.
func TestOllamaStreamRecoversCallFromThinking(t *testing.T) {
	stream := `{"message":{"role":"assistant","thinking":"Let me look.\n\n<tool_call>\n<function=file_read>\n<parameter=path>\nREADME.md\n</parameter>\n</function>\n</tool_call>"},"done":false}` + "\n" +
		`{"message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","prompt_eval_count":5,"eval_count":7}` + "\n"
	o := &OpenAI{name: "ollama"}
	ch := make(chan Delta, 16)
	go func() {
		defer close(ch)
		o.streamOllama(emitter{context.Background(), ch}, strings.NewReader(stream), "m", 0, textCallTools)
	}()
	r := Collect(ch)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	if r.FinishReason != FinishToolUse || len(r.ToolCalls) != 1 || r.ToolCalls[0].Arguments["path"] != "README.md" {
		t.Fatalf("result = %+v", r)
	}
}

func TestOllamaStreamKeepsStructuredCallsOverText(t *testing.T) {
	stream := `{"message":{"role":"assistant","thinking":"<tool_call><function=run_command><parameter=command>ls</parameter></function></tool_call>","tool_calls":[{"function":{"name":"file_read","arguments":{"path":"x"}}}]},"done":false}` + "\n" +
		`{"message":{"role":"assistant","content":""},"done":true,"done_reason":"stop"}` + "\n"
	o := &OpenAI{name: "ollama"}
	ch := make(chan Delta, 16)
	go func() {
		defer close(ch)
		o.streamOllama(emitter{context.Background(), ch}, strings.NewReader(stream), "m", 0, textCallTools)
	}()
	r := Collect(ch)
	if len(r.ToolCalls) != 1 || r.ToolCalls[0].Name != "file_read" {
		t.Fatalf("calls = %+v", r.ToolCalls)
	}
}

func TestOpenAIStreamRecoversCallFromContent(t *testing.T) {
	sse := "data: " + `{"choices":[{"delta":{"content":"Checking. <tool_call><function=file_read><parameter=path>a.txt</parameter></function></tool_call>"}}]}` + "\n\n" +
		"data: " + `{"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	o := &OpenAI{name: "compat"}
	ch := make(chan Delta, 16)
	go func() {
		defer close(ch)
		o.stream(emitter{context.Background(), ch}, &http.Response{Body: io.NopCloser(strings.NewReader(sse))}, textCallTools)
	}()
	r := Collect(ch)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	if r.FinishReason != FinishToolUse || len(r.ToolCalls) != 1 || r.ToolCalls[0].Arguments["path"] != "a.txt" {
		t.Fatalf("result = %+v", r)
	}
}
