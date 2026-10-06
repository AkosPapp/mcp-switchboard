package store

import (
	"context"
	"errors"
	"testing"
)

func TestTodosReplaceListCascade(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, tempDB(t))
	c := mkChat(t, s, mkAgent(t, s, nil, "a").ID)

	if got, err := s.ListTodos(ctx, c.ID); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty list = %#v %v", got, err)
	}
	got, err := s.ReplaceTodos(ctx, c.ID, []TodoItem{{"one", TodoInProgress}, {"two", TodoPending}})
	if err != nil || len(got) != 2 || got[0].Content != "one" || got[1].Status != TodoPending {
		t.Fatalf("replace = %+v %v", got, err)
	}
	got, err = s.ReplaceTodos(ctx, c.ID, []TodoItem{{"two", TodoCompleted}})
	if err != nil || len(got) != 1 || got[0].Content != "two" || got[0].Status != TodoCompleted {
		t.Fatalf("second replace = %+v %v", got, err)
	}
	if _, err := s.ReplaceTodos(ctx, "nope", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown chat: %v", err)
	}
	if _, err := s.ReplaceTodos(ctx, c.ID, []TodoItem{{"x", "bogus"}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad status: %v", err)
	}
	if got, _ := s.ListTodos(ctx, c.ID); len(got) != 1 {
		t.Fatalf("failed replace must leave the list alone: %+v", got)
	}
	if got, err := s.ReplaceTodos(ctx, c.ID, nil); err != nil || len(got) != 0 {
		t.Fatalf("clear = %+v %v", got, err)
	}
	if _, err := s.ReplaceTodos(ctx, c.ID, []TodoItem{{"again", TodoPending}}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteChat(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	s.read.QueryRow("SELECT COUNT(*) FROM chat_todos").Scan(&n)
	if n != 0 {
		t.Fatalf("todos survived chat delete: %d", n)
	}
}
