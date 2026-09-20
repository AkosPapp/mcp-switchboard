package api

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// RequireToken is optional bearer auth for the private listener, for when it is
// exposed anyway (spec.md H1). It is middleware rather than something baked
// into the API handler so the same wrapper can cover /mcp and /metrics in the
// hub's wiring: those are as sensitive as /api, and a token that protected only
// the REST surface would be a false comfort.
//
// An empty token returns the handler untouched, which is the documented default:
// the private listener is unauthenticated unless MCP_SWITCHBOARD_PRIVATE_TOKEN
// is set.
func RequireToken(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /health stays open: it is what a supervisor, a container probe or a
		// reverse proxy polls, and none of those carry the token.
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}
		if !tokenMatches(token, r.Header.Get("Authorization")) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// tokenMatches compares in constant time, so a caller cannot learn the token
// one byte at a time from how long the comparison took.
func tokenMatches(token, header string) bool {
	var supplied string
	if len(header) >= len("Bearer ") && strings.EqualFold(header[:len("Bearer ")], "Bearer ") {
		supplied = strings.TrimSpace(header[len("Bearer "):])
	}
	return subtle.ConstantTimeCompare([]byte(supplied), []byte(token)) == 1
}
