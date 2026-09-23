// Package llm is the provider layer (spec.md §5.7): one streaming interface,
// three providers (anthropic, openai, openai-compatible), a model registry with
// integer-micro pricing, and a scripted fake for tests.
package llm

import "context"

// Message roles.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Block types.
const (
	BlockText     = "text"
	BlockImage    = "image"
	BlockThinking = "thinking"
)

// Delta types.
const (
	DeltaText     = "text"
	DeltaThinking = "thinking"
	DeltaToolCall = "tool_call"
	DeltaUsage    = "usage"
	DeltaDone     = "done"
	DeltaError    = "error"
)

// Finish reasons carried by the "done" delta.
const (
	FinishStop      = "stop"
	FinishToolUse   = "tool_use"
	FinishMaxTokens = "max_tokens"
	FinishError     = "error"
)

// Message is one provider-neutral conversation entry. A "tool" message carries
// exactly one ToolResult (the R6 chain shape); providers regroup as needed.
type Message struct {
	Role        string       `json:"role"`
	Content     []Block      `json:"content,omitempty"`
	ToolCalls   []ToolCall   `json:"tool_calls,omitempty"`
	ToolResults []ToolResult `json:"tool_results,omitempty"`
}

// TextMessage is shorthand for a single-text-block message.
func TextMessage(role, text string) Message {
	return Message{Role: role, Content: []Block{{Type: BlockText, Text: text}}}
}

// Text concatenates the text blocks of a message.
func (m Message) Text() string { return blocksText(m.Content) }

// Block is one piece of content. Image blocks carry base64 Data and MediaType.
// Signature is the opaque provider signature of a thinking block, which
// Anthropic requires when the block is replayed alongside tool use.
type Block struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	Signature string `json:"signature,omitempty"`
	// Index is the provider's original content-block position within the
	// turn (Delta.ContentIndex), carried onto the persisted block so a
	// replayed assistant message can be rebuilt in the order the model
	// actually produced it - see ToolCall.Index.
	Index int `json:"index,omitempty"`
}

// ToolCall is a fully assembled tool invocation requested by the model.
type ToolCall struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
	// Index is the provider's content-block position this call was made at
	// (Delta.ContentIndex). Content blocks (thinking/text) and tool calls are
	// stored in two separate slices (Message.Content / Message.ToolCalls),
	// so this is the only record of how they interleaved; without it a
	// replayed assistant message reorders every tool call after every
	// content block, which breaks Anthropic's extended-thinking requirement
	// that a thinking block stay immediately before the tool_use it led to
	// (the API rejects the next turn, so the call never gets to run again).
	Index int `json:"index,omitempty"`
}

// ToolResult answers one ToolCall.
type ToolResult struct {
	ToolCallID string  `json:"tool_call_id"`
	Content    []Block `json:"content,omitempty"`
	IsError    bool    `json:"is_error,omitempty"`
}

// Tool is a callable function offered to the model; InputSchema is the MCP
// tool's JSON Schema, passed through untouched (L6).
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema,omitempty"`
}

// Thinking configures extended thinking (Type "enabled").
type Thinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

// Options are the per-call knobs.
type Options struct {
	Model       string
	MaxTokens   int
	Temperature *float64
	Thinking    *Thinking
	System      string
}

// Usage counts tokens. InputTokens EXCLUDES cache reads and writes, so the four
// fields are disjoint and Price.Cost is a plain weighted sum.
type Usage struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
	// ContextWindow is the context size (tokens) the request ran with, when the
	// provider let us set/know it (native Ollama). Truncated is true when the
	// prompt filled that window, so the server silently dropped the oldest part.
	ContextWindow int  `json:"context_window,omitempty"`
	Truncated     bool `json:"truncated,omitempty"`
}

// Add returns the field-wise sum.
func (u Usage) Add(o Usage) Usage {
	cw := u.ContextWindow
	if o.ContextWindow > cw {
		cw = o.ContextWindow
	}
	return Usage{
		InputTokens: u.InputTokens + o.InputTokens, OutputTokens: u.OutputTokens + o.OutputTokens,
		CacheReadTokens: u.CacheReadTokens + o.CacheReadTokens, CacheWriteTokens: u.CacheWriteTokens + o.CacheWriteTokens,
		ContextWindow: cw, Truncated: u.Truncated || o.Truncated,
	}
}

// Delta is one streamed event. A stream is a sequence of text/thinking deltas
// and complete tool_call deltas, then at most one usage delta, then exactly one
// final "done" or "error" delta, after which the channel is closed.
// ContentIndex is the provider's content-block index (best effort). Signature
// is set on a "thinking" delta that closes a thinking block.
type Delta struct {
	Type         string
	ContentIndex int
	Text         string
	Signature    string
	ToolCall     *ToolCall
	Usage        *Usage
	FinishReason string
	Err          error
}

// Provider streams one model completion. Complete returns an error only when
// the request could not be started (bad config, HTTP status >= 400); failures
// after streaming begins arrive as an "error" delta. Cancelling ctx aborts the
// HTTP request and closes the channel.
type Provider interface {
	Name() string
	Complete(ctx context.Context, msgs []Message, tools []Tool, opts Options) (<-chan Delta, error)
}

func blocksText(bs []Block) string {
	var out string
	for _, b := range bs {
		if b.Type == BlockText {
			out += b.Text
		}
	}
	return out
}

// Collect drains a stream into its parts; handy for tests and non-streaming
// consumers. The error is the "error" delta's, if any.
type Result struct {
	Text         string
	Thinking     string
	ToolCalls    []ToolCall
	Usage        Usage
	FinishReason string
	Err          error
}

// Collect reads ch to completion.
func Collect(ch <-chan Delta) Result {
	var r Result
	for d := range ch {
		switch d.Type {
		case DeltaText:
			r.Text += d.Text
		case DeltaThinking:
			r.Thinking += d.Text
		case DeltaToolCall:
			if d.ToolCall != nil {
				r.ToolCalls = append(r.ToolCalls, *d.ToolCall)
			}
		case DeltaUsage:
			if d.Usage != nil {
				r.Usage = r.Usage.Add(*d.Usage)
			}
		case DeltaDone:
			r.FinishReason = d.FinishReason
		case DeltaError:
			r.FinishReason = FinishError
			r.Err = d.Err
		}
	}
	return r
}
