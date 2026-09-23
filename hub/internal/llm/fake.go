package llm

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Turn is one canned model answer. Build one with Say, CallTool, or a literal.
// Zero Usage is replaced by a deterministic estimate (chars/4) so budgets see
// non-zero tokens; set NoUsage to suppress the usage delta entirely.
type Turn struct {
	Thinking  string
	Text      string
	ToolCalls []ToolCall // empty ID gets "call_<n>_<i>"
	Usage     *Usage
	NoUsage   bool
	Finish    string // default "tool_use" if ToolCalls else "stop"
	Err       error  // streamed as an error delta after everything else
	Delay     time.Duration
	// Deltas, when non-empty, is emitted verbatim instead of the fields above
	// (a "done" delta is appended if absent).
	Deltas []Delta
}

// Say is a plain text answer.
func Say(text string) Turn { return Turn{Text: text} }

// CallTool is an answer that requests one tool call.
func CallTool(name string, args map[string]any) Turn {
	return Turn{ToolCalls: []ToolCall{{Name: name, Arguments: args}}}
}

// Fail is a turn that ends in a stream error.
func Fail(err error) Turn { return Turn{Err: err} }

// WithText adds text before the tool calls.
func (t Turn) WithText(s string) Turn { t.Text = s; return t }

// WithCall adds another tool call (parallel tool use).
func (t Turn) WithCall(name string, args map[string]any) Turn {
	t.ToolCalls = append(append([]ToolCall(nil), t.ToolCalls...), ToolCall{Name: name, Arguments: args})
	return t
}

// WithUsage sets explicit token counts.
func (t Turn) WithUsage(in, out int) Turn {
	t.Usage = &Usage{InputTokens: in, OutputTokens: out}
	return t
}

// After delays the turn before streaming.
func (t Turn) After(d time.Duration) Turn { t.Delay = d; return t }

// FakeCall records one Complete invocation for assertions.
type FakeCall struct {
	Messages []Message
	Tools    []Tool
	Options  Options
}

// Scripted is a deterministic Provider (V6): it replays canned turns, or asks
// a function what to say. Safe for concurrent use.
type Scripted struct {
	name string
	fn   func(call int, msgs []Message) Turn

	mu    sync.Mutex
	turns []Turn
	next  int // index into turns
	calls []FakeCall
	delay func(call int) time.Duration
}

// NewScripted replays turns in order; once exhausted, Complete returns an
// error (a test that makes more calls than scripted should fail loudly).
func NewScripted(name string, turns ...Turn) *Scripted {
	return &Scripted{name: name, turns: turns}
}

// NewScriptedFunc decides each answer with fn(callIndex, messages); call
// indexes start at 0. fn runs synchronously inside Complete, under no lock of
// this type, so it may call back into the code under test.
func NewScriptedFunc(name string, fn func(call int, msgs []Message) Turn) *Scripted {
	return &Scripted{name: name, fn: fn}
}

// Push appends turns to a replay script.
func (s *Scripted) Push(turns ...Turn) {
	s.mu.Lock()
	s.turns = append(s.turns, turns...)
	s.mu.Unlock()
}

// SetDelay installs a per-call delay hook applied before streaming, in
// addition to Turn.Delay. Useful to keep a run "in flight".
func (s *Scripted) SetDelay(fn func(call int) time.Duration) {
	s.mu.Lock()
	s.delay = fn
	s.mu.Unlock()
}

func (s *Scripted) Name() string { return s.name }

// Calls returns a copy of every recorded invocation.
func (s *Scripted) Calls() []FakeCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]FakeCall(nil), s.calls...)
}

// CallCount is the number of Complete calls so far.
func (s *Scripted) CallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// Models returns a spec for a model of this provider, for Registry.Register.
func (s *Scripted) Models(names ...string) []ModelSpec {
	if len(names) == 0 {
		names = []string{"fake"}
	}
	var out []ModelSpec
	for _, n := range names {
		out = append(out, ModelSpec{Provider: s.name, Model: n})
	}
	return out
}

func (s *Scripted) Complete(ctx context.Context, msgs []Message, tools []Tool, opts Options) (<-chan Delta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	call := len(s.calls)
	s.calls = append(s.calls, FakeCall{
		Messages: append([]Message(nil), msgs...),
		Tools:    append([]Tool(nil), tools...),
		Options:  opts,
	})
	delayFn := s.delay
	var turn Turn
	if s.fn == nil {
		if s.next >= len(s.turns) {
			s.mu.Unlock()
			return nil, fmt.Errorf("llm: scripted provider %q exhausted after %d turns", s.name, len(s.turns))
		}
		turn = s.turns[s.next]
		s.next++
	}
	s.mu.Unlock()
	if s.fn != nil {
		turn = s.fn(call, msgs)
	}

	delay := turn.Delay
	if delayFn != nil {
		delay += delayFn(call)
	}
	deltas := s.deltasFor(turn, call, msgs)

	ch := make(chan Delta, 8)
	go func() {
		defer close(ch)
		if delay > 0 {
			t := time.NewTimer(delay)
			defer t.Stop()
			select {
			case <-t.C:
			case <-ctx.Done():
				return
			}
		}
		em := emitter{ctx, ch}
		for _, d := range deltas {
			if !em.send(d) {
				return
			}
		}
	}()
	return ch, nil
}

func (s *Scripted) deltasFor(t Turn, call int, msgs []Message) []Delta {
	if len(t.Deltas) > 0 {
		out := append([]Delta(nil), t.Deltas...)
		if last := out[len(out)-1]; last.Type != DeltaDone && last.Type != DeltaError {
			out = append(out, Delta{Type: DeltaDone, FinishReason: FinishStop})
		}
		return out
	}
	var out []Delta
	idx := 0
	if t.Thinking != "" {
		out = append(out, Delta{Type: DeltaThinking, ContentIndex: idx, Text: t.Thinking})
		idx++
	}
	if t.Text != "" {
		out = append(out, Delta{Type: DeltaText, ContentIndex: idx, Text: t.Text})
		idx++
	}
	for i, tc := range t.ToolCalls {
		if tc.ID == "" {
			tc.ID = fmt.Sprintf("call_%d_%d", call, i)
		}
		if tc.Arguments == nil {
			tc.Arguments = map[string]any{}
		}
		c := tc
		out = append(out, Delta{Type: DeltaToolCall, ContentIndex: idx, ToolCall: &c})
		idx++
	}
	if t.Err != nil {
		return append(out, Delta{Type: DeltaError, FinishReason: FinishError, Err: t.Err})
	}
	if !t.NoUsage {
		u := Usage{}
		if t.Usage != nil {
			u = *t.Usage
		} else {
			u.InputTokens = estimateTokens(msgs)
			u.OutputTokens = (len(t.Text)+len(t.Thinking)+40*len(t.ToolCalls))/4 + 1
		}
		out = append(out, Delta{Type: DeltaUsage, Usage: &u})
	}
	finish := t.Finish
	if finish == "" {
		finish = FinishStop
		if len(t.ToolCalls) > 0 {
			finish = FinishToolUse
		}
	}
	return append(out, Delta{Type: DeltaDone, FinishReason: finish})
}

func estimateTokens(msgs []Message) int {
	n := 0
	for _, m := range msgs {
		n += len(m.Text())
		for _, c := range m.ToolCalls {
			n += len(c.Name) + 20
		}
		for _, r := range m.ToolResults {
			n += len(blocksText(r.Content))
		}
	}
	return n/4 + 1
}
