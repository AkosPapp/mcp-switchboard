package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

// exportChat serves GET /api/chats/{id}/export?format=json|markdown (U12).
// json is the whole DAG (every branch); markdown is the active path only.
func (h *Handler) exportChat(w http.ResponseWriter, r *http.Request) {
	c := h.loadChat(w, r)
	if c == nil {
		return
	}
	ctx := r.Context()
	format := strings.ToLower(clean(r.URL.Query().Get("format")))
	if format == "" {
		format = "json"
	}
	switch format {
	case "json":
		all, err := h.rd.ListMessages(ctx, c.ID)
		if err != nil {
			h.writeErr(w, err)
			return
		}
		views, err := viewTree(all)
		if err != nil {
			h.writeErr(w, err)
			return
		}
		// The system prompt and tool catalog are not stored on the chat itself
		// (they are resolved live from the profile/agent, same as the Tools and
		// System prompt panels, so the export can only carry a snapshot of them
		// as of the moment it was generated). Best-effort: an export must not
		// fail just because the resolving record went away since the chat was
		// last active.
		out := map[string]any{"chat": c, "messages": views}
		if sp, err := h.opts.Agents.ChatSystemPrompt(ctx, c.ID); err == nil {
			out["systemPrompt"] = sp
		}
		if tools, err := h.opts.Agents.ChatTools(ctx, c.ID); err == nil {
			out["tools"] = tools
		}
		w.Header().Set("Content-Disposition", `attachment; filename="chat-`+c.ID+`.json"`)
		writeJSON(w, http.StatusOK, out)
	case "markdown", "md":
		path, err := h.rd.ActivePath(ctx, c.ID)
		if err != nil {
			h.writeErr(w, err)
			return
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="chat-`+c.ID+`.md"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(renderMarkdown(*c, path)))
	default:
		writeError(w, http.StatusBadRequest, "format must be json or markdown")
	}
}

func renderMarkdown(c store.Chat, path []store.Message) string {
	var sb strings.Builder
	title := c.Title
	if title == "" {
		title = "Chat " + c.ID
	}
	fmt.Fprintf(&sb, "# %s\n\n", title)
	for _, m := range path {
		heading := strings.ToUpper(m.Role[:1]) + m.Role[1:]
		// An injected message was not typed by the human: say who it came from.
		if from := senderLabel(m.Sender); from != "" {
			heading = from
		}
		fmt.Fprintf(&sb, "## %s\n\n", heading)
		if text := blocksText(m.Content); text != "" {
			sb.WriteString(text + "\n\n")
		}
		var calls []struct {
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if json.Unmarshal(m.ToolCalls, &calls) == nil {
			for _, tc := range calls {
				fmt.Fprintf(&sb, "**Tool call** `%s`\n\n```json\n%s\n```\n\n", tc.Name, indent(tc.Arguments))
			}
		}
		var results []struct {
			Result json.RawMessage `json:"result"`
			Error  *string         `json:"error"`
		}
		if json.Unmarshal(m.ToolResults, &results) == nil {
			for _, tr := range results {
				if tr.Error != nil && *tr.Error != "" {
					fmt.Fprintf(&sb, "**Tool error**\n\n```\n%s\n```\n\n", *tr.Error)
				} else {
					fmt.Fprintf(&sb, "**Tool result**\n\n```json\n%s\n```\n\n", indent(tr.Result))
				}
			}
		}
	}
	return sb.String()
}

func indent(raw json.RawMessage) string {
	var v any
	if len(raw) == 0 || json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}

// blocksText flattens text blocks; other blocks become a placeholder.
func blocksText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		case "image":
			parts = append(parts, "_[image]_")
		}
	}
	return strings.Join(parts, "\n\n")
}

// senderLabel names the chat an injected user-role message came from, or "" for
// a message the human wrote (Sender is null).
func senderLabel(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s struct {
		ChatID    string `json:"chatId"`
		ChatTitle string `json:"chatTitle"`
		Kind      string `json:"kind"`
	}
	if json.Unmarshal(raw, &s) != nil || s.ChatID == "" {
		return ""
	}
	title := s.ChatTitle
	if title == "" {
		title = s.ChatID
	}
	kind := map[string]string{"reply": "Reply", "spawn": "Task"}[s.Kind]
	if kind == "" {
		kind = "Message"
	}
	return fmt.Sprintf("%s from chat \"%s\"", kind, title)
}
