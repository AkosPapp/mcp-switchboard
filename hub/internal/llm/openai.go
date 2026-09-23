package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

const openaiDefaultBase = "https://api.openai.com/v1"

// OpenAI speaks Chat Completions over streaming SSE. The same implementation,
// with a base URL, serves openai-compatible servers (Ollama, vLLM, OpenRouter,
// LM Studio).
type OpenAI struct {
	name       string
	apiKey     string
	baseURL    string
	compatible bool
	Client     *http.Client

	// Ollama support (openai-compatible only; see ollama.go).
	kind   string // auto | ollama | generic
	numCtx int    // MCP_SWITCHBOARD_LLM_OLLAMA_NUM_CTX, 0 = auto
	ctxFor func(model string) int
	ol     ollamaState
}

// NewOpenAI returns the "openai" provider; empty baseURL means api.openai.com.
func NewOpenAI(apiKey, baseURL string) *OpenAI {
	if baseURL == "" {
		baseURL = openaiDefaultBase
	}
	return &OpenAI{name: "openai", apiKey: apiKey, baseURL: strings.TrimRight(baseURL, "/")}
}

// NewOpenAICompatible returns the "openai-compatible" provider. The key may be
// empty; baseURL should include any /v1 prefix the server expects.
func NewOpenAICompatible(apiKey, baseURL string) *OpenAI {
	return &OpenAI{name: "openai-compatible", apiKey: apiKey, baseURL: strings.TrimRight(baseURL, "/"), compatible: true}
}

// WithName overrides the provider name (useful for several compatible servers).
func (o *OpenAI) WithName(name string) *OpenAI { o.name = name; return o }

func (o *OpenAI) Name() string { return o.name }

// String never includes the key.
func (o *OpenAI) String() string { return o.name + "(" + o.baseURL + ")" }

func openaiMessages(msgs []Message, system string) []map[string]any {
	var out []map[string]any
	if system != "" {
		out = append(out, map[string]any{"role": "system", "content": system})
	}
	for _, m := range msgs {
		switch m.Role {
		case RoleSystem:
			out = append(out, map[string]any{"role": "system", "content": m.Text()})
		case RoleUser:
			hasImage := false
			for _, b := range m.Content {
				hasImage = hasImage || b.Type == BlockImage
			}
			if !hasImage {
				out = append(out, map[string]any{"role": "user", "content": m.Text()})
				continue
			}
			var parts []map[string]any
			for _, b := range m.Content {
				switch b.Type {
				case BlockText:
					parts = append(parts, map[string]any{"type": "text", "text": b.Text})
				case BlockImage:
					parts = append(parts, map[string]any{"type": "image_url",
						"image_url": map[string]any{"url": "data:" + b.MediaType + ";base64," + b.Data}})
				}
			}
			out = append(out, map[string]any{"role": "user", "content": parts})
		case RoleAssistant:
			msg := map[string]any{"role": "assistant"}
			text := m.Text()
			if text != "" || len(m.ToolCalls) == 0 {
				msg["content"] = text
			}
			if len(m.ToolCalls) > 0 {
				var calls []map[string]any
				for _, c := range m.ToolCalls {
					args, _ := json.Marshal(c.Arguments)
					if c.Arguments == nil {
						args = []byte("{}")
					}
					calls = append(calls, map[string]any{"id": c.ID, "type": "function",
						"function": map[string]any{"name": c.Name, "arguments": string(args)}})
				}
				msg["tool_calls"] = calls
			}
			out = append(out, msg)
		case RoleTool:
			// One role=tool message per result, in order.
			for _, r := range m.ToolResults {
				text := blocksText(r.Content)
				for _, b := range r.Content {
					if b.Type == BlockImage {
						text += "\n[image omitted: not supported in tool results]"
					}
				}
				out = append(out, map[string]any{"role": "tool", "tool_call_id": r.ToolCallID, "content": text})
			}
		}
	}
	return out
}

