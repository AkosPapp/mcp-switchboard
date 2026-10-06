package agents

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestHTTPGet(t *testing.T) {
	var gotAuth, gotCookie, gotX string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotCookie, gotX = r.Header.Get("Authorization"), r.Header.Get("Cookie"), r.Header.Get("X-Thing")
		switch r.URL.Path {
		case "/json":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"q":"` + r.URL.Query().Get("q") + `","n":[1,2]}`))
		case "/text":
			w.Write([]byte("hello"))
		case "/redir":
			http.Redirect(w, r, "/text", http.StatusFound)
		case "/big":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`"` + strings.Repeat("x", httpGetBodyCap+10) + `"`))
		case "/500":
			w.WriteHeader(500)
			w.Write([]byte("boom"))
		}
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	allow := []string{u.Hostname()}
	run := func(args map[string]any) (map[string]any, error) {
		out, err := httpGet(context.Background(), httpGetClient, allow, args)
		if err != nil {
			return nil, err
		}
		return out.(map[string]any), nil
	}

	// private/loopback hosts are fine when allowlisted
	m, err := run(map[string]any{"url": srv.URL + "/json", "query": map[string]any{"q": "a b"}, "headers": map[string]any{"X-Thing": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	body := m["body"].(map[string]any)
	if m["status"] != 200 || body["q"] != "a b" || len(body["n"].([]any)) != 2 || gotX != "1" || gotAuth != "" || gotCookie != "" {
		t.Fatalf("%v (x=%q)", m, gotX)
	}
	if m, _ = run(map[string]any{"url": srv.URL + "/text"}); m["body"] != "hello" {
		t.Fatalf("%v", m)
	}
	if m, _ = run(map[string]any{"url": srv.URL + "/500"}); m["status"] != 500 || m["body"] != "boom" {
		t.Fatalf("%v", m)
	}
	if m, _ = run(map[string]any{"url": srv.URL + "/redir"}); m["status"] != 302 || m["location"] != "/text" {
		t.Fatalf("redirect followed: %v", m)
	}
	if m, _ = run(map[string]any{"url": srv.URL + "/big"}); m["truncated"] != true || len(m["body"].(string)) != httpGetBodyCap {
		t.Fatalf("cap: %v", m["truncated"])
	}

	for _, h := range []string{"Authorization", "cookie", "Proxy-Authorization"} {
		if _, err := run(map[string]any{"url": srv.URL, "headers": map[string]any{h: "x"}}); !errors.Is(err, ErrInvalid) {
			t.Errorf("header %s allowed: %v", h, err)
		}
	}
	for _, bad := range []string{"", "ftp://" + u.Host, "http://other.example/x", "http://" + u.Hostname() + ".evil.com/"} {
		if _, err := run(map[string]any{"url": bad}); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestHostAllowed(t *testing.T) {
	p := func(s string) *url.URL { u, _ := url.Parse(s); return u }
	allow := []string{"api.internal", "svc.local:8080", "secure.local:443"}
	for u, want := range map[string]bool{
		"http://api.internal/x": true, "https://API.internal:9999/x": true, "http://svc.local:8080/": true,
		"http://svc.local/": false, "http://svc.local:9/": false, "https://secure.local/": true, "http://secure.local/": false,
		"http://evil.api.internal/": false, "http://api.internal.evil/": false,
	} {
		if got := hostAllowed(p(u), allow); got != want {
			t.Errorf("%s: got %v", u, got)
		}
	}
}

func TestHTTPGetOfferedOnlyWithAllowlist(t *testing.T) {
	var none, some Manager
	some.set.HTTPGetAllowlist = []string{"a"}
	if none.extraToolByName(httpGetName) != nil || some.extraToolByName(httpGetName) == nil {
		t.Fatal("offer predicate wrong")
	}
	if !httpGetTool.ann.ReadOnlyHint || !*httpGetTool.ann.OpenWorldHint {
		t.Fatal("annotations")
	}
	if _, err := httpGetTool.run(&some, context.Background(), nil, map[string]any{"url": "http://b/"}); err == nil {
		t.Fatal("non-allowlisted host allowed")
	}
}
