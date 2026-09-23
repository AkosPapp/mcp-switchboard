package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/events"
)

type draftBody struct {
	Draft     string  `json:"draft"`
	UpdatedAt *string `json:"updatedAt"`
}

func decodeDraft(t *testing.T, body []byte) draftBody {
	t.Helper()
	var d draftBody
	if err := json.Unmarshal(body, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestDraftRoutes(t *testing.T) {
	o := newOrch(t)
	c := o.chat(t, o.agent(t, "a").ID, "t")
	url := "/api/chats/" + c.ID + "/draft"

	rec := o.do(t, "GET", url, "")
	requireStatus(t, rec, 200)
	if !strings.Contains(rec.Body.String(), `"updatedAt":null`) {
		t.Fatalf("empty draft body: %s", rec.Body.String())
	}
	if d := decodeDraft(t, rec.Body.Bytes()); d.Draft != "" || d.UpdatedAt != nil {
		t.Fatalf("empty draft: %+v", d)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, unsub := o.bus.Subscribe(ctx)
	defer unsub()

	rec = o.do(t, "PUT", url, `{"draft":"half typed"}`)
	requireStatus(t, rec, 200)
	if d := decodeDraft(t, rec.Body.Bytes()); d.Draft != "half typed" || d.UpdatedAt == nil {
		t.Fatalf("put: %+v", d)
	}
	select {
	case ev := <-ch:
		if ev.Type != events.TypeDraft || ev.ChatID != c.ID {
			t.Fatalf("event = %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no chat event after PUT")
	}

	rec = o.do(t, "GET", url, "")
	if d := decodeDraft(t, rec.Body.Bytes()); d.Draft != "half typed" || d.UpdatedAt == nil {
		t.Fatalf("get after put: %+v", d)
	}

	rec = o.do(t, "PUT", url, `{"draft":"  \n"}`)
	requireStatus(t, rec, 200)
	if d := decodeDraft(t, rec.Body.Bytes()); d.Draft != "" || d.UpdatedAt != nil {
		t.Fatalf("whitespace put: %+v", d)
	}
	if d, _ := o.db.GetDraft(context.Background(), c.ID); d.UpdatedAt != nil {
		t.Fatalf("row not deleted: %+v", d)
	}

	// Deleting the chat removes the draft.
	o.do(t, "PUT", url, `{"draft":"x"}`)
	requireStatus(t, o.do(t, "DELETE", "/api/chats/"+c.ID, ""), http.StatusOK)
}

func TestDraftErrors(t *testing.T) {
	o := newOrch(t)
	c := o.chat(t, o.agent(t, "a").ID, "t")
	requireStatus(t, o.do(t, "GET", "/api/chats/nope/draft", ""), 404)
	requireStatus(t, o.do(t, "PUT", "/api/chats/nope/draft", `{"draft":"x"}`), 404)
	requireStatus(t, o.do(t, "PUT", "/api/chats/"+c.ID+"/draft", `{not json`), 400)
	big := `{"draft":"` + strings.Repeat("a", 300<<10) + `"}`
	requireStatus(t, o.do(t, "PUT", "/api/chats/"+c.ID+"/draft", big), http.StatusRequestEntityTooLarge)
}

func TestDraftRoutes404WhenDisabled(t *testing.T) {
	f := newFixture(t, nil)
	requireStatus(t, f.do(t, "GET", "/api/chats/x/draft", ""), 404)
	requireStatus(t, f.do(t, "PUT", "/api/chats/x/draft", `{"draft":"a"}`), 404)
}
