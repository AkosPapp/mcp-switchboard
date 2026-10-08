package agents

import (
	"context"
	"testing"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

func TestBridgeChatPostsNeverSchedule(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	chat, err := e.m.CreateChat(ctx, CreateChatInput{Title: "opencode mirror", Bridge: true})
	if err != nil {
		t.Fatal(err)
	}
	if chat.Kind != store.ChatKindBridge {
		t.Fatalf("kind = %s", chat.Kind)
	}
	// A scripted provider is not even wired to this chat's agent; if Post
	// scheduled a run, the missing model config would end it in error - this
	// asserts there is no run at all.
	res, err := e.m.Post(ctx, chat.ID, PostInput{Content: say("from the console")})
	if err != nil {
		t.Fatal(err)
	}
	if res.RunID != "" {
		t.Fatalf("bridge post scheduled a run: %+v", res)
	}
	runs, err := e.st.ListRuns(ctx, store.RunFilter{ChatID: chat.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("runs = %+v", runs)
	}
	path, err := e.st.ActivePath(ctx, chat.ID)
	if err != nil || len(path) != 1 || path[0].Role != store.RoleUser {
		t.Fatalf("path = %+v err=%v", path, err)
	}
}

func TestAppendBridgeRolesSenderAndGuards(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	chat, err := e.m.CreateChat(ctx, CreateChatInput{Title: "mirror", Bridge: true})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := e.m.AppendBridge(ctx, chat.ID, BridgeAppend{Role: "assistant", Text: "edited src/main.go"})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Role != store.RoleAssistant || len(msg.Sender) == 0 {
		t.Fatalf("msg = %+v", msg)
	}
	// Folded tool lines and unknown roles.
	if _, err := e.m.AppendBridge(ctx, chat.ID, BridgeAppend{Role: "tool", Text: "ran tests"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.AppendBridge(ctx, chat.ID, BridgeAppend{Role: "system", Text: "nope"}); err == nil {
		t.Fatal("system role accepted")
	}
	// A normal chat refuses bridge appends.
	plain, err := e.m.CreateChat(ctx, CreateChatInput{Title: "human"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.AppendBridge(ctx, plain.ID, BridgeAppend{Role: "assistant", Text: "x"}); err == nil {
		t.Fatal("append onto a human chat accepted")
	}
}

func TestBridgeQuestionWithoutPushConfigured(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	chat, err := e.m.CreateChat(ctx, CreateChatInput{Title: "mirror", Bridge: true})
	if err != nil {
		t.Fatal(err)
	}
	pushed, err := e.m.BridgeQuestion(ctx, chat.ID, "shall I delete the cache?")
	if err != nil {
		t.Fatal(err)
	}
	if pushed {
		t.Fatal("push reported without configuration")
	}
}

func TestBridgeConversationThreadsOnTheLeaf(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	chat, err := e.m.CreateChat(ctx, CreateChatInput{Title: "threaded", Bridge: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []struct {
		human bool
		text  string
	}{
		{true, "turn one"},
		{false, "reply one"},
		{true, "turn two"},
		{false, "reply two"},
	} {
		if in.human {
			if _, err = e.m.Post(ctx, chat.ID, PostInput{Content: say(in.text)}); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if _, err = e.m.AppendBridge(ctx, chat.ID, BridgeAppend{Role: "assistant", Text: in.text}); err != nil {
			t.Fatal(err)
		}
	}
	// The default GET /messages (ActivePath) is what the console renders and
	// what the plugin's pump polls: every line must be on it, in order.
	path, err := e.st.ActivePath(ctx, chat.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantROLES := []string{store.RoleUser, store.RoleAssistant, store.RoleUser, store.RoleAssistant}
	if len(path) != len(wantROLES) {
		t.Fatalf("path length = %d, want %d: %+v", len(path), len(wantROLES), path)
	}
	for i, m := range path {
		if m.Role != wantROLES[i] {
			t.Errorf("path[%d].Role = %s, want %s", i, m.Role, wantROLES[i])
		}
	}
}
