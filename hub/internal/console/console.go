// Package console serves the built single-page console out of the binary.
//
// It takes an fs.FS rather than reaching for the embedded files itself, so the
// serving rules below can be tested against a handful of fake files instead of
// against whatever Vite last produced.
package console

import (
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// IndexFile is the document every client-side route resolves to.
const IndexFile = "index.html"

// ManifestFile and ServiceWorkerFile are the PWA installability files
// (spec.md 8.6, U60/U61). Both live in web/public and are copied verbatim by
// the Vite build, so - unlike everything under assets/ - their names carry no
// content hash and must never be cached long-term.
const (
	ManifestFile      = "manifest.webmanifest"
	ServiceWorkerFile = "sw.js"
)

// Handler serves the console.
//
// Two caching rules, and they are opposites on purpose. Vite fingerprints every
// asset it emits, so those files are immutable and can be cached for a year;
// index.html names them, so it must never be cached or a browser will keep
// asking for the assets of a build that is no longer in the binary.
func Handler(assets fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		securityHeaders(w, r)
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name == "" {
			name = IndexFile
		}

		file, err := assets.Open(name)
		if err != nil {
			// Any path that is not a file is a client-side route: the router in
			// the browser owns it, so it gets index.html and works out what to
			// render. A deep link into the console must not 404 (spec.md 8).
			serveIndex(w, r, assets)
			return
		}
		defer file.Close()

		info, err := file.Stat()
		if err != nil || info.IsDir() {
			serveIndex(w, r, assets)
			return
		}

		if name == IndexFile || name == ManifestFile || name == ServiceWorkerFile {
			// None of these three are fingerprinted by the build (the manifest
			// and the service worker are hand-written files in public/, not
			// Vite output), so caching them for a year like the fingerprinted
			// assets below would leave an installed PWA running a stale service
			// worker or advertising a manifest that no longer matches the app.
			noStore(w)
		} else {
			// Fingerprinted by the build, so the content behind this URL can
			// never change.
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}

		// Set explicitly rather than letting http.ServeContent guess from the
		// system mime registry: that registry varies by machine (some don't
		// know ".webmanifest") and its sniffing fallback misreads content.
		w.Header().Set("Content-Type", contentType(name))

		seeker, ok := file.(io.ReadSeeker)
		if !ok {
			// Without a seeker there is no range support; send it whole rather
			// than refuse. embed.FS files always seek, so this is for the tests
			// and for any future backing store.
			_, _ = io.Copy(w, file)
			return
		}
		http.ServeContent(w, r, name, info.ModTime(), seeker)
	})
}

// securityHeaders sets the browser-side hardening headers on every console
// response. The console is a same-origin SPA with no third-party resources, so
// the policy is tight: everything from 'self', with three deliberate holes:
// style-src allows inline styles (React style={} attributes and the inline
// styles mermaid puts in its SVG), img-src allows data:/blob: (inline icons and
// rendered diagrams), and connect-src names ws:/wss: for this host explicitly
// because not every browser treats 'self' as covering WebSocket schemes.
// worker-src 'self' is for the service worker (sw.js); frame-ancestors and
// X-Frame-Options forbid embedding the console (clickjacking). Not verified in
// a browser here: KaTeX fonts (font-src 'self' data:) and mermaid rendering.
func securityHeaders(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	connect := "'self'"
	if host := r.Host; host != "" && !strings.ContainsAny(host, " ;,'\"") {
		connect += " ws://" + host + " wss://" + host
	}
	h.Set("Content-Security-Policy", strings.Join([]string{
		"default-src 'self'",
		"script-src 'self'",
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data: blob:",
		"font-src 'self' data:",
		"connect-src " + connect,
		"worker-src 'self'",
		"manifest-src 'self'",
		"object-src 'none'",
		"base-uri 'self'",
		"form-action 'self'",
		"frame-ancestors 'none'",
	}, "; "))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
}

func serveIndex(w http.ResponseWriter, r *http.Request, assets fs.FS) {
	index, err := assets.Open(IndexFile)
	if err != nil {
		// The console was not built into this binary. Say so plainly: the
		// alternative is a blank page and a confused operator.
		http.Error(w, "the console is not built into this binary", http.StatusNotFound)
		return
	}
	defer index.Close()

	info, err := index.Stat()
	if err != nil {
		http.Error(w, "the console is not readable", http.StatusInternalServerError)
		return
	}

	noStore(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	if seeker, ok := index.(io.ReadSeeker); ok {
		http.ServeContent(w, r, IndexFile, info.ModTime(), seeker)
		return
	}
	_, _ = io.Copy(w, index)
}

func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store, must-revalidate")
}

// contentType covers what a Vite build emits; anything else falls back to
// octet-stream, which is safer than guessing.
func contentType(name string) string {
	switch path.Ext(name) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".json":
		return "application/json"
	case ".woff2":
		return "font/woff2"
	case ".png":
		return "image/png"
	case ".webmanifest":
		return "application/manifest+json"
	default:
		return "application/octet-stream"
	}
}

// Built reports whether a console was compiled into this binary, so the caller
// can mount something else - or say something useful - when it was not.
func Built(assets fs.FS) bool {
	file, err := assets.Open(IndexFile)
	if err != nil {
		return false
	}
	_ = file.Close()
	return true
}
