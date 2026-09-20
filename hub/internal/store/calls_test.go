package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func ptr(s string) *string { return &s }

func sampleCall(id string, started time.Time, mutate func(*CallRecord)) CallRecord {
	rec := CallRecord{
		ID:           id,
		ConnectionID: "conn-1",
		Label:        "laptop",
		Server:       "harness",
		Tool:         "read_file",
		ExposedName:  "laptop__harness__read_file",
		Arguments:    map[string]any{"path": "/etc/hosts"},
		Result:       map[string]any{"content": "127.0.0.1"},
		Status:       StatusOK,
		Source:       SourceAPI,
		StartedAt:    started,
		DurationMs:   12.5,
	}
	if mutate != nil {
		mutate(&rec)
	}
	return rec
}

func TestRecordGetRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, tempDB(t))

	started := time.Date(2026, 3, 4, 5, 6, 7, 123456000, time.UTC)
	want := sampleCall("abc", started, func(r *CallRecord) {
		r.Status = StatusDenied
		r.Source = SourceAgent
		r.Error = "not permitted: laptop//harness"
		r.Result = nil
		r.AgentID = ptr("agent-1")
		r.ChatID = ptr("chat-1")
		r.RunID = ptr("run-1")
	})
	if err := s.RecordCall(ctx, want); err != nil {
		t.Fatalf("RecordCall: %v", err)
	}

	got, err := s.GetCall(ctx, "abc")
	if err != nil {
		t.Fatalf("GetCall: %v", err)
	}
	if got == nil {
		t.Fatal("GetCall returned nothing for a call just recorded")
	}
	if !got.StartedAt.Equal(started) {
		t.Errorf("StartedAt = %v, want %v", got.StartedAt, started)
	}
	if got.Arguments["path"] != "/etc/hosts" {
		t.Errorf("Arguments = %v", got.Arguments)
	}
	if got.Result != nil {
		t.Errorf("Result = %v, want nil", got.Result)
	}
	if got.Error != want.Error || got.Status != StatusDenied || got.Source != SourceAgent {
		t.Errorf("status/source/error round-trip wrong: %+v", got)
	}
	for name, pair := range map[string][2]*string{
		"agentId": {got.AgentID, want.AgentID},
		"chatId":  {got.ChatID, want.ChatID},
		"runId":   {got.RunID, want.RunID},
	} {
		if pair[0] == nil || *pair[0] != *pair[1] {
			t.Errorf("%s did not round-trip: %v", name, pair[0])
		}
	}

	missing, err := s.GetCall(ctx, "nope")
	if err != nil || missing != nil {
		t.Fatalf("GetCall for an unknown id = %v, %v; want nil, nil", missing, err)
	}
}

func TestCallJSONShape(t *testing.T) {
	started := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	rec := sampleCall("abc", started, nil)

	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	want := []string{
		"id", "connectionId", "label", "server", "tool", "exposedName", "arguments",
		"result", "error", "status", "source", "startedAt", "durationMs",
	}
	for _, key := range want {
		if _, ok := got[key]; !ok {
			t.Errorf("payload is missing %q", key)
		}
	}
	if len(got) != len(want) {
		t.Errorf("payload has %d keys, want %d (nil agent/chat/run must be omitted): %v", len(got), len(want), got)
	}
	if got["startedAt"] != "2026-03-04T05:06:07+00:00" {
		t.Errorf("startedAt = %v, want an explicit +00:00 offset and no Z", got["startedAt"])
	}
	if got["error"] != nil {
		t.Errorf("error = %v, want null", got["error"])
	}

	rec.StartedAt = started.Add(123456 * time.Microsecond)
	rec.AgentID = ptr("agent-1")
	data, _ = json.Marshal(rec)
	if !strings.Contains(string(data), `"startedAt":"2026-03-04T05:06:07.123456+00:00"`) {
		t.Errorf("fractional seconds do not match Python's isoformat: %s", data)
	}
	if !strings.Contains(string(data), `"agentId":"agent-1"`) {
		t.Errorf("agentId missing when set: %s", data)
	}
}

