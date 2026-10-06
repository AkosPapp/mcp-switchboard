package agents

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSearxSearch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" || r.URL.Query().Get("q") != "go" || r.URL.Query().Get("format") != "json" || r.URL.Query().Get("pageno") != "2" {
			t.Errorf("unexpected request %s", r.URL)
		}
		w.Write([]byte(`{"results":[{"title":"A","url":"http://a","content":"x"},{"title":"B","url":"http://b","content":"y"}]}`))
	}))
	defer srv.Close()
	out, err := searxSearch(context.Background(), srv.URL, map[string]any{"query": "go", "page": 2.0, "limit": 1.0})
	if err != nil {
		t.Fatal(err)
	}
	res := out.(map[string]any)["results"].([]map[string]any)
	if len(res) != 1 || res[0]["title"] != "A" {
		t.Fatalf("got %v", res)
	}
	if _, err := searxSearch(context.Background(), srv.URL, map[string]any{}); err == nil {
		t.Fatal("empty query accepted")
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) }))
	defer bad.Close()
	if _, err := searxSearch(context.Background(), bad.URL, map[string]any{"query": "x"}); err == nil {
		t.Fatal("403 accepted")
	}
}
