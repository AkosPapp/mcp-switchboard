package console

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

func testAssets() fstest.MapFS {
	return fstest.MapFS{
		"index.html":              {Data: []byte("<!doctype html><div id=root>")},
		"assets/index-a1b2c3.js":  {Data: []byte("console.log(1)")},
		"assets/index-d4e5f6.css": {Data: []byte("body{}")},
		"favicon.svg":             {Data: []byte("<svg/>")},
		"manifest.webmanifest":    {Data: []byte(`{"name":"mcp-switchboard"}`)},
		"sw.js":                   {Data: []byte("self.addEventListener('install', () => {});")},
		"icons/icon-192.png":      {Data: []byte("png-bytes-192")},
	}
}

func get(t *testing.T, handler http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func TestServesTheIndexAtRoot(t *testing.T) {
	rec := get(t, Handler(testAssets()), "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Body.String(); got != "<!doctype html><div id=root>" {
		t.Errorf("body = %q", got)
	}
	if got := rec.Header().Get("Content-Type"); got == "" {
		t.Error("the index needs a content type")
	}
}

// index.html names the fingerprinted assets, so caching it would leave a
// browser asking for files that are no longer in the binary.
func TestTheIndexIsNeverCached(t *testing.T) {
	for _, target := range []string{"/", "/index.html", "/calls"} {
		rec := get(t, Handler(testAssets()), target)
		if got := rec.Header().Get("Cache-Control"); got != "no-store, must-revalidate" {
			t.Errorf("%s: Cache-Control = %q", target, got)
		}
	}
}

func TestFingerprintedAssetsAreCachedForever(t *testing.T) {
	rec := get(t, Handler(testAssets()), "/assets/index-a1b2c3.js")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Errorf("Cache-Control = %q", got)
	}
	if rec.Body.String() != "console.log(1)" {
		t.Errorf("body = %q", rec.Body.String())
	}
}

// The manifest and service worker are hand-written files with no content hash
// in their name, unlike the fingerprinted build output, so they must be
// revalidated on every request like index.html (spec.md U60/U61).
func TestTheManifestAndServiceWorkerAreNeverCached(t *testing.T) {
	handler := Handler(testAssets())
	for _, target := range []string{"/manifest.webmanifest", "/sw.js"} {
		rec := get(t, handler, target)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d", target, rec.Code)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store, must-revalidate" {
			t.Errorf("%s: Cache-Control = %q", target, got)
		}
	}
}

func TestTheManifestHasTheRightContentType(t *testing.T) {
	rec := get(t, Handler(testAssets()), "/manifest.webmanifest")
	if got := rec.Header().Get("Content-Type"); got != "application/manifest+json" {
		t.Errorf("Content-Type = %q", got)
	}
}

// Client-side routing: a deep link must render the console, not a 404.
func TestUnknownPathsFallBackToTheIndex(t *testing.T) {
	handler := Handler(testAssets())
	for _, target := range []string{"/calls", "/connections", "/endpoints", "/deep/link"} {
		rec := get(t, handler, target)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want the index", target, rec.Code)
		}
		if rec.Body.String() != "<!doctype html><div id=root>" {
			t.Errorf("%s: did not serve the index", target)
		}
	}
}

// A path that escapes the asset root must not reach the filesystem.
func TestTraversalIsRefused(t *testing.T) {
	rec := get(t, Handler(testAssets()), "/../../etc/passwd")
	if rec.Code != http.StatusOK || rec.Body.String() != "<!doctype html><div id=root>" {
		t.Errorf("traversal was not folded into the index: status=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestWithoutABuiltConsole(t *testing.T) {
	empty := fstest.MapFS{}
	if Built(empty) {
		t.Error("an empty FS is not a built console")
	}
	rec := get(t, Handler(empty), "/")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if got := rec.Body.String(); got == "" {
		t.Error("the response should explain that the console was not built in")
	}
}

func TestBuiltReportsAConsole(t *testing.T) {
	if !Built(testAssets()) {
		t.Error("an FS with an index is a built console")
	}
}

func TestOnlyReads(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler(testAssets()).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}
