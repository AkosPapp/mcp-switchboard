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

const (
	anthropicDefaultBase = "https://api.anthropic.com"
	anthropicVersion     = "2023-06-01"
	anthropicMaxTokens   = 4096
)

// Anthropic speaks the Messages API over streaming SSE.
type Anthropic struct {
	apiKey  string
	baseURL string
	Client  *http.Client
}

// NewAnthropic returns the provider; an empty baseURL means api.anthropic.com.
func NewAnthropic(apiKey, baseURL string) *Anthropic {
	if baseURL == "" {
		baseURL = anthropicDefaultBase
	}
	return &Anthropic{apiKey: apiKey, baseURL: strings.TrimRight(baseURL, "/")}
}

func (a *Anthropic) Name() string { return "anthropic" }

// String never includes the key.
func (a *Anthropic) String() string { return "anthropic(" + a.baseURL + ")" }

type anthMsg struct {
	Role    string           `json:"role"`
	Content []map[string]any `json:"content"`
}

// anthContentItem is one block of an assistant turn, still tagged with its
// original content-block index so anthAssistantBlocks can restore order.
type anthContentItem struct {
	index int
	block map[string]any
}

// anthAssistantBlocks rebuilds an assistant turn's content in the order the
// model actually produced it: Message.Content (text/thinking) and
// Message.ToolCalls are stored as two separate slices (see ToolCall.Index),
// so replaying them content-then-calls, as if every thinking block preceded
// every tool call, breaks Anthropic's extended-thinking requirement that a
// thinking block stay immediately before the tool_use it led to - the API
// then rejects the next turn's request, and the call never gets to run
// again. Both slices carry the provider's original content-block index
// (Delta.ContentIndex); merging by that index restores the real order.
func anthAssistantBlocks(m Message) []map[string]any {
	items := make([]anthContentItem, 0, len(m.Content)+len(m.ToolCalls))
	for _, b := range m.Content {
		for _, blk := range anthBlocks([]Block{b}, true) {
			items = append(items, anthContentItem{index: b.Index, block: blk})
		}
	}
	for _, c := range m.ToolCalls {
		args := c.Arguments
		if args == nil {
			args = map[string]any{}
		}
		items = append(items, anthContentItem{
			index: c.Index,
			block: map[string]any{"type": "tool_use", "id": c.ID, "name": c.Name, "input": args},
		})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].index < items[j].index })
	out := make([]map[string]any, len(items))
	for i, it := range items {
		out[i] = it.block
	}
	return out
}

func anthBlocks(bs []Block, allowThinking bool) []map[string]any {
	var out []map[string]any
	for _, b := range bs {
		switch b.Type {
		case BlockText:
			if b.Text != "" {
				out = append(out, map[string]any{"type": "text", "text": b.Text})
			}
		case BlockImage:
			out = append(out, map[string]any{"type": "image", "source": map[string]any{
				"type": "base64", "media_type": b.MediaType, "data": b.Data}})
		case BlockThinking:
			// Only a signed thinking block can be replayed.
			if allowThinking && b.Signature != "" {
				out = append(out, map[string]any{"type": "thinking", "thinking": b.Text, "signature": b.Signature})
			}
		}
	}
	return out
}

// anthropicMessages converts our chain (L6): system messages are lifted out,
// and consecutive tool results merge into one user message of tool_result
// blocks (later user text joins the same message, after the results).
func anthropicMessages(msgs []Message, system string) (string, []anthMsg) {
	var sys []string
	if system != "" {
		sys = append(sys, system)
	}
	var out []anthMsg
	appendUser := func(blocks []map[string]any) {
		if len(blocks) == 0 {
			return
		}
		if n := len(out); n > 0 && out[n-1].Role == "user" {
			out[n-1].Content = append(out[n-1].Content, blocks...)
			return
		}
		out = append(out, anthMsg{Role: "user", Content: blocks})
	}
	for _, m := range msgs {
		switch m.Role {
		case RoleSystem:
			if t := m.Text(); t != "" {
				sys = append(sys, t)
			}
		case RoleUser:
			appendUser(anthBlocks(m.Content, false))
		case RoleTool:
			var blocks []map[string]any
			for _, r := range m.ToolResults {
				content := anthBlocks(r.Content, false)
				if content == nil {
					content = []map[string]any{}
				}
				tr := map[string]any{"type": "tool_result", "tool_use_id": r.ToolCallID, "content": content}
				if r.IsError {
					tr["is_error"] = true
				}
				blocks = append(blocks, tr)
			}
			appendUser(blocks)
		case RoleAssistant:
			blocks := anthAssistantBlocks(m)
			if len(blocks) > 0 {
				out = append(out, anthMsg{Role: "assistant", Content: blocks})
			}
		}
	}
	return strings.Join(sys, "\n\n"), out
}

