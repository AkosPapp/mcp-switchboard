package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/config"
)

type fakeOllama struct {
	mu       sync.Mutex
	chat     map[string]any
	chatBody string
	versions atomic.Int32
	tags     atomic.Int32
	down     atomic.Bool
	models   map[string]string // name -> show JSON
	ctxLen   int
	prompt   int
}

func (f *fakeOllama) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/version", func(w http.ResponseWriter, r *http.Request) {
		f.versions.Add(1)
		fmt.Fprint(w, `{"version":"0.9.0"}`)
	})
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		f.tags.Add(1)
		if f.down.Load() {
			http.Error(w, "boom", 500)
			return
		}
		var names []string
		for n := range f.models {
			names = append(names, fmt.Sprintf(`{"name":%q,"model":%q}`, n, n))
		}
		fmt.Fprintf(w, `{"models":[%s]}`, strings.Join(names, ","))
	})
	mux.HandleFunc("/api/show", func(w http.ResponseWriter, r *http.Request) {
		var b struct{ Model string }
		json.NewDecoder(r.Body).Decode(&b)
		if s, ok := f.models[b.Model]; ok {
			fmt.Fprint(w, s)
			return
		}
		fmt.Fprintf(w, `{"model_info":{"general.architecture":"llama","llama.context_length":%d},"capabilities":["completion","tools","thinking"]}`, f.ctxLen)
	})
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.chat = nil
		_ = json.Unmarshal(body, &f.chat)
		f.mu.Unlock()
		fmt.Fprint(w, `{"message":{"role":"assistant","thinking":"hm"},"done":false}`+"\n")
		fmt.Fprint(w, `{"message":{"role":"assistant","content":"hel"},"done":false}`+"\n")
		fmt.Fprint(w, `{"message":{"role":"assistant","content":"lo","tool_calls":[{"function":{"name":"fs_read","arguments":{"path":"/x"}}}]},"done":false}`+"\n")
		fmt.Fprintf(w, `{"message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","prompt_eval_count":%d,"eval_count":7}`+"\n", f.prompt)
	})
	return mux
}

func newFakeOllama(t *testing.T) (*fakeOllama, *httptest.Server) {
	f := &fakeOllama{ctxLen: 131072, prompt: 100, models: map[string]string{}}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeOllama) sent() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.chat
}

func ollamaChat(t *testing.T, p *OpenAI, opts Options) Result {
	t.Helper()
	opts.Model = "m"
	ch, err := p.Complete(context.Background(), []Message{TextMessage(RoleUser, "hi")}, []Tool{{Name: "fs_read", InputSchema: map[string]any{"type": "object"}}}, opts)
	if err != nil {
		t.Fatal(err)
	}
	return Collect(ch)
}

func numCtxSent(f *fakeOllama) any {
	return f.sent()["options"].(map[string]any)["num_ctx"]
}

func TestOllamaDetection(t *testing.T) {
	f, srv := newFakeOllama(t)
	p := NewOpenAICompatible("", srv.URL+"/v1")
	if !p.isOllama(context.Background()) || !p.isOllama(context.Background()) {
		t.Fatal("should detect ollama")
	}
	if f.versions.Load() != 1 {
		t.Errorf("detection must be cached, probes = %d", f.versions.Load())
	}
	// generic server: 404 on /api/version
	gen := httptest.NewServer(http.NotFoundHandler())
	defer gen.Close()
	if NewOpenAICompatible("", gen.URL+"/v1").isOllama(context.Background()) {
		t.Error("404 => generic")
	}
	// forced kinds do not probe
	if !NewOpenAICompatible("", gen.URL).WithOllama("ollama", 0).isOllama(context.Background()) {
		t.Error("forced ollama")
	}
	if NewOpenAICompatible("", srv.URL).WithOllama("generic", 0).isOllama(context.Background()) {
		t.Error("forced generic")
	}
	// unreachable: not cached
	dead := NewOpenAICompatible("", "http://127.0.0.1:1/v1")
	if dead.isOllama(context.Background()) || dead.ol.detected != 0 {
		t.Error("unreachable must not be cached")
	}
}

