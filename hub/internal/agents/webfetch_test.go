package agents

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

const testPage = `<!doctype html><html><head><title>My &amp; Page</title><style>body{color:red}</style><script>var x="SECRET_JS";</script></head>
<body><nav><a href="/menu">MENU_NAV</a></nav>
<h1>Hello</h1><p>First <b>para</b> with a <a href="/docs/x">link</a>.</p>
<ul><li>one</li><li>two</li></ul><ol><li>a</li><li>b</li></ol>
<pre>line1
  line2</pre><p>Use <code>go test</code> now.</p>
<footer>FOOTER_TEXT</footer></body></html>`

func TestHTMLToText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(testPage))
	}))
	defer srv.Close()
	out, err := webFetch(context.Background(), newFetchClient(false), map[string]any{"url": srv.URL + "/p"})
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	text := m["text"].(string)
	if m["title"] != "My & Page" || m["untrusted"] != true || m["truncated"] != false || m["status"] != 200 {
		t.Fatalf("%v", m)
	}
	if !strings.HasPrefix(text, webFetchNote+"\n") {
		t.Fatalf("no untrusted note: %q", text)
	}
	for _, want := range []string{"# Hello", "[link](" + srv.URL + "/docs/x)", "- one", "- two", "1. a", "2. b", "```\nline1\n  line2\n```", "`go test`", "First para with a"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in\n%s", want, text)
		}
	}
	for _, bad := range []string{"SECRET_JS", "MENU_NAV", "FOOTER_TEXT", "color:red", "<p>"} {
		if strings.Contains(text, bad) {
			t.Errorf("unexpected %q in\n%s", bad, text)
		}
	}
}

func TestWebFetchContentTypes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/j":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"a":1,"b":[2]}`))
		case "/t":
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte("plain text"))
		case "/b":
			w.Header().Set("Content-Type", "image/png")
			w.Write([]byte{0x89, 'P', 'N', 'G', 0, 1, 2})
		case "/big":
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte(strings.Repeat("x", webFetchBodyCap+500)))
		}
	}))
	defer srv.Close()
	c := newFetchClient(false)
	get := func(path string, extra map[string]any) map[string]any {
		t.Helper()
		args := map[string]any{"url": srv.URL + path}
		for k, v := range extra {
			args[k] = v
		}
		out, err := webFetch(context.Background(), c, args)
		if err != nil {
			t.Fatal(err)
		}
		return out.(map[string]any)
	}
	if txt := get("/j", nil)["text"].(string); !strings.Contains(txt, "{\n  \"a\": 1,") {
		t.Errorf("json not pretty: %q", txt)
	}
	if txt := get("/t", nil)["text"].(string); !strings.HasSuffix(txt, "\nplain text") {
		t.Errorf("%q", txt)
	}
	if txt := get("/b", nil)["text"].(string); !strings.Contains(txt, "not shown") {
		t.Errorf("%q", txt)
	}
	// paging
	p1 := get("/t", map[string]any{"max_chars": 5.0})
	if p1["truncated"] != true || p1["next_offset"] != 5 || !strings.HasSuffix(p1["text"].(string), "\nplain") {
		t.Fatalf("%v", p1)
	}
	p2 := get("/t", map[string]any{"max_chars": 5.0, "offset": 5.0})
	if p2["truncated"] != false || !strings.HasSuffix(p2["text"].(string), "\n text") {
		t.Fatalf("%v", p2)
	}
	big := get("/big", map[string]any{"max_chars": 1e9})
	if big["body_truncated"] != true || len([]rune(big["text"].(string))) > webFetchMaxChars+200 {
		t.Errorf("caps not applied: %v", big["truncated"])
	}
}

func TestWebFetchSSRFGuard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("internal")) }))
	defer srv.Close()
	// guarded: loopback is refused
	if _, err := webFetch(context.Background(), newFetchClient(true), map[string]any{"url": srv.URL}); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("loopback fetched: %v", err)
	}
	// a public-looking URL redirecting to loopback is refused at dial time
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL, http.StatusFound)
	}))
	defer redir.Close()
	if _, err := webFetch(context.Background(), newFetchClient(true), map[string]any{"url": redir.URL}); err == nil {
		t.Fatal("redirect to loopback allowed")
	}
	for _, bad := range []string{"", "ftp://x/", "file:///etc/passwd", "javascript:1", "/relative"} {
		if _, err := webFetch(context.Background(), newFetchClient(true), map[string]any{"url": bad}); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	// the tool itself uses the guarded client
	if _, err := webFetchTool.run(nil, context.Background(), nil, map[string]any{"url": srv.URL}); err == nil {
		t.Fatal("tool fetched loopback")
	}
}

func TestBlockedAddr(t *testing.T) {
	for ip, want := range map[string]bool{
		"127.0.0.1": true, "::1": true, "10.1.2.3": true, "192.168.0.1": true, "172.16.5.5": true, "169.254.169.254": true,
		"0.0.0.0": true, "::": true, "224.0.0.1": true, "ff02::1": true, "100.64.0.1": true, "100.127.255.255": true,
		"fe80::1": true, "fc00::1": true, "::ffff:127.0.0.1": true, "::ffff:10.0.0.1": true,
		"8.8.8.8": false, "1.1.1.1": false, "100.128.0.1": false, "2606:4700::1111": false,
	} {
		if got := blockedAddr(netip.MustParseAddr(ip)); got != want {
			t.Errorf("%s: got %v want %v", ip, got, want)
		}
	}
}

func TestWebFetchRedirectLimit(t *testing.T) {
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		http.Redirect(w, r, "/next", http.StatusFound)
	}))
	defer srv.Close()
	if _, err := webFetch(context.Background(), newFetchClient(false), map[string]any{"url": srv.URL}); err == nil {
		t.Fatal("endless redirect accepted")
	}
	if n > webFetchRedirects+1 {
		t.Fatalf("followed %d requests", n)
	}
}

func TestWebFetchEnabledPredicate(t *testing.T) {
	var on, off Manager
	off.set.WebFetchDisabled = true
	if on.extraToolByName(webFetchName) == nil {
		t.Error("web.fetch should be on by default")
	}
	if off.extraToolByName(webFetchName) != nil {
		t.Error("WEB_FETCH=false ignored")
	}
	if !webFetchTool.ann.ReadOnlyHint || !*webFetchTool.ann.OpenWorldHint {
		t.Error("annotations")
	}
}