func TestListFiltersOrderingAndLimit(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, tempDB(t))

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 20; i++ {
		rec := sampleCall(fmt.Sprintf("call-%02d", i), base.Add(time.Duration(i)*time.Minute), func(r *CallRecord) {
			if i%2 == 0 {
				r.Server = "other"
				r.Tool = "write_file"
				r.Label = "desktop"
			}
			if i%5 == 0 {
				r.Status = StatusError
				r.Source = SourceConsole
			}
			if i%4 == 0 {
				r.AgentID = ptr("agent-A")
				r.ChatID = ptr("chat-A")
				r.Source = SourceAgent
			}
		})
		if err := s.RecordCall(ctx, rec); err != nil {
			t.Fatalf("RecordCall: %v", err)
		}
	}

	all, err := s.ListCalls(ctx, CallFilter{Limit: 100})
	if err != nil {
		t.Fatalf("ListCalls: %v", err)
	}
	if len(all) != 20 {
		t.Fatalf("got %d rows, want 20", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i-1].StartedAt.Before(all[i].StartedAt) {
			t.Fatalf("rows are not newest first at %d", i)
		}
	}
	if all[0].ID != "call-19" {
		t.Errorf("newest row is %s, want call-19", all[0].ID)
	}

	for name, f := range map[string]CallFilter{
		"server":  {Server: "other"},
		"tool":    {Tool: "write_file"},
		"label":   {Label: "desktop"},
		"status":  {Status: StatusError},
		"source":  {Source: SourceAgent},
		"agentId": {AgentID: "agent-A"},
		"chatId":  {ChatID: "chat-A"},
	} {
		got, err := s.ListCalls(ctx, f)
		if err != nil {
			t.Fatalf("ListCalls(%s): %v", name, err)
		}
		if len(got) == 0 || len(got) == 20 {
			t.Errorf("filter %s selected %d of 20 rows, which filters nothing", name, len(got))
		}
	}

	// Filters combine with AND, and an unknown value matches nothing.
	combined, err := s.ListCalls(ctx, CallFilter{Server: "other", Status: StatusError})
	if err != nil {
		t.Fatalf("ListCalls combined: %v", err)
	}
	for _, rec := range combined {
		if rec.Server != "other" || rec.Status != StatusError {
			t.Fatalf("combined filter leaked a row: %+v", rec)
		}
	}
	none, err := s.ListCalls(ctx, CallFilter{Server: "no-such-server"})
	if err != nil || len(none) != 0 {
		t.Fatalf("unknown filter value returned %d rows (err %v)", len(none), err)
	}

	// Limit clamp and paging.
	clamped, err := s.ListCalls(ctx, CallFilter{Limit: 5_000_000})
	if err != nil {
		t.Fatalf("ListCalls clamped: %v", err)
	}
	if len(clamped) != 20 {
		t.Fatalf("clamped list returned %d rows", len(clamped))
	}
	page, err := s.ListCalls(ctx, CallFilter{Limit: 5, Offset: 5})
	if err != nil {
		t.Fatalf("ListCalls paged: %v", err)
	}
	if len(page) != 5 || page[0].ID != all[5].ID {
		t.Fatalf("paging is not stable: %v", page)
	}
}

// Identical timestamps must still yield a total order, or a row moves between
// pages and is returned twice.
func TestListOrderIsTotalWhenTimestampsCollide(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, tempDB(t))

	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		if err := s.RecordCall(ctx, sampleCall(fmt.Sprintf("c%02d", i), at, nil)); err != nil {
			t.Fatalf("RecordCall: %v", err)
		}
	}

	seen := map[string]bool{}
	for offset := 0; offset < 10; offset += 3 {
		page, err := s.ListCalls(ctx, CallFilter{Limit: 3, Offset: offset})
		if err != nil {
			t.Fatalf("ListCalls: %v", err)
		}
		for _, rec := range page {
			if seen[rec.ID] {
				t.Fatalf("pagination repeated %s", rec.ID)
			}
			seen[rec.ID] = true
		}
	}
	if len(seen) != 10 {
		t.Fatalf("pagination saw %d of 10 rows", len(seen))
	}
}

