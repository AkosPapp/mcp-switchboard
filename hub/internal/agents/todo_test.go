package agents

import (
	"errors"
	"strings"
	"testing"
)

func todoArgs(items ...[2]string) map[string]any {
	l := []any{}
	for _, it := range items {
		l = append(l, map[string]any{"content": it[0], "status": it[1]})
	}
	return map[string]any{"todos": l}
}

func TestParseTodos(t *testing.T) {
	got, err := parseTodos(todoArgs([2]string{" a ", "completed"}, [2]string{"b", "in_progress"}, [2]string{"c", "pending"}))
	if err != nil || len(got) != 3 || got[0].Content != "a" || got[1].Status != "in_progress" {
		t.Fatalf("valid list: %+v %v", got, err)
	}
	if got, err := parseTodos(todoArgs()); err != nil || len(got) != 0 {
		t.Fatalf("empty list clears: %+v %v", got, err)
	}
	many := todoArgs()
	for i := 0; i < 51; i++ {
		many["todos"] = append(many["todos"].([]any), map[string]any{"content": "x", "status": "pending"})
	}
	bad := map[string]map[string]any{
		"missing":     {},
		"two active":  todoArgs([2]string{"a", "in_progress"}, [2]string{"b", "in_progress"}),
		"empty":       todoArgs([2]string{"  ", "pending"}),
		"too long":    todoArgs([2]string{strings.Repeat("x", 301), "pending"}),
		"bad status":  todoArgs([2]string{"a", "done"}),
		"51 items":    many,
		"not objects": {"todos": []any{"a"}},
	}
	for name, a := range bad {
		if _, err := parseTodos(a); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	if _, err := parseTodos(todoArgs([2]string{strings.Repeat("é", 300), "pending"})); err != nil {
		t.Errorf("300 runes must be accepted: %v", err)
	}
}

func TestTodoNeedsChatContext(t *testing.T) {
	if _, err := todoChatID(&callCtx{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no run: %v", err)
	}
	if id, err := todoChatID(&callCtx{rs: &runState{chatID: "c1"}}); err != nil || id != "c1" {
		t.Fatalf("with run: %q %v", id, err)
	}
	if todoWriteTool.ann.ReadOnlyHint || todoWriteTool.ann.DestructiveHint == nil || *todoWriteTool.ann.DestructiveHint {
		t.Fatal("todo.write annotations must be non-destructive, not read-only")
	}
}