func TestOllamaNativeChat(t *testing.T) {
	f, srv := newFakeOllama(t)
	p := NewOpenAICompatible("", srv.URL+"/v1")
	temp := 0.5
	res := ollamaChat(t, p, Options{MaxTokens: 99, Temperature: &temp, Thinking: &Thinking{Type: "enabled"}, System: "sys"})
	if res.Err != nil || res.Text != "hello" || res.Thinking != "hm" || res.FinishReason != FinishToolUse {
		t.Fatalf("%+v", res)
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].ID != "call_0" || res.ToolCalls[0].Arguments["path"] != "/x" {
		t.Fatalf("tool calls: %+v", res.ToolCalls)
	}
	if res.Usage.InputTokens != 100 || res.Usage.OutputTokens != 7 || res.Usage.ContextWindow != 32768 || res.Usage.Truncated {
		t.Fatalf("usage %+v", res.Usage)
	}
	sent := f.sent()
	opt := sent["options"].(map[string]any)
	if opt["num_predict"] != 99.0 || opt["temperature"] != 0.5 || opt["num_ctx"] != 32768.0 || sent["think"] != true || sent["stream"] != true {
		t.Fatalf("sent %v", sent)
	}
	if len(sent["tools"].([]any)) != 1 || sent["messages"].([]any)[0].(map[string]any)["role"] != "system" {
		t.Fatalf("sent %v", sent)
	}
}

func TestOllamaMessageTranslation(t *testing.T) {
	msgs := []Message{
		{Role: RoleUser, Content: []Block{{Type: BlockText, Text: "look"}, {Type: BlockImage, MediaType: "image/png", Data: "QUJD"}}},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Name: "fs_read", Arguments: map[string]any{"a": 1}}}},
		{Role: RoleTool, ToolResults: []ToolResult{{ToolCallID: "c1", Content: []Block{{Type: BlockText, Text: "res"}}}}},
	}
	out := ollamaMessages(msgs, "")
	b, _ := json.Marshal(out)
	s := string(b)
	for _, want := range []string{`"images":["QUJD"]`, `"arguments":{"a":1}`, `"role":"tool"`, `"tool_name":"fs_read"`, `"content":"res"`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s in %s", want, s)
		}
	}
}

func TestOllamaNumCtx(t *testing.T) {
	f, srv := newFakeOllama(t)
	// auto: capped at 32768
	ollamaChat(t, NewOpenAICompatible("", srv.URL+"/v1"), Options{})
	if numCtxSent(f) != 32768.0 {
		t.Errorf("auto cap: %v", numCtxSent(f))
	}
	// auto: below the cap uses the model maximum
	f.ctxLen = 8192
	ollamaChat(t, NewOpenAICompatible("", srv.URL+"/v1"), Options{})
	if numCtxSent(f) != 8192.0 {
		t.Errorf("auto small: %v", numCtxSent(f))
	}
	// setting
	p := NewOpenAICompatible("", srv.URL+"/v1").WithOllama("auto", 16000)
	ollamaChat(t, p, Options{})
	if numCtxSent(f) != 16000.0 {
		t.Errorf("setting: %v", numCtxSent(f))
	}
	// per-model override beats the setting
	p.ctxFor = func(m string) int { return 12345 }
	ollamaChat(t, p, Options{})
	if numCtxSent(f) != 12345.0 {
		t.Errorf("override: %v", numCtxSent(f))
	}
}

func TestOllamaTruncationFlag(t *testing.T) {
	f, srv := newFakeOllama(t)
	f.prompt = 4096
	p := NewOpenAICompatible("", srv.URL+"/v1").WithOllama("auto", 4096)
	res := ollamaChat(t, p, Options{})
	if !res.Usage.Truncated || res.Usage.ContextWindow != 4096 {
		t.Fatalf("usage %+v", res.Usage)
	}
	f.prompt = 4095
	if res := ollamaChat(t, p, Options{}); res.Usage.Truncated {
		t.Fatal("not truncated below the window")
	}
}

func TestGenericPathUnchanged(t *testing.T) {
	var got map[string]any
	srv := sseServer(t, []string{"data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n", "data: [DONE]\n\n"}, &got, nil)
	// srv answers every path (incl. /api/version) with SSE, which is not JSON with a version.
	p := NewOpenAICompatible("", srv.URL+"/v1")
	res := ollamaChat(t, p, Options{})
	if res.Text != "ok" || res.Err != nil {
		t.Fatalf("%+v", res)
	}
	if got["stream_options"] == nil {
		t.Errorf("generic path must use /v1 chat completions: %v", got)
	}
	if _, has := got["options"]; has {
		t.Error("no ollama options on generic")
	}
}

