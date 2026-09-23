package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// Native Ollama support for the openai-compatible provider (spec.md L7).
// Ollama's /v1/chat/completions cannot take a per-request context size, so a
// prompt full of tool definitions is silently truncated at the server default
// (4096). The native /api/chat accepts options.num_ctx.

const (
	ollamaAutoCtxCap = 32768
	ollamaShowTTL    = 10 * time.Minute
)

type ollamaState struct {
	mu       sync.Mutex
	detected int // 0 unknown, 1 ollama, 2 generic
	show     map[string]showEntry
}

type showEntry struct {
	info ollamaShow
	at   time.Time
}

type ollamaShow struct {
	ContextLength int // model's own maximum, 0 unknown
	Capabilities  []string
}

func (s ollamaShow) has(c string) bool {
	for _, x := range s.Capabilities {
		if x == c {
			return true
		}
	}
	return false
}

// WithOllama configures Ollama handling: kind is auto|ollama|generic (empty =
// auto) and numCtx the configured context window (0 = auto).
func (o *OpenAI) WithOllama(kind string, numCtx int) *OpenAI {
	if kind == "" {
		kind = "auto"
	}
	o.kind, o.numCtx = strings.ToLower(kind), numCtx
	return o
}

// ollamaRoot is the base URL without a trailing /v1.
func (o *OpenAI) ollamaRoot() string {
	return strings.TrimSuffix(strings.TrimRight(o.baseURL, "/"), "/v1")
}

func (o *OpenAI) httpClient() *http.Client {
	if o.Client != nil {
		return o.Client
	}
	return http.DefaultClient
}

func (o *OpenAI) authHeader(req *http.Request) {
	if o.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.apiKey)
	}
}

// getJSON does a GET with a short timeout and decodes the JSON answer.
func (o *OpenAI) getJSON(ctx context.Context, u string, dst any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	o.authHeader(req)
	resp, err := o.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return &HTTPError{Status: resp.StatusCode}
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(dst)
}

// isOllama reports whether the server behind this provider is Ollama. A
// definitive answer is cached; a network failure is not (retried next call).
func (o *OpenAI) isOllama(ctx context.Context) bool {
	switch o.kind {
	case "ollama":
		return true
	case "generic":
		return false
	}
	o.ol.mu.Lock()
	d := o.ol.detected
	o.ol.mu.Unlock()
	if d != 0 {
		return d == 1
	}
	var v struct {
		Version string `json:"version"`
	}
	dctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	err := o.getJSON(dctx, o.ollamaRoot()+"/api/version", &v)
	var he *HTTPError
	switch {
	case err == nil && v.Version != "":
		d = 1
	case err == nil, errors.As(err, &he):
		d = 2
	default:
		var se *json.SyntaxError
		var ue *json.UnmarshalTypeError
		if errors.As(err, &se) || errors.As(err, &ue) || errors.Is(err, io.EOF) {
			d = 2 // answered, but not with Ollama's JSON
		} else {
			return false // unreachable: unknown, do not cache
		}
	}
	o.ol.mu.Lock()
	o.ol.detected = d
	o.ol.mu.Unlock()
	if d == 1 {
		slog.Info("llm: openai-compatible endpoint is Ollama; using native /api/chat", "root", o.ollamaRoot(), "version", v.Version)
	}
	return d == 1
}

