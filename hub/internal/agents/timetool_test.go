package agents

import (
	"errors"
	"testing"
	"time"
)

func TestTimeNow(t *testing.T) {
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	get := func(args map[string]any) map[string]any {
		t.Helper()
		out, err := timeNow(now, args)
		if err != nil {
			t.Fatal(err)
		}
		return out.(map[string]any)
	}
	m := get(map[string]any{"tz": "Europe/Berlin"})
	if m["iso8601"] != "2026-01-15T13:00:00+01:00" || m["weekday"] != "Thursday" || m["utc_offset"] != "+01:00" || m["unix"] != now.Unix() {
		t.Fatalf("%v", m)
	}
	m = get(map[string]any{"tz": "UTC", "add": "2d"})
	if m["iso8601"] != "2026-01-17T12:00:00Z" || m["weekday"] != "Saturday" {
		t.Fatalf("%v", m)
	}
	m = get(map[string]any{"tz": "UTC", "add": "-90m"})
	if m["iso8601"] != "2026-01-15T10:30:00Z" {
		t.Fatalf("%v", m)
	}
	m = get(map[string]any{"convert_from": "2026-07-01 09:00", "from_tz": "America/New_York", "convert_to": "Asia/Tokyo"})
	if m["iso8601"] != "2026-07-01T22:00:00+09:00" || m["utc_offset"] != "+09:00" {
		t.Fatalf("%v", m)
	}
	m = get(map[string]any{"convert_from": "2026-01-01T00:00:00Z", "convert_to": "America/New_York"})
	if m["utc_offset"] != "-05:00" {
		t.Fatalf("%v", m)
	}
	for _, bad := range []map[string]any{
		{"tz": "Mars/Base"}, {"add": "soon"}, {"convert_from": "2026-01-01"}, {"convert_from": "garbage", "convert_to": "UTC"},
	} {
		if _, err := timeNow(now, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%v: %v", bad, err)
		}
	}
	if _, err := timeNow(now, nil); err != nil {
		t.Fatal(err)
	}
}
