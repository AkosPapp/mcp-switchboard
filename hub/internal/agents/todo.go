package agents

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

const (
	todoWriteName = "switchboard.todo.write"
	todoReadName  = "switchboard.todo.read"
	todoMaxItems  = 50
	todoMaxLen    = 300
)

var todoAnn = &mcp.ToolAnnotations{DestructiveHint: bp(false), OpenWorldHint: bp(false)}

var todoWriteTool = &sbTool{
	name:  todoWriteName,
	alias: "todowrite",
	desc: "Maintain your plan for this chat as a TODO list that the user sees live. Use it for any task with 3 or more steps (skip it for trivial one-step requests). " +
		"Rewrite the WHOLE list on every call: it replaces the previous list, so include items that are already completed. " +
		"Keep exactly one item in_progress while you are working (none only when everything is pending or done), " +
		"and mark an item completed as soon as it is done, not in a batch at the end. Add newly discovered steps and drop ones that no longer apply. " +
		"At most 50 items, each 1-300 characters. Statuses: pending, in_progress, completed. " +
		`Example: {"todos":[{"content":"Read the config loader","status":"completed"},{"content":"Add the new setting","status":"in_progress"},{"content":"Update the tests","status":"pending"}]}`,
	schema: obj([]string{"todos"}, map[string]any{
		"todos": map[string]any{
			"type":        "array",
			"description": "the complete list, in order",
			"maxItems":    todoMaxItems,
			"items": obj([]string{"content", "status"}, map[string]any{
				"content": map[string]any{"type": "string", "description": "what to do, imperative and short", "maxLength": todoMaxLen},
				"status":  map[string]any{"type": "string", "enum": []string{store.TodoPending, store.TodoInProgress, store.TodoCompleted}},
			}),
		},
	}),
	ann:     todoAnn,
	visible: func(store.Capabilities) bool { return true },
	run: func(m *Manager, ctx context.Context, cc *callCtx, args map[string]any) (any, error) {
		chatID, err := todoChatID(cc)
		if err != nil {
			return nil, err
		}
		items, err := parseTodos(args)
		if err != nil {
			return nil, err
		}
		got, err := m.st.ReplaceTodos(ctx, chatID, items)
		if err != nil {
			return nil, err
		}
		m.publish(eventChat(cc.rs))
		return todoResult(got), nil
	},
}

var todoReadTool = &sbTool{
	name:    todoReadName,
	desc:    "Return this chat's current TODO list (as last written with switchboard.todo.write).",
	schema:  obj(nil, map[string]any{}),
	ann:     &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: bp(false)},
	visible: func(store.Capabilities) bool { return true },
	run: func(m *Manager, ctx context.Context, cc *callCtx, _ map[string]any) (any, error) {
		chatID, err := todoChatID(cc)
		if err != nil {
			return nil, err
		}
		got, err := m.st.ListTodos(ctx, chatID)
		if err != nil {
			return nil, err
		}
		return todoResult(got), nil
	},
}

func init() {
	registerExtraTool(todoWriteTool, nil)
	registerExtraTool(todoReadTool, nil)
}

func todoChatID(cc *callCtx) (string, error) {
	if cc == nil || cc.rs == nil || cc.rs.chatID == "" {
		return "", fmt.Errorf("%w: the todo list belongs to a chat, and this call has no chat context", ErrInvalid)
	}
	return cc.rs.chatID, nil
}

// parseTodos validates the whole-list argument of todo.write.
func parseTodos(args map[string]any) ([]store.TodoItem, error) {
	raw, ok := args["todos"].([]any)
	if !ok {
		return nil, fmt.Errorf("%w: todos must be an array of {content, status}", ErrInvalid)
	}
	if len(raw) > todoMaxItems {
		return nil, fmt.Errorf("%w: at most %d todos, got %d", ErrInvalid, todoMaxItems, len(raw))
	}
	items := make([]store.TodoItem, 0, len(raw))
	inProgress := 0
	for i, r := range raw {
		o, ok := r.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%w: todos[%d] must be an object with content and status", ErrInvalid, i)
		}
		content := strings.TrimSpace(argStr(o, "content"))
		if content == "" {
			return nil, fmt.Errorf("%w: todos[%d].content must not be empty", ErrInvalid, i)
		}
		if utf8.RuneCountInString(content) > todoMaxLen {
			return nil, fmt.Errorf("%w: todos[%d].content is longer than %d characters", ErrInvalid, i, todoMaxLen)
		}
		status := argStr(o, "status")
		switch status {
		case store.TodoPending, store.TodoCompleted:
		case store.TodoInProgress:
			inProgress++
		default:
			return nil, fmt.Errorf("%w: todos[%d].status must be pending, in_progress or completed", ErrInvalid, i)
		}
		items = append(items, store.TodoItem{Content: content, Status: status})
	}
	if inProgress > 1 {
		return nil, fmt.Errorf("%w: at most one todo may be in_progress, got %d", ErrInvalid, inProgress)
	}
	return items, nil
}

func todoResult(ts []store.Todo) map[string]any {
	rows := make([]map[string]any, 0, len(ts))
	done := 0
	for _, t := range ts {
		rows = append(rows, map[string]any{"content": t.Content, "status": t.Status})
		if t.Status == store.TodoCompleted {
			done++
		}
	}
	return map[string]any{"todos": rows, "completed": done, "total": len(ts)}
}