// showModel returns /api/show facts for a model, cached with a TTL. Failures
// are returned, not cached.
func (o *OpenAI) showModel(ctx context.Context, model string) (ollamaShow, error) {
	o.ol.mu.Lock()
	if e, ok := o.ol.show[model]; ok && time.Since(e.at) < ollamaShowTTL {
		o.ol.mu.Unlock()
		return e.info, nil
	}
	o.ol.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	payload, _ := json.Marshal(map[string]any{"model": model})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.ollamaRoot()+"/api/show", bytes.NewReader(payload))
	if err != nil {
		return ollamaShow{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	o.authHeader(req)
	resp, err := o.httpClient().Do(req)
	if err != nil {
		return ollamaShow{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ollamaShow{}, &HTTPError{Status: resp.StatusCode}
	}
	var raw struct {
		ModelInfo    map[string]any `json:"model_info"`
		Capabilities []string       `json:"capabilities"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&raw); err != nil {
		return ollamaShow{}, err
	}
	info := ollamaShow{Capabilities: raw.Capabilities}
	if arch, _ := raw.ModelInfo["general.architecture"].(string); arch != "" {
		if f, ok := raw.ModelInfo[arch+".context_length"].(float64); ok {
			info.ContextLength = int(f)
		}
	}
	if info.ContextLength == 0 { // architecture key missing: any *.context_length
		for k, v := range raw.ModelInfo {
			if f, ok := v.(float64); ok && strings.HasSuffix(k, ".context_length") {
				info.ContextLength = int(f)
			}
		}
	}
	o.ol.mu.Lock()
	if o.ol.show == nil {
		o.ol.show = map[string]showEntry{}
	}
	o.ol.show[model] = showEntry{info, time.Now()}
	o.ol.mu.Unlock()
	return info, nil
}

// contextWindow resolves num_ctx: models-file override, else the setting, else
// min(model maximum, 32768). 0 means unknown (num_ctx is then not sent).
func (o *OpenAI) contextWindow(ctx context.Context, model string, info ollamaShow) int {
	if o.ctxFor != nil {
		if n := o.ctxFor(model); n > 0 {
			return n
		}
	}
	if o.numCtx > 0 {
		return o.numCtx
	}
	if info.ContextLength > 0 {
		return min(info.ContextLength, ollamaAutoCtxCap)
	}
	return 0
}

func ollamaMessages(msgs []Message, system string) []map[string]any {
	var out []map[string]any
	if system != "" {
		out = append(out, map[string]any{"role": "system", "content": system})
	}
	names := map[string]string{} // tool call id -> name
	for _, m := range msgs {
		switch m.Role {
		case RoleSystem:
			out = append(out, map[string]any{"role": "system", "content": m.Text()})
		case RoleUser:
			msg := map[string]any{"role": "user", "content": m.Text()}
			var imgs []string
			for _, b := range m.Content {
				if b.Type == BlockImage {
					imgs = append(imgs, b.Data)
				}
			}
			if len(imgs) > 0 {
				msg["images"] = imgs
			}
			out = append(out, msg)
		case RoleAssistant:
			msg := map[string]any{"role": "assistant", "content": m.Text()}
			if len(m.ToolCalls) > 0 {
				var calls []map[string]any
				for _, c := range m.ToolCalls {
					names[c.ID] = c.Name
					args := c.Arguments
					if args == nil {
						args = map[string]any{}
					}
					calls = append(calls, map[string]any{"function": map[string]any{"name": c.Name, "arguments": args}})
				}
				msg["tool_calls"] = calls
			}
			out = append(out, msg)
		case RoleTool:
			for _, r := range m.ToolResults {
				text := blocksText(r.Content)
				for _, b := range r.Content {
					if b.Type == BlockImage {
						text += "\n[image omitted: not supported in tool results]"
					}
				}
				out = append(out, map[string]any{"role": "tool", "tool_name": names[r.ToolCallID], "content": text})
			}
		}
	}
	return out
}

func (o *OpenAI) completeOllama(ctx context.Context, msgs []Message, tools []Tool, opts Options) (<-chan Delta, error) {
	info, showErr := o.showModel(ctx, opts.Model)
	if showErr != nil {
		slog.Debug("llm: ollama /api/show failed", "model", opts.Model, "err", showErr)
	}
	numCtx := o.contextWindow(ctx, opts.Model, info)
	options := map[string]any{}
	if numCtx > 0 {
		options["num_ctx"] = numCtx
	}
	if opts.Temperature != nil {
		options["temperature"] = *opts.Temperature
	}
	if opts.MaxTokens > 0 {
		options["num_predict"] = opts.MaxTokens
	}
	body := map[string]any{
		"model": opts.Model, "stream": true,
		"messages": ollamaMessages(msgs, opts.System),
		"options":  options,
	}
	if opts.Thinking != nil && opts.Thinking.Type == "enabled" && info.has("thinking") {
		body["think"] = true
	}
	if kept := sanitizeTools(o.name, tools); len(kept) > 0 {
		var ts []map[string]any
		for _, t := range kept {
			fn := map[string]any{"name": t.Name, "parameters": t.InputSchema}
			if t.Description != "" {
				fn["description"] = t.Description
			}
			ts = append(ts, map[string]any{"type": "function", "function": fn})
		}
		body["tools"] = ts
	}
	headers := map[string]string{}
	if o.apiKey != "" {
		headers["Authorization"] = "Bearer " + o.apiKey
	}
	resp, err := postStream(ctx, o.Client, o.ollamaRoot()+"/api/chat", headers, body)
	if err != nil {
		return nil, err
	}
	ch := make(chan Delta, 16)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		o.streamOllama(emitter{ctx, ch}, resp.Body, opts.Model, numCtx)
	}()
	return ch, nil
}

func (o *OpenAI) streamOllama(em emitter, body io.Reader, model string, numCtx int) {
	br := bufio.NewReaderSize(body, 64*1024)
	nCalls := 0
	for {
		line, rerr := br.ReadBytes('\n')
		if line = bytes.TrimSpace(line); len(line) > 0 {
			var c struct {
				Error   string `json:"error"`
				Message struct {
					Content   string `json:"content"`
					Thinking  string `json:"thinking"`
					ToolCalls []struct {
						ID       string `json:"id"`
						Function struct {
							Name      string          `json:"name"`
							Arguments json.RawMessage `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"message"`
				Done            bool   `json:"done"`
				DoneReason      string `json:"done_reason"`
				PromptEvalCount int    `json:"prompt_eval_count"`
				EvalCount       int    `json:"eval_count"`
			}
			if err := json.Unmarshal(line, &c); err != nil {
				em.fail(fmt.Errorf("%s: bad stream chunk: %w", o.name, err))
				return
			}
			if c.Error != "" {
				em.fail(fmt.Errorf("%s: %s", o.name, c.Error))
				return
			}
			if c.Message.Thinking != "" && !em.send(Delta{Type: DeltaThinking, Text: c.Message.Thinking}) {
				return
			}
			if c.Message.Content != "" && !em.send(Delta{Type: DeltaText, Text: c.Message.Content}) {
				return
			}
			for _, tc := range c.Message.ToolCalls {
				id := tc.ID
				if id == "" {
					id = fmt.Sprintf("call_%d", nCalls)
				}
				nCalls++
				args := parseArgsRaw(tc.Function.Arguments)
				if !em.send(Delta{Type: DeltaToolCall, ContentIndex: nCalls, ToolCall: &ToolCall{ID: id, Name: tc.Function.Name, Arguments: args}}) {
					return
				}
			}
			if c.Done {
				u := &Usage{InputTokens: c.PromptEvalCount, OutputTokens: c.EvalCount, ContextWindow: numCtx}
				if numCtx > 0 && c.PromptEvalCount >= numCtx {
					u.Truncated = true
					slog.Warn("llm: prompt filled the Ollama context window; the oldest part was truncated by the server",
						"model", model, "num_ctx", numCtx, "prompt_tokens", c.PromptEvalCount)
				}
				if (c.PromptEvalCount > 0 || c.EvalCount > 0) && !em.send(Delta{Type: DeltaUsage, Usage: u}) {
					return
				}
				reason := FinishStop
				switch {
				case nCalls > 0:
					reason = FinishToolUse
				case c.DoneReason == "length":
					reason = FinishMaxTokens
				}
				em.send(Delta{Type: DeltaDone, FinishReason: reason})
				return
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				em.fail(errors.New(o.name + ": stream ended without done"))
			} else {
				em.fail(rerr)
			}
			return
		}
	}
}

func parseArgsRaw(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err == nil && m != nil {
		return m
	}
	var s string // some servers send arguments as a JSON string
	if json.Unmarshal(raw, &s) == nil {
		return parseArgs(s)
	}
	return map[string]any{}
}

// --------------------------------------------------------------------------
// discovery (L8)
// --------------------------------------------------------------------------

// discoverable reports whether discovery makes sense for this endpoint.
func (o *OpenAI) discoverable() bool {
	u, err := url.Parse(o.baseURL)
	if err != nil {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return h != "openrouter.ai" && !strings.HasSuffix(h, ".openrouter.ai")
}

func (o *OpenAI) discoverModels(ctx context.Context) ([]ModelSpec, error) {
	if o.isOllama(ctx) {
		return o.discoverOllama(ctx)
	}
	var v struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := o.getJSON(ctx, o.baseURL+"/models", &v); err != nil {
		return nil, err
	}
	var out []ModelSpec
	for _, d := range v.Data {
		if d.ID != "" {
			out = append(out, ModelSpec{Provider: o.name, Model: d.ID, Discovered: true})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out, nil
}

func (o *OpenAI) discoverOllama(ctx context.Context) ([]ModelSpec, error) {
	var tags struct {
		Models []struct {
			Name  string `json:"name"`
			Model string `json:"model"`
		} `json:"models"`
	}
	if err := o.getJSON(ctx, o.ollamaRoot()+"/api/tags", &tags); err != nil {
		return nil, err
	}
	specs := make([]*ModelSpec, len(tags.Models))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, t := range tags.Models {
		name := t.Model
		if name == "" {
			name = t.Name
		}
		if name == "" {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			spec := &ModelSpec{Provider: o.name, Model: name, Discovered: true, Priced: true}
			info, err := o.showModel(ctx, name)
			if err == nil {
				if info.has("embedding") && !info.has("completion") {
					return
				}
				spec.ContextWindow = info.ContextLength
				if len(info.Capabilities) > 0 {
					b := info.has("tools")
					spec.SupportsTools = &b
				}
			}
			specs[i] = spec
		}()
	}
	wg.Wait()
	var out []ModelSpec
	for _, s := range specs {
		if s != nil {
			out = append(out, *s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out, nil
}