func TestPurgeByAge(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, Options{Path: tempDB(t), RetentionDays: 7, MaxRows: -1})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	now := time.Now().UTC()
	for i, age := range []time.Duration{time.Hour, 3 * 24 * time.Hour, 8 * 24 * time.Hour, 90 * 24 * time.Hour} {
		if err := s.RecordCall(ctx, sampleCall(fmt.Sprintf("c%d", i), now.Add(-age), nil)); err != nil {
			t.Fatalf("RecordCall: %v", err)
		}
	}

	deleted, err := s.PurgeCalls(ctx)
	if err != nil {
		t.Fatalf("PurgeCalls: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("purged %d rows, want 2", deleted)
	}
	left, _ := s.ListCalls(ctx, CallFilter{})
	if len(left) != 2 {
		t.Fatalf("%d rows survived, want 2", len(left))
	}

	// Nothing left to do on a second sweep.
	if again, err := s.PurgeCalls(ctx); err != nil || again != 0 {
		t.Fatalf("second purge deleted %d rows (err %v)", again, err)
	}
}

func TestPurgeByRowCount(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, Options{Path: tempDB(t), RetentionDays: -1, MaxRows: 10})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 25; i++ {
		if err := s.RecordCall(ctx, sampleCall(fmt.Sprintf("c%02d", i), base.Add(time.Duration(i)*time.Minute), nil)); err != nil {
			t.Fatalf("RecordCall: %v", err)
		}
	}

	deleted, err := s.PurgeCalls(ctx)
	if err != nil {
		t.Fatalf("PurgeCalls: %v", err)
	}
	if deleted != 15 {
		t.Fatalf("purged %d rows, want 15", deleted)
	}
	left, _ := s.ListCalls(ctx, CallFilter{})
	if len(left) != 10 || left[0].ID != "c24" || left[9].ID != "c15" {
		t.Fatalf("wrong rows survived the row cap: %d rows, newest %s", len(left), left[0].ID)
	}
}

func TestStats(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, tempDB(t))

	now := time.Now().UTC()
	for i := 0; i < 7; i++ {
		rec := sampleCall(fmt.Sprintf("c%d", i), now, func(r *CallRecord) {
			switch {
			case i < 4:
				r.Status = StatusOK
			case i < 6:
				r.Status = StatusError
			default:
				r.Status = StatusDenied
			}
		})
		if err := s.RecordCall(ctx, rec); err != nil {
			t.Fatalf("RecordCall: %v", err)
		}
	}

	stats, err := s.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Calls.Total != 7 || stats.Calls.OK != 4 || stats.Calls.Error != 2 {
		t.Errorf("call counts = %+v", stats.Calls)
	}
	if stats.DatabaseBytes <= 0 {
		t.Errorf("DatabaseBytes = %d, want the file's size on disk", stats.DatabaseBytes)
	}
	if stats.Tables["calls"] != 7 {
		t.Errorf("Tables[calls] = %d, want 7", stats.Tables["calls"])
	}
	if _, ok := stats.Tables["schema_migrations"]; !ok {
		t.Errorf("Tables should cover every table, got %v", stats.Tables)
	}
}

// The single write connection is the whole point of D3: concurrent writers must
// serialise rather than collide, while readers run alongside them.
func TestConcurrentWritersAndReaders(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, tempDB(t))

	const writers, perWriter = 8, 25
	var wg sync.WaitGroup
	errs := make(chan error, writers*2)

	now := time.Now().UTC()
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				id := fmt.Sprintf("w%d-c%d", w, i)
				if err := s.RecordCall(ctx, sampleCall(id, now.Add(time.Duration(i)*time.Millisecond), nil)); err != nil {
					errs <- err
					return
				}
			}
		}(w)

		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				if _, err := s.ListCalls(ctx, CallFilter{Limit: 10}); err != nil {
					errs <- err
					return
				}
				if _, err := s.CallStats(ctx); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent access failed: %v", err)
	}

	var n int64
	if err := s.read.QueryRowContext(ctx, "SELECT COUNT(*) FROM calls").Scan(&n); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if n != writers*perWriter {
		t.Fatalf("stored %d rows, want %d", n, writers*perWriter)
	}
}