func TestDiscoveryOllama(t *testing.T) {
	f, srv := newFakeOllama(t)
	f.models["chat:1b"] = `{"model_info":{"general.architecture":"qwen3","qwen3.context_length":40960},"capabilities":["completion","tools"]}`
	f.models["nomic-embed"] = `{"model_info":{"general.architecture":"bert","bert.context_length":2048},"capabilities":["embedding"]}`
	f.models["plain"] = `{"model_info":{"general.architecture":"llama","llama.context_length":4096},"capabilities":["completion"]}`
	r := NewRegistry()
	r.Register(NewOpenAICompatible("", srv.URL+"/v1"))
	if err := r.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	ms := r.Models()
	if len(ms) != 2 {
		t.Fatalf("models %+v", ms)
	}
	c, p := ms[0], ms[1]
	if c.Model != "chat:1b" || !c.Discovered || c.ContextWindow != 40960 || c.SupportsTools == nil || !*c.SupportsTools || !c.Priced || c.Prices != (Price{}) {
		t.Errorf("chat: %+v", c)
	}
	if p.Model != "plain" || p.SupportsTools == nil || *p.SupportsTools {
		t.Errorf("plain: %+v", p)
	}
	if _, known, ok := r.Lookup("openai-compatible", "chat:1b"); !ok || !known {
		t.Error("discovered models are usable immediately and free (priced)")
	}
	b, _ := json.Marshal(c)
	if !strings.Contains(string(b), `"contextWindow":40960`) || !strings.Contains(string(b), `"discovered":true`) {
		t.Errorf("json %s", b)
	}
}

func TestDiscoveryGenericModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" && r.Header.Get("Authorization") == "Bearer k" {
			fmt.Fprint(w, `{"data":[{"id":"gpt-x"},{"id":"claude-y"}]}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	r := NewRegistry()
	r.Register(NewOpenAICompatible("k", srv.URL+"/v1"))
	if err := r.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	ms := r.Models()
	if len(ms) != 2 || ms[0].Model != "claude-y" || ms[0].Priced || !ms[0].Discovered {
		t.Fatalf("%+v", ms)
	}
	if _, known, ok := r.Lookup("openai-compatible", "gpt-x"); !ok || known {
		t.Error("discovered non-ollama models are usable but cost-unknown")
	}
}

func TestDiscoverySkips(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, `{"data":[{"id":"a"}]}`)
	}))
	defer srv.Close()
	// discover=false
	r, err := NewRegistryFromSettingsForTest(srv.URL+"/v1", false)
	if err != nil {
		t.Fatal(err)
	}
	r.Discover(context.Background())
	if len(r.Models()) != 0 || hits.Load() != 0 {
		t.Errorf("discover=false must not call the provider (hits %d)", hits.Load())
	}
	// openrouter
	o := NewOpenAICompatible("", "https://openrouter.ai/api/v1")
	if o.discoverable() {
		t.Error("openrouter.ai must be skipped")
	}
	if !NewOpenAICompatible("", "http://localhost:4000/v1").discoverable() {
		t.Error("localhost is discoverable")
	}
	r2 := NewRegistry()
	r2.Register(o)
	if err := r2.Discover(context.Background()); err != nil || len(r2.Models()) != 0 {
		t.Error("openrouter discovery is a no-op")
	}
	// openai / anthropic never discover
	r3 := NewRegistry()
	r3.Register(NewOpenAI("k", srv.URL))
	r3.Register(NewAnthropic("k", srv.URL))
	r3.Discover(context.Background())
	if len(r3.Models()) != 0 || hits.Load() != 0 {
		t.Error("only openai-compatible discovers")
	}
}

func TestDiscoveryMergePrecedence(t *testing.T) {
	f, srv := newFakeOllama(t)
	f.models["gpt-oss:20b"] = `{"model_info":{"general.architecture":"gptoss","gptoss.context_length":131072},"capabilities":["completion","tools","thinking"]}`
	f.models["other"] = `{"model_info":{"general.architecture":"llama","llama.context_length":8192},"capabilities":["completion"]}`
	r := NewRegistry()
	r.Register(NewOpenAICompatible("", srv.URL+"/v1"))
	r.AddModel(ModelSpec{Provider: "openai-compatible", Model: "gpt-oss:20b", Priced: true, Prices: Price{InputMicrosPerMTok: 5}, ContextWindow: 20000})
	r.AddModel(ModelSpec{Provider: "openai-compatible", Model: "other"}) // declared, no prices
	if err := r.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	ms := r.Models()
	if len(ms) != 2 {
		t.Fatalf("%+v", ms)
	}
	g := ms[0]
	if g.Model != "gpt-oss:20b" || g.Prices.InputMicrosPerMTok != 5 || g.ContextWindow != 20000 || g.Discovered || g.SupportsTools == nil {
		t.Errorf("declared wins: %+v", g)
	}
	if o := ms[1]; !o.Priced || o.ContextWindow != 8192 {
		t.Errorf("declared-without-prices inherits the local zero price: %+v", o)
	}
	// the override feeds num_ctx
	p, _ := r.Provider("openai-compatible")
	res := ollamaChat(t, p.(*OpenAI), Options{})
	_ = res
	if o := p.(*OpenAI).ctxFor("gpt-oss:20b"); o != 20000 {
		t.Errorf("declared override = %d", o)
	}
}

func TestModelsFileContextWindow(t *testing.T) {
	r := NewRegistry()
	if err := r.LoadModelsJSON([]byte(`[{"provider":"openai-compatible","model":"m","context_window":9000}]`)); err != nil {
		t.Fatal(err)
	}
	if r.models[0].ContextWindow != 9000 {
		t.Errorf("%+v", r.models[0])
	}
}

func TestDiscoveryProviderDownKeepsList(t *testing.T) {
	f, srv := newFakeOllama(t)
	f.models["a"] = `{"model_info":{},"capabilities":["completion"]}`
	r := NewRegistry()
	r.Register(NewOpenAICompatible("", srv.URL+"/v1"))
	if err := r.Discover(context.Background()); err != nil || len(r.Models()) != 1 {
		t.Fatal("initial discovery")
	}
	f.down.Store(true)
	if err := r.Discover(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
	if len(r.Models()) != 1 {
		t.Error("last good list must be kept")
	}
}

func TestDiscoveryTTLRefresh(t *testing.T) {
	f, srv := newFakeOllama(t)
	f.models["a"] = `{"model_info":{},"capabilities":["completion"]}`
	r := NewRegistry()
	r.Register(NewOpenAICompatible("", srv.URL+"/v1"))
	r.SetDiscoveryTTL(80 * time.Millisecond)
	ctx := context.Background()
	r.RefreshModels(ctx, 2*time.Second)
	r.RefreshModels(ctx, 2*time.Second) // fresh: no new request
	if f.tags.Load() != 1 || len(r.Models()) != 1 {
		t.Fatalf("tags calls %d", f.tags.Load())
	}
	time.Sleep(120 * time.Millisecond)
	f.models["b"] = `{"model_info":{},"capabilities":["completion"]}`
	r.RefreshModels(ctx, 2*time.Second)
	if f.tags.Load() != 2 || len(r.Models()) != 2 {
		t.Fatalf("stale refresh: tags %d models %d", f.tags.Load(), len(r.Models()))
	}
}

func TestRefreshModelsDoesNotWaitOnSlowProvider(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/version" {
			http.NotFound(w, r)
			return
		}
		<-release
		fmt.Fprint(w, `{"data":[{"id":"late"}]}`)
	}))
	defer srv.Close()
	defer close(release)
	r := NewRegistry()
	r.Register(NewOpenAICompatible("", srv.URL+"/v1"))
	start := time.Now()
	r.RefreshModels(context.Background(), 100*time.Millisecond)
	if time.Since(start) > time.Second || len(r.Models()) != 0 {
		t.Errorf("took %v", time.Since(start))
	}
}

// NewRegistryFromSettingsForTest builds a registry through the settings path.
func NewRegistryFromSettingsForTest(base string, discover bool) (*Registry, error) {
	return NewRegistryFromSettings(configForTest(base, discover))
}

func configForTest(base string, discover bool) config.Settings {
	return config.Settings{LLMOpenAICompatibleBaseURL: base, LLMOpenAICompatibleDiscover: discover}
}
