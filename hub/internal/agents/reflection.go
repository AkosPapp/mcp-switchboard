package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

// Self-reflection tools (capability pass, Oct 2026): the model cannot see the
// shape of its own transcript or history from the inside — these two give it
// the two views it actually acts on: how its tool calls have gone (friction)
// and what was ever said in this chat, including what compaction folded away.

const (
	callsStatsName = "switchboard.calls.stats"
	chatSearchName = "switchboard.chat.search"
	statsCallLimit = 200
)

var callsStatsTool = &sbTool{
	name: callsStatsName,
	desc: "How your tool calls in this chat have gone, as numbers: counts and errors per tool, calls you repeated with identical arguments (usually a habit worth breaking), your slowest calls, and your most recent errors. " +
		"Read it before you re-run the expensive thing for the fourth time. Example: calls.stats {}",
	schema:  obj(nil, map[string]any{}),
	ann:     &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: bp(false)},
	visible: func(store.Capabilities) bool { return true },
	run: func(m *Manager, ctx context.Context, cc *callCtx, _ map[string]any) (any, error) {
		if cc == nil || cc.rs == nil || cc.rs.chatID == "" {
			return nil, fmt.Errorf("%w: call statistics belong to a chat, and this call has no chat context", ErrInvalid)
		}
		rows, err := m.st.ListCalls(ctx, store.CallFilter{ChatID: cc.rs.chatID, Limit: statsCallLimit})
		if err != nil {
			return nil, err
		}
		return map[string]any{"summary": renderCallStats(rows)}, nil
	},
}

var chatSearchTool = &sbTool{
	name: chatSearchName,
	desc: "Search every message EVER written in this chat — including branches you are not on and text that context compression folded away (those live in the store, not in your prompt). " +
		"Returns message ids and snippets, newest first. Use it before rediscovering a decision or re-reading a file the history already contains. Example: chat.search {query: \"contextLimit must not be negative\"}",
	schema: obj([]string{"query"}, map[string]any{
		"query": map[string]any{"type": "string", "description": "words to find (full-text; a plain phrase works best)"},
		"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 20, "description": "hits to return, default 8"},
	}),
	ann:     &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: bp(false)},
	visible: func(store.Capabilities) bool { return true },
	run: func(m *Manager, ctx context.Context, cc *callCtx, args map[string]any) (any, error) {
		if cc == nil || cc.rs == nil || cc.rs.chatID == "" {
			return nil, fmt.Errorf("%w: history search belongs to a chat, and this call has no chat context", ErrInvalid)
		}
		q := strings.TrimSpace(argStr(args, "query"))
		if len(q) < 2 {
			return nil, fmt.Errorf("%w: query must be at least two characters", ErrInvalid)
		}
		limit := 8
		if f, ok := args["limit"].(float64); ok && f >= 1 {
			limit = int(f)
			if limit > 20 {
				limit = 20
			}
		}
		hits, err := m.st.SearchMessages(ctx, store.SearchQuery{Text: q, ChatID: cc.rs.chatID, Limit: limit})
		if err != nil {
			return nil, err
		}
		out := make([]map[string]any, 0, len(hits))
		for _, h := range hits {
			snippet := strings.NewReplacer(store.SnippetStart, "", store.SnippetEnd, "").Replace(h.Snippet)
			out = append(out, map[string]any{"messageId": h.MessageID, "role": h.Role, "snippet": snippet})
		}
		return map[string]any{"hits": out, "total": len(out)}, nil
	},
}

func init() {
	registerExtraTool(callsStatsTool, nil)
	registerExtraTool(chatSearchTool, nil)
}