func (o *OpenAI) Complete(ctx context.Context, msgs []Message, tools []Tool, opts Options) (<-chan Delta, error) {
	if opts.Model == "" {
		return nil, fmt.Errorf("llm: %s: model is required", o.name)
	}
	if o.compatible && o.isOllama(ctx) {
		return o.completeOllama(ctx, msgs, tools, opts)
	}
	body := map[string]any{
		"model": opts.Model, "stream": true,
		"stream_options": map[string]any{"include_usage": true},
		"messages":       openaiMessages(msgs, opts.System),
	}
	if opts.MaxTokens > 0 {
		if o.compatible {
			body["max_tokens"] = opts.MaxTokens
		} else {
			body["max_completion_tokens"] = opts.MaxTokens
		}
	}
	if opts.Temperature != nil {
		body["temperature"] = *opts.Temperature
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
	resp, err := postStream(ctx, o.Client, o.baseURL+"/chat/completions", headers, body)
	if err != nil {
		return nil, err
	}
	ch := make(chan Delta, 16)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		o.stream(emitter{ctx, ch}, resp)
	}()
	return ch, nil
}

type oaiCall struct {
	id, name string
	args     strings.Builder
}

func (o *OpenAI) stream(em emitter, resp *http.Response) {
	var usage *Usage
	finish := ""
	calls := map[int]*oaiCall{}
	sawDone := false

	err := readSSE(resp.Body, func(_, data string) error {
		if strings.TrimSpace(data) == "[DONE]" {
			sawDone = true
			return nil
		}
		var ch struct {
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					Reasoning string `json:"reasoning_content"`
					Reason2   string `json:"reasoning"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				Prompt  int `json:"prompt_tokens"`
				Compl   int `json:"completion_tokens"`
				Details *struct {
					Cached int `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &ch); err != nil {
			return fmt.Errorf("%s: bad stream chunk: %w", o.name, err)
		}
		if ch.Error != nil {
			return fmt.Errorf("%s: %s", o.name, ch.Error.Message)
		}
		if ch.Usage != nil {
			cached := 0
			if ch.Usage.Details != nil {
				cached = ch.Usage.Details.Cached
			}
			// prompt_tokens includes cached ones; keep Usage fields disjoint.
			usage = &Usage{InputTokens: ch.Usage.Prompt - cached, OutputTokens: ch.Usage.Compl, CacheReadTokens: cached}
		}
		for _, c := range ch.Choices {
			d := c.Delta
			think := d.Reasoning
			if think == "" {
				think = d.Reason2
			}
			if think != "" && !em.send(Delta{Type: DeltaThinking, Text: think}) {
				return context.Canceled
			}
			if d.Content != "" && !em.send(Delta{Type: DeltaText, Text: d.Content}) {
				return context.Canceled
			}
			for _, tc := range d.ToolCalls {
				st := calls[tc.Index]
				if st == nil {
					st = &oaiCall{}
					calls[tc.Index] = st
				}
				if tc.ID != "" {
					st.id = tc.ID
				}
				if tc.Function.Name != "" {
					st.name = tc.Function.Name
				}
				st.args.WriteString(tc.Function.Arguments)
			}
			if c.FinishReason != "" {
				finish = c.FinishReason
			}
		}
		return nil
	})
	if err != nil {
		em.fail(err)
		return
	}
	if finish == "" && !sawDone {
		em.fail(errors.New(o.name + ": stream ended without a finish reason"))
		return
	}

	idx := make([]int, 0, len(calls))
	for i := range calls {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	for n, i := range idx {
		st := calls[i]
		id := st.id
		if id == "" {
			id = fmt.Sprintf("call_%d", n)
		}
		tc := &ToolCall{ID: id, Name: st.name, Arguments: parseArgs(st.args.String())}
		if !em.send(Delta{Type: DeltaToolCall, ContentIndex: i + 1, ToolCall: tc}) {
			return
		}
	}
	if usage != nil && !em.send(Delta{Type: DeltaUsage, Usage: usage}) {
		return
	}
	reason := FinishStop
	switch {
	case len(calls) > 0 || finish == "tool_calls" || finish == "function_call":
		reason = FinishToolUse
	case finish == "length":
		reason = FinishMaxTokens
	}
	em.send(Delta{Type: DeltaDone, FinishReason: reason})
}
