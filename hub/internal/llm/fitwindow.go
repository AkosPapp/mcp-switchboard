package llm

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"unicode/utf8"
)

// When a prompt is longer than the context window, Ollama does not fail: it
// silently drops the OLDEST messages and carries on. The oldest message is the
// user's request, and chat templates that look for the last user query (Qwen's
// does) then abort with "No user query found in messages", or worse, the model
// answers a conversation whose question it never saw.
//
// fitToWindow keeps that from happening by making the prompt smaller first, in
// the one way that loses the least: old tool results, which are bulky and the
// least likely to matter, are shortened. User messages, the assistant's own
// words and tool calls are never touched.

const (
	// charsPerToken is deliberately pessimistic (JSON and code tokenize at 3 to 4
	// characters per token); over-estimating only shrinks results a little more.
	charsPerToken = 3
	// windowFill is how much of the window the prompt may use, leaving the rest
	// for the reply.
	windowNum, windowDen = 85, 100
	// shrunkResultChars is what an old tool result is cut down to.
	shrunkResultChars = 800
	// minLatestChars is the floor for the most recent results.
	minLatestChars = 2000
	// markerAllowance is the length of the omission note appended to a cut result.
	markerAllowance = 200
)

// fitToWindow returns msgs, with tool results shortened as needed so that an
// estimate of the whole prompt (system, tool definitions, messages) fits in
// numCtx. It returns msgs itself when nothing needs to change, and never
// modifies the caller's slices.
func fitToWindow(msgs []Message, tools []Tool, system string, numCtx int) []Message {
	if numCtx <= 0 {
		return msgs
	}
	budget := numCtx * windowNum / windowDen * charsPerToken
	fixed := utf8.RuneCountInString(system) + toolsChars(tools)
	used := fixed
	for _, m := range msgs {
		used += messageChars(m)
	}
	if used <= budget {
		return msgs
	}

	out := make([]Message, len(msgs))
	copy(out, msgs)

	// Position of the last tool message: those results are what the model is
	// working on right now, so they are shortened last.
	last := -1
	for i, m := range out {
		if m.Role == RoleTool {
			last = i
		}
	}

	shrink := func(i int, keep int) {
		m := out[i]
		results := make([]ToolResult, len(m.ToolResults))
		copy(results, m.ToolResults)
		for j, r := range results {
			blocks := make([]Block, len(r.Content))
			copy(blocks, r.Content)
			for k, b := range blocks {
				if b.Type != BlockText {
					continue
				}
				n := utf8.RuneCountInString(b.Text)
				if n <= keep {
					continue
				}
				used -= n
				blocks[k].Text = shrunkText(b.Text, n, keep)
				used += utf8.RuneCountInString(blocks[k].Text)
			}
			results[j].Content = blocks
		}
		m.ToolResults = results
		out[i] = m
	}

	// Oldest first, sparing the latest results for now.
	for i := 0; i < len(out) && used > budget; i++ {
		if out[i].Role == RoleTool && i != last {
			shrink(i, shrunkResultChars)
		}
	}
	// Still too big: cut the latest results to whatever room is left.
	if used > budget && last >= 0 {
		room := budget - (used - toolResultChars(out[last]))
		// The omission marker is added on top of what is kept, so leave room for it.
		shrink(last, max(room-markerAllowance, minLatestChars))
	}
	if used > budget {
		slog.Warn("llm: the prompt still exceeds the context window after shortening tool results; the server may drop messages",
			"estimated_tokens", used/charsPerToken, "num_ctx", numCtx)
	} else {
		slog.Warn("llm: shortened tool results to fit the context window", "num_ctx", numCtx)
	}
	return out
}

func shrunkText(text string, n, keep int) string {
	runes := []rune(text)
	if keep >= len(runes) {
		return text
	}
	return string(runes[:keep]) + fmt.Sprintf("\n[... %d more characters omitted to fit the context window; ask for less or read it in pages ...]", n-keep)
}

func messageChars(m Message) int {
	n := 0
	for _, b := range m.Content {
		n += utf8.RuneCountInString(b.Text)
	}
	for _, c := range m.ToolCalls {
		raw, _ := json.Marshal(c.Arguments)
		n += utf8.RuneCountInString(c.Name) + len(raw)
	}
	return n + toolResultChars(m)
}

func toolResultChars(m Message) int {
	n := 0
	for _, r := range m.ToolResults {
		for _, b := range r.Content {
			n += utf8.RuneCountInString(b.Text)
		}
	}
	return n
}

func toolsChars(tools []Tool) int {
	raw, _ := json.Marshal(tools)
	return len(raw)
}
