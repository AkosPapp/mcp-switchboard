package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/config"
)

func sseServer(t *testing.T, events []string, capture *map[string]any, hdr *http.Header) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if capture != nil {
			_ = json.Unmarshal(body, capture)
		}
		if hdr != nil {
			*hdr = r.Header.Clone()
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range events {
			fmt.Fprint(w, e)
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestPriceCost(t *testing.T) {
	p := Price{InputMicrosPerMTok: 3_000_000, OutputMicrosPerMTok: 15_000_000, CacheReadMicrosPerMTok: 300_000, CacheWriteMicrosPerMTok: 3_750_000}
	got := p.Cost(Usage{InputTokens: 1000, OutputTokens: 500, CacheReadTokens: 2000, CacheWriteTokens: 100})
	// 3000+7500+600+375 = 11475 micros
	if got != 11475 {
		t.Errorf("cost = %d", got)
	}
	if p.Cost(Usage{}) != 0 {
		t.Error("zero usage must cost zero")
	}
	if c := (Price{InputMicrosPerMTok: 1}).Cost(Usage{InputTokens: 1}); c != 1 {
		t.Errorf("tiny usage rounds up, got %d", c)
	}
}

func TestRegistry(t *testing.T) {
	r := NewRegistry()
	fake := NewScripted("fake")
	r.AddModel(ModelSpec{Provider: "fake", Model: "m"})
	if len(r.Models()) != 0 {
		t.Error("models of unconfigured providers are not usable")
	}
	r.Register(fake)
	r.Register(NewAnthropic("k", ""))
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")
	os.WriteFile(path, []byte(`{"models":[
	  {"provider":"anthropic","model":"claude-x","prices":{"input_micros_per_mtok":3000000,"output_micros_per_mtok":15000000,"cache_read":300000,"cache_write":3750000}},
	  {"provider":"anthropic","model":"unpriced"},
	  {"provider":"openai","model":"gpt"}]}`), 0o600)
	if err := r.LoadModelsFile(path); err != nil {
		t.Fatal(err)
	}
	if n := len(r.Models()); n != 3 { // fake/m, claude-x, unpriced; openai unconfigured
		t.Errorf("usable models = %d", n)
	}
	spec, known, ok := r.Lookup("anthropic", "claude-x")
	if !ok || !known || spec.Prices.CacheReadMicrosPerMTok != 300000 || spec.Prices.CacheWriteMicrosPerMTok != 3750000 {
		t.Errorf("lookup: %+v %v %v", spec, known, ok)
	}
	if _, known, ok := r.Lookup("anthropic", "unpriced"); !ok || known {
		t.Error("unpriced model must be found but cost-unknown")
	}
	if c, k := (ModelSpec{}).Cost(Usage{InputTokens: 5}); c != 0 || k {
		t.Error("no price -> cost 0, unknown")
	}
	if _, _, ok := r.Lookup("openai", "gpt"); ok {
		t.Error("openai has no credentials")
	}
	if _, ok := r.Provider("fake"); !ok {
		t.Error("fake provider missing")
	}
	if err := r.LoadModelsJSON([]byte(`[{"provider":"","model":"x"}]`)); err == nil {
		t.Error("entry without provider must fail")
	}
}

func TestRegistryFromSettings(t *testing.T) {
	r, err := NewRegistryFromSettings(config.Settings{LLMAnthropicAPIKey: "k", LLMOpenAICompatibleBaseURL: "http://x/v1"})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(r.Providers(), ",")
	if got != "anthropic,openai-compatible" {
		t.Errorf("providers = %s", got)
	}
}

const anthText = `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":10,"cache_read_input_tokens":4,"cache_creation_input_tokens":2,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig1"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Hel"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"lo"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: content_block_start
data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"echo"}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"a\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"1}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":2}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":42}}

event: message_stop
data: {"type":"message_stop"}

`

func TestAnthropicStreamAndRequest(t *testing.T) {
	var body map[string]any
	var hdr http.Header
	srv := sseServer(t, []string{anthText}, &body, &hdr)
	p := NewAnthropic("secret-key", srv.URL)
	temp := 0.5
	msgs := []Message{
		TextMessage(RoleUser, "hi"),
		{Role: RoleAssistant, Content: []Block{{Type: BlockThinking, Text: "t", Signature: "s"}, {Type: BlockText, Text: "ok"}},
			ToolCalls: []ToolCall{{ID: "a", Name: "x", Arguments: map[string]any{"q": 1}}, {ID: "b", Name: "y"}}},
		{Role: RoleTool, ToolResults: []ToolResult{{ToolCallID: "a", Content: []Block{{Type: BlockText, Text: "r1"}}}}},
		{Role: RoleTool, ToolResults: []ToolResult{{ToolCallID: "b", Content: []Block{{Type: BlockText, Text: "r2"}}, IsError: true}}},
	}
	tools := []Tool{
		{Name: "echo", InputSchema: map[string]any{"type": "object"}},
		{Name: "bad", InputSchema: map[string]any{"type": "array"}},
		{Name: "dotted.name"},
	}
	ch, err := p.Complete(context.Background(), msgs, tools, Options{Model: "m", System: "sys", Temperature: &temp})
	if err != nil {
		t.Fatal(err)
	}
	r := Collect(ch)
	if r.Err != nil || r.FinishReason != FinishToolUse || r.Text != "Hello" || r.Thinking != "hmm" {
		t.Errorf("result: %+v", r)
	}
	if len(r.ToolCalls) != 1 || r.ToolCalls[0].ID != "toolu_1" || r.ToolCalls[0].Arguments["a"] != float64(1) {
		t.Errorf("tool calls: %+v", r.ToolCalls)
	}
	if r.Usage != (Usage{InputTokens: 10, OutputTokens: 42, CacheReadTokens: 4, CacheWriteTokens: 2}) {
		t.Errorf("usage: %+v", r.Usage)
	}
	if hdr.Get("X-Api-Key") != "secret-key" || hdr.Get("Anthropic-Version") == "" {
		t.Errorf("headers: %v", hdr)
	}
	if body["stream"] != true || body["system"] != "sys" || body["temperature"] != 0.5 || body["max_tokens"] == nil {
		t.Errorf("body: %v", body)
	}
	if n := len(body["tools"].([]any)); n != 1 {
		t.Errorf("only the valid tool should be sent, got %d", n)
	}
	ms := body["messages"].([]any)
	if len(ms) != 3 {
		t.Fatalf("tool results must merge into one user message, got %d messages", len(ms))
	}
	last := ms[2].(map[string]any)
	blocks := last["content"].([]any)
	if last["role"] != "user" || len(blocks) != 2 || blocks[0].(map[string]any)["type"] != "tool_result" ||
		blocks[1].(map[string]any)["is_error"] != true {
		t.Errorf("merged message: %v", last)
	}
	asst := ms[1].(map[string]any)["content"].([]any)
	if len(asst) != 4 || asst[0].(map[string]any)["type"] != "thinking" || asst[3].(map[string]any)["type"] != "tool_use" {
		t.Errorf("assistant blocks: %v", asst)
	}
}

func TestAnthropicThinkingDropsTemperature(t *testing.T) {
	var body map[string]any
	srv := sseServer(t, []string{"data: {\"type\":\"message_stop\"}\n\n"}, &body, nil)
	temp := 1.0
	ch, err := NewAnthropic("k", srv.URL).Complete(context.Background(), nil, nil,
		Options{Model: "m", Temperature: &temp, Thinking: &Thinking{Type: "enabled", BudgetTokens: 2000}})
	if err != nil {
		t.Fatal(err)
	}
	Collect(ch)
	if _, has := body["temperature"]; has || body["thinking"] == nil {
		t.Errorf("body: %v", body)
	}
}

// A model using interleaved extended thinking can think, call a tool, think
// again, then call a second tool, all within one turn. Replaying that turn
// content-then-calls (every thinking block before every tool_use) breaks
// Anthropic's requirement that a thinking block stay immediately before the
// tool_use it led to: the API rejects the next request, and the second tool
// call never gets to run again. Message.Content and Message.ToolCalls carry
// their original Delta.ContentIndex so the real order can be rebuilt.
func TestAnthropicPreservesInterleavedThinkingAndToolCallOrder(t *testing.T) {
	msg := Message{
		Role: RoleAssistant,
		Content: []Block{
			{Type: BlockThinking, Text: "first", Signature: "s1", Index: 0},
			{Type: BlockThinking, Text: "second", Signature: "s2", Index: 2},
		},
		ToolCalls: []ToolCall{
			{ID: "a", Name: "x", Index: 1},
			{ID: "b", Name: "y", Index: 3},
		},
	}
	blocks := anthAssistantBlocks(msg)
	if len(blocks) != 4 {
		t.Fatalf("blocks: %v", blocks)
	}
	kinds := make([]string, len(blocks))
	for i, b := range blocks {
		kinds[i] = b["type"].(string)
	}
	want := []string{"thinking", "tool_use", "thinking", "tool_use"}
	for i := range want {
		if kinds[i] != want[i] {
			t.Errorf("blocks in generation order = %v, want %v", kinds, want)
			break
		}
	}
	if blocks[0]["thinking"] != "first" || blocks[2]["thinking"] != "second" {
		t.Errorf("thinking text out of place: %v", blocks)
	}
	if blocks[1]["id"] != "a" || blocks[3]["id"] != "b" {
		t.Errorf("tool_use out of place: %v", blocks)
	}
}

func TestAnthropicErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"overloaded"}`, 529)
	}))
	defer srv.Close()
	_, err := NewAnthropic("k", srv.URL).Complete(context.Background(), nil, nil, Options{Model: "m"})
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != 529 || strings.Contains(err.Error(), "k\"") {
		t.Errorf("err = %v", err)
	}

	srv2 := sseServer(t, []string{"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"busy\"}}\n\n"}, nil, nil)
	ch, _ := NewAnthropic("k", srv2.URL).Complete(context.Background(), nil, nil, Options{Model: "m"})
	r := Collect(ch)
	if r.Err == nil || !strings.Contains(r.Err.Error(), "busy") || r.FinishReason != FinishError {
		t.Errorf("result: %+v", r)
	}

	srv3 := sseServer(t, []string{"data: {\"type\":\"message_start\",\"message\":{}}\n\n"}, nil, nil)
	ch, _ = NewAnthropic("k", srv3.URL).Complete(context.Background(), nil, nil, Options{Model: "m"})
	if r := Collect(ch); r.Err == nil {
		t.Error("truncated stream must be an error")
	}
}

const oaiStream = `data: {"choices":[{"delta":{"role":"assistant","content":"Hi "}}]}

data: {"choices":[{"delta":{"content":"there"}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"echo","arguments":"{\"a\""}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":":2}"}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"c2","function":{"name":"other","arguments":"{}"}}]}}]}

data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}

data: {"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":40}}}

