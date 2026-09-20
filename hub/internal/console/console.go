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

// Handler serves the console.
//
// Two caching rules, and they are opposites on purpose. Vite fingerprints every
// asset it emits, so those files are immutable and can be cached for a year;
// index.html names them, so it must never be cached or a browser will keep
// asking for the assets of a build that is no longer in the binary.
func Handler(assets fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

		if name == IndexFile {
			noStore(w)
		} else {
			// Fingerprinted by the build, so the content behind this URL can
			// never change.
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}

		seeker, ok := file.(io.ReadSeeker)
		if !ok {
			// Without a seeker there is no range support; send it whole rather
			// than refuse. embed.FS files always seek, so this is for the tests
			// and for any future backing store.
			w.Header().Set("Content-Type", contentType(name))
			_, _ = io.Copy(w, file)
			return
		}
		http.ServeContent(w, r, name, info.ModTime(), seeker)
	})
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
