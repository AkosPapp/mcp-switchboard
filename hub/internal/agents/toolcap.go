package agents

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/llm"
)

// capBlocks bounds the text of one tool result to maxChars characters in total,
// keeping the head and the tail and replacing the middle with a marker that
// tells the model what happened and how to get less. It exists because a single
// result (a recursive directory listing, a large file) can be many times the
// model's context window: the provider then silently drops the oldest messages,
// which can be the user's request itself. Images are left alone. maxChars <= 0
// means no cap. The input is not modified.
func capBlocks(blocks []llm.Block, maxChars int) []llm.Block {
	if maxChars <= 0 {
		return blocks
	}
	total := 0
	for _, b := range blocks {
		if b.Type == llm.BlockText {
			total += utf8.RuneCountInString(b.Text)
		}
	}
	if total <= maxChars {
		return blocks
	}
	out := make([]llm.Block, len(blocks))
	copy(out, blocks)
	remaining := maxChars
	for i, b := range out {
		if b.Type != llm.BlockText {
			continue
		}
		n := utf8.RuneCountInString(b.Text)
		// Split what is left of the budget across the text blocks in order; a
		// block that fits keeps its size, one that does not is shortened.
		share := remaining
		if n <= share {
			remaining -= n
			continue
		}
		out[i].Text = headTail(b.Text, n, share)
		remaining = 0
	}
	return out
}

// headTail keeps about 60% of keep at the start and 40% at the end of text
// (n runes long), cutting on rune boundaries.
func headTail(text string, n, keep int) string {
	if keep < 200 {
		keep = 200
	}
	head := keep * 6 / 10
	tail := keep - head
	runes := []rune(text)
	if head+tail >= len(runes) {
		return text
	}
	omitted := len(runes) - head - tail
	var b strings.Builder
	b.Grow(keep + 300)
	b.WriteString(string(runes[:head]))
	fmt.Fprintf(&b, "\n\n[... %d of %d characters omitted to fit the model's context window. "+
		"Ask for less: a narrower path or filter, a limit, or read it in pages ...]\n\n", omitted, n)
	b.WriteString(string(runes[len(runes)-tail:]))
	return b.String()
}