data: [DONE]

`

func TestOpenAIStreamAndRequest(t *testing.T) {
	var body map[string]any
	var hdr http.Header
	srv := sseServer(t, []string{oaiStream}, &body, &hdr)
	p := NewOpenAI("oai-key", srv.URL)
	msgs := []Message{
		TextMessage(RoleUser, "hi"),
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "a", Name: "x", Arguments: map[string]any{"q": 1}}, {ID: "b", Name: "y"}}},
		{Role: RoleTool, ToolResults: []ToolResult{{ToolCallID: "a", Content: []Block{{Type: BlockText, Text: "r1"}}}}},
		{Role: RoleTool, ToolResults: []ToolResult{{ToolCallID: "b", Content: []Block{{Type: BlockText, Text: "r2"}}}}},
	}
	ch, err := p.Complete(context.Background(), msgs,
		[]Tool{{Name: "echo", Description: "d", InputSchema: map[string]any{"properties": map[string]any{}}}, {Name: "bad", InputSchema: map[string]any{"oneOf": []any{}}}},
		Options{Model: "gpt", MaxTokens: 99, System: "sys"})
	if err != nil {
		t.Fatal(err)
	}
	r := Collect(ch)
	if r.Err != nil || r.Text != "Hi there" || r.FinishReason != FinishToolUse {
		t.Errorf("result: %+v", r)
	}
	if len(r.ToolCalls) != 2 || r.ToolCalls[0].Arguments["a"] != float64(2) || r.ToolCalls[1].Name != "other" {
		t.Errorf("calls: %+v", r.ToolCalls)
	}
	if r.Usage != (Usage{InputTokens: 60, OutputTokens: 7, CacheReadTokens: 40}) {
		t.Errorf("usage: %+v", r.Usage)
	}
	if hdr.Get("Authorization") != "Bearer oai-key" {
		t.Error("missing bearer")
	}
	if body["max_completion_tokens"] != float64(99) || body["stream_options"] == nil {
		t.Errorf("body: %v", body)
	}
	ms := body["messages"].([]any)
	roles := []string{}
	for _, m := range ms {
		roles = append(roles, m.(map[string]any)["role"].(string))
	}
	if strings.Join(roles, ",") != "system,user,assistant,tool,tool" {
		t.Errorf("roles: %v", roles)
	}
	if len(body["tools"].([]any)) != 1 {
		t.Errorf("tools: %v", body["tools"])
	}
	fn := body["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if fn["parameters"].(map[string]any)["type"] != "object" {
		t.Errorf("schema should be normalised: %v", fn)
	}
}

func TestOpenAICompatible(t *testing.T) {
	var body map[string]any
	var hdr http.Header
	srv := sseServer(t, []string{
		"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"think\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"length\"}]}\n\n",
	}, &body, &hdr)
	p := NewOpenAICompatible("", srv.URL)
	if p.Name() != "openai-compatible" {
		t.Error(p.Name())
	}
	ch, _ := p.Complete(context.Background(), []Message{TextMessage(RoleUser, "x")}, nil, Options{Model: "llama", MaxTokens: 5})
	r := Collect(ch)
	if r.Err != nil || r.Text != "ok" || r.Thinking != "think" || r.FinishReason != FinishMaxTokens {
		t.Errorf("result: %+v", r)
	}
	if hdr.Get("Authorization") != "" || body["max_tokens"] != float64(5) {
		t.Errorf("hdr=%v body=%v", hdr, body)
	}
}

func TestOpenAIBadArgsKept(t *testing.T) {
	srv := sseServer(t, []string{
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c\",\"function\":{\"name\":\"t\",\"arguments\":\"{\\\"a\"}}]},\"finish_reason\":\"length\"}]}\n\n",
		"data: [DONE]\n\n"}, nil, nil)
	ch, _ := NewOpenAI("k", srv.URL).Complete(context.Background(), nil, nil, Options{Model: "m"})
	r := Collect(ch)
	if len(r.ToolCalls) != 1 || r.ToolCalls[0].Arguments["_raw_arguments"] == nil {
		t.Errorf("%+v", r)
	}
}

func TestCancellation(t *testing.T) {
	for name, mk := range map[string]func(url string) Provider{
		"anthropic": func(u string) Provider { return NewAnthropic("k", u) },
		"openai":    func(u string) Provider { return NewOpenAI("k", u) },
	} {
		t.Run(name, func(t *testing.T) {
			released := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if name == "anthropic" {
					fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{}}\n\n")
				} else {
					fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
				}
				w.(http.Flusher).Flush()
				<-r.Context().Done() // the client aborting must reach the server
				close(released)
			}))
			defer srv.Close()
			ctx, cancel := context.WithCancel(context.Background())
			ch, err := mk(srv.URL).Complete(ctx, []Message{TextMessage(RoleUser, "x")}, nil, Options{Model: "m"})
			if err != nil {
				t.Fatal(err)
			}
			cancel()
			select {
			case <-released:
			case <-time.After(3 * time.Second):
				t.Fatal("request was not aborted")
			}
			done := make(chan struct{})
			go func() {
				for range ch {
				}
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("channel not closed")
			}
		})
	}
}

func TestScripted(t *testing.T) {
	s := NewScripted("fake",
		CallTool("echo", map[string]any{"x": 1}).WithText("calling").WithCall("b", nil),
		Say("done").WithUsage(3, 4),
		Turn{Finish: FinishMaxTokens, Text: "cut", NoUsage: true},
		Fail(errors.New("boom")),
	)
	ctx := context.Background()
	ch, _ := s.Complete(ctx, []Message{TextMessage(RoleUser, "hello world")}, nil, Options{Model: "m"})
	r := Collect(ch)
	if r.Text != "calling" || len(r.ToolCalls) != 2 || r.ToolCalls[0].ID != "call_0_0" || r.FinishReason != FinishToolUse || r.Usage.InputTokens == 0 {
		t.Errorf("%+v", r)
	}
	ch, _ = s.Complete(ctx, nil, nil, Options{})
	if r := Collect(ch); r.Text != "done" || r.Usage != (Usage{InputTokens: 3, OutputTokens: 4}) || r.FinishReason != FinishStop {
		t.Errorf("%+v", r)
	}
	ch, _ = s.Complete(ctx, nil, nil, Options{})
	if r := Collect(ch); r.FinishReason != FinishMaxTokens || r.Usage != (Usage{}) {
		t.Errorf("%+v", r)
	}
	ch, _ = s.Complete(ctx, nil, nil, Options{})
	if r := Collect(ch); r.Err == nil || r.FinishReason != FinishError {
		t.Errorf("%+v", r)
	}
	if _, err := s.Complete(ctx, nil, nil, Options{}); err == nil {
		t.Error("exhausted script must error")
	}
	if s.CallCount() != 5 || s.Calls()[0].Options.Model != "m" {
		t.Error("calls not recorded")
	}
}

func TestScriptedFuncAndDelay(t *testing.T) {
	s := NewScriptedFunc("f", func(call int, msgs []Message) Turn {
		return Say(fmt.Sprintf("call %d saw %d msgs", call, len(msgs)))
	})
	ch, _ := s.Complete(context.Background(), []Message{TextMessage(RoleUser, "a")}, nil, Options{})
	if r := Collect(ch); r.Text != "call 0 saw 1 msgs" {
		t.Error(r.Text)
	}

	s.SetDelay(func(int) time.Duration { return time.Hour })
	ctx, cancel := context.WithCancel(context.Background())
	ch, _ = s.Complete(ctx, nil, nil, Options{})
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("cancelled delayed turn must emit nothing")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("channel not closed on cancel")
	}
}

func TestScriptedConcurrent(t *testing.T) {
	s := NewScriptedFunc("f", func(int, []Message) Turn { return Say("x") })
	done := make(chan struct{})
	for i := 0; i < 20; i++ {
		go func() {
			ch, _ := s.Complete(context.Background(), nil, nil, Options{})
			Collect(ch)
			done <- struct{}{}
		}()
	}
	for i := 0; i < 20; i++ {
		<-done
	}
	if s.CallCount() != 20 {
		t.Error(s.CallCount())
	}
}

func TestScriptedRegistersAsProvider(t *testing.T) {
	r := NewRegistry()
	s := NewScripted("alice")
	r.Register(s, s.Models()...)
	if _, _, ok := r.Lookup("alice", "fake"); !ok {
		t.Error("fake model should be usable")
	}
}
