package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

func TestTodosRoute(t *testing.T) {
	o := newOrch(t)
	c := o.chat(t, o.agent(t, "a").ID, "t")
	url := "/api/chats/" + c.ID + "/todos"

	rec := o.do(t, "GET", url, "")
	requireStatus(t, rec, 200)
	if got := rec.Body.String(); got != "{\"todos\":[]}\n" && got != "{\"todos\":[]}" {
		t.Fatalf("empty body = %q", got)
	}
	if _, err := o.db.ReplaceTodos(context.Background(), c.ID, []store.TodoItem{{Content: "a", Status: "in_progress"}, {Content: "b", Status: "pending"}}); err != nil {
		t.Fatal(err)
	}
	rec = o.do(t, "GET", url, "")
	requireStatus(t, rec, 200)
	var body struct {
		Todos []struct {
			Content string `json:"content"`
			Status  string `json:"status"`
		} `json:"todos"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body.Todos) != 2 || body.Todos[0].Content != "a" || body.Todos[0].Status != "in_progress" {
		t.Fatalf("body = %s (%v)", rec.Body.String(), err)
	}
	requireStatus(t, o.do(t, "GET", "/api/chats/nope/todos", ""), 404)
}