// renderCallStats turns a chat's call records into the compact text the model
// reads. Pure so it can be tested directly.
func renderCallStats(rows []store.CallRecord) string {
	if len(rows) == 0 {
		return "no tool calls recorded in this chat yet."
	}
	type agg struct {
		n, errs int
		ms      float64
	}
	byTool := map[string]*agg{}
	freq := map[string]int{}
	example := map[string]string{}
	var failures []string
	var slowest []store.CallRecord
	errored := 0
	for _, r := range rows {
		a := byTool[r.Tool]
		if a == nil {
			a = &agg{}
			byTool[r.Tool] = a
		}
		a.n++
		a.ms += r.DurationMs
		if r.Status != store.StatusOK {
			a.errs++
			errored++
			if len(failures) < 3 {
				e := r.Error
				if len(e) > 160 {
					e = e[:160] + "…"
				}
				failures = append(failures, fmt.Sprintf("%s: %s", r.Tool, e))
			}
		}
		key := r.Tool + " " + argsFingerprint(r.Arguments)
		freq[key]++
		if _, seen := example[key]; !seen {
			example[key] = key
		}
		if len(slowest) < 3 {
			slowest = append(slowest, r)
		} else if i := worstIdx(slowest, r); i >= 0 {
			slowest[i] = r
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d calls, %d errors (the newest %d calls of this chat).", len(rows), errored, statsCallLimit)
	fmt.Fprintf(&b, "\nper tool:")
	type line struct {
		tool string
		a    *agg
	}
	lines := make([]line, 0, len(byTool))
	for t, a := range byTool {
		lines = append(lines, line{t, a})
	}
	sort.Slice(lines, func(i, j int) bool {
		if lines[i].a.n != lines[j].a.n {
			return lines[i].a.n > lines[j].a.n
		}
		return lines[i].tool < lines[j].tool
	})
	for i, l := range lines {
		if i >= 8 {
			fmt.Fprintf(&b, " …")
			break
		}
		fmt.Fprintf(&b, " %s: %d (%.0fms avg, %d err)", l.tool, l.a.n, l.a.ms/float64(l.a.n), l.a.errs)
	}
	type rep struct {
		key string
		n   int
	}
	var reps []rep
	for k, n := range freq {
		if n > 1 {
			reps = append(reps, rep{k, n})
		}
	}
	sort.Slice(reps, func(i, j int) bool { return reps[i].n > reps[j].n })
	if len(reps) > 0 {
		b.WriteString("\nrepeated with identical arguments:")
		for i, r := range reps {
			if i >= 4 {
				break
			}
			e := r.key
			if len(e) > 140 {
				e = e[:140] + "…"
			}
			fmt.Fprintf(&b, "\n  x%d  %s", r.n, e)
		}
	}
	sort.Slice(slowest, func(i, j int) bool { return slowest[i].DurationMs > slowest[j].DurationMs })
	b.WriteString("\nslowest:")
	for _, r := range slowest {
		fmt.Fprintf(&b, " %s %.1fs", r.Tool, r.DurationMs/1000)
	}
	if len(failures) > 0 {
		b.WriteString("\nrecent errors:")
		for _, f := range failures {
			fmt.Fprintf(&b, "\n  - %s", f)
		}
	}
	return b.String()
}

// argsFingerprint is a stable compact rendering of a call's arguments for the
// repeat detector (JSON has no key order from a map, so sort it).
func argsFingerprint(a map[string]any) string {
	if len(a) == 0 {
		return ""
	}
	keys := make([]string, 0, len(a))
	for k := range a {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	compact := map[string]any{}
	for _, k := range keys {
		v := a[k]
		if s, ok := v.(string); ok && len(s) > 60 {
			s = s[:60] + "…"
		}
		compact[k] = v
	}
	b, err := json.Marshal(compact)
	if err != nil {
		return ""
	}
	return string(b)
}

// worstIdx returns the index of the kept row the newcomer r should replace
// (the current minimum, when r outranks every kept row), else -1.
func worstIdx(rows []store.CallRecord, r store.CallRecord) int {
	min := 0
	for i, x := range rows {
		if x.DurationMs >= r.DurationMs {
			return -1
		}
		if x.DurationMs < rows[min].DurationMs {
			min = i
		}
	}
	return min
}