func (a *Anthropic) Complete(ctx context.Context, msgs []Message, tools []Tool, opts Options) (<-chan Delta, error) {
	if opts.Model == "" {
		return nil, errors.New("llm: anthropic: model is required")
	}
	system, amsgs := anthropicMessages(msgs, opts.System)
	maxTokens := opts.MaxTokens
	if maxTokens <= 0 {
		maxTokens = anthropicMaxTokens
	}
	body := map[string]any{"model": opts.Model, "max_tokens": maxTokens, "stream": true, "messages": amsgs}
	if system != "" {
		body["system"] = system
	}
	if kept := sanitizeTools(a.Name(), tools); len(kept) > 0 {
		var ts []map[string]any
		for _, t := range kept {
			d := map[string]any{"name": t.Name, "input_schema": t.InputSchema}
			if t.Description != "" {
				d["description"] = t.Description
			}
			ts = append(ts, d)
		}
		body["tools"] = ts
	}
	if opts.Thinking != nil && opts.Thinking.Type == "enabled" {
		body["thinking"] = map[string]any{"type": "enabled", "budget_tokens": opts.Thinking.BudgetTokens}
	} else if opts.Temperature != nil {
		body["temperature"] = *opts.Temperature // thinking forbids setting it
	}

	resp, err := postStream(ctx, a.Client, a.baseURL+"/v1/messages",
		map[string]string{"x-api-key": a.apiKey, "anthropic-version": anthropicVersion}, body)
	if err != nil {
		return nil, err
	}
	ch := make(chan Delta, 16)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		a.stream(emitter{ctx, ch}, resp)
	}()
	return ch, nil
}

type anthBlockState struct {
	typ, id, name string
	args          strings.Builder
	sig           string
}

func (a *Anthropic) stream(em emitter, resp *http.Response) {
	var usage Usage
	finish := ""
	finished := false
	blocks := map[int]*anthBlockState{}

	err := readSSE(resp.Body, func(event, data string) error {
		var ev struct {
			Type  string `json:"type"`
			Index int    `json:"index"`
			Error *struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
			Message struct {
				Usage struct {
					Input      int `json:"input_tokens"`
					Output     int `json:"output_tokens"`
					CacheRead  int `json:"cache_read_input_tokens"`
					CacheWrite int `json:"cache_creation_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
			ContentBlock struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				Signature   string `json:"signature"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			Usage struct {
				Input      int `json:"input_tokens"`
				Output     int `json:"output_tokens"`
				CacheRead  int `json:"cache_read_input_tokens"`
				CacheWrite int `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return fmt.Errorf("anthropic: bad stream event: %w", err)
		}
		if ev.Type == "" {
			ev.Type = event
		}
		switch ev.Type {
		case "message_start":
			u := ev.Message.Usage
			usage.InputTokens, usage.OutputTokens = u.Input, u.Output
			usage.CacheReadTokens, usage.CacheWriteTokens = u.CacheRead, u.CacheWrite
		case "content_block_start":
			blocks[ev.Index] = &anthBlockState{typ: ev.ContentBlock.Type, id: ev.ContentBlock.ID, name: ev.ContentBlock.Name}
		case "content_block_delta":
			st := blocks[ev.Index]
			if st == nil {
				st = &anthBlockState{}
				blocks[ev.Index] = st
			}
			switch ev.Delta.Type {
			case "text_delta":
				if !em.send(Delta{Type: DeltaText, ContentIndex: ev.Index, Text: ev.Delta.Text}) {
					return context.Canceled
				}
			case "thinking_delta":
				if !em.send(Delta{Type: DeltaThinking, ContentIndex: ev.Index, Text: ev.Delta.Thinking}) {
					return context.Canceled
				}
			case "signature_delta":
				st.sig += ev.Delta.Signature
			case "input_json_delta":
				st.args.WriteString(ev.Delta.PartialJSON)
			}
		case "content_block_stop":
			st := blocks[ev.Index]
			if st == nil {
				break
			}
			delete(blocks, ev.Index)
			switch st.typ {
			case "tool_use":
				tc := &ToolCall{ID: st.id, Name: st.name, Arguments: parseArgs(st.args.String())}
				if !em.send(Delta{Type: DeltaToolCall, ContentIndex: ev.Index, ToolCall: tc}) {
					return context.Canceled
				}
			case "thinking":
				if st.sig != "" && !em.send(Delta{Type: DeltaThinking, ContentIndex: ev.Index, Signature: st.sig}) {
					return context.Canceled
				}
			}
		case "message_delta":
			if ev.Delta.StopReason != "" {
				finish = mapAnthropicStop(ev.Delta.StopReason)
			}
			// message_delta usage is cumulative for the fields it carries.
			if ev.Usage.Output > 0 {
				usage.OutputTokens = ev.Usage.Output
			}
			if ev.Usage.Input > 0 {
				usage.InputTokens = ev.Usage.Input
			}
			if ev.Usage.CacheRead > 0 {
				usage.CacheReadTokens = ev.Usage.CacheRead
			}
			if ev.Usage.CacheWrite > 0 {
				usage.CacheWriteTokens = ev.Usage.CacheWrite
			}
		case "message_stop":
			finished = true
		case "error":
			msg := "unknown error"
			if ev.Error != nil {
				msg = ev.Error.Type + ": " + ev.Error.Message
			}
			return fmt.Errorf("anthropic: %s", msg)
		}
		return nil
	})
	if err != nil {
		em.fail(err)
		return
	}
	if !finished {
		em.fail(errors.New("anthropic: stream ended before message_stop"))
		return
	}
	if finish == "" {
		finish = FinishStop
	}
	u := usage
	if !em.send(Delta{Type: DeltaUsage, Usage: &u}) {
		return
	}
	em.send(Delta{Type: DeltaDone, FinishReason: finish})
}

func mapAnthropicStop(r string) string {
	switch r {
	case "tool_use":
		return FinishToolUse
	case "max_tokens":
		return FinishMaxTokens
	default:
		return FinishStop
	}
}
