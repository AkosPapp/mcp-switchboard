package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/config"
)

// The NixOS module still offers Python's level names, and there are .env files
// in the wild written against the hub this replaces. Falling back to INFO on
// those would quietly discard the operator's intent.
func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"DEBUG":    slog.LevelDebug,
		"debug":    slog.LevelDebug,
		"INFO":     slog.LevelInfo,
		"WARN":     slog.LevelWarn,
		"WARNING":  slog.LevelWarn,
		"ERROR":    slog.LevelError,
		"CRITICAL": slog.LevelError,
		"FATAL":    slog.LevelError,
		" warn ":   slog.LevelWarn,
		"nonsense": slog.LevelInfo,
		"":         slog.LevelInfo,
	}
	for input, want := range cases {
		if got := parseLevel(input); got != want {
			t.Errorf("parseLevel(%q) = %v, want %v", input, got, want)
		}
	}
}

func TestTokenGuard(t *testing.T) {
	inside := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	guarded := tokenGuard("s3cret", inside)
	cases := []struct {
		auth string
		want int
	}{
		{"", http.StatusUnauthorized},
		{"s3cret", http.StatusUnauthorized},        // scheme required
		{"Bearer wrong", http.StatusUnauthorized},  // wrong secret
		{"bearer s3cret", http.StatusUnauthorized}, // exact match incl. scheme spelling
		{"Bearer s3cret", http.StatusTeapot},
		{"Bearer s3cret ", http.StatusUnauthorized},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, "/api/chats", nil)
		if c.auth != "" {
			r.Header.Set("Authorization", c.auth)
		}
		rec := httptest.NewRecorder()
		guarded.ServeHTTP(rec, r)
		if rec.Code != c.want {
			t.Errorf("Authorization %q: got %d, want %d", c.auth, rec.Code, c.want)
		}
	}
}

func TestTunnelMuxPublicAPI(t *testing.T) {
	apiStub := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	off := tunnelMux(config.Settings{TunnelToken: "t"}, http.NotFoundHandler(), apiStub)
	on := tunnelMux(config.Settings{TunnelToken: "t", PublicAPI: true, PublicAPIToken: "t"}, http.NotFoundHandler(), apiStub)
	for _, tt := range []struct {
		name string
		mux  http.Handler
		want int
	}{{"off", off, http.StatusNotFound}, {"on", on, http.StatusTeapot}} {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/api/anything", nil)
		r.Header.Set("Authorization", "Bearer t")
		tt.mux.ServeHTTP(rec, r)
		if rec.Code != tt.want {
			t.Errorf("public api %s: /api/anything got %d, want %d", tt.name, rec.Code, tt.want)
		}
	}
	rec := httptest.NewRecorder()
	on.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/anything", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("public api on without token: got %d, want 401", rec.Code)
	}
	rec = httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/anything", nil)
	r.Header.Set("Authorization", "Bearer t")
	on.ServeHTTP(rec, r)
	if rec.Code != http.StatusTeapot {
		t.Errorf("public api on with tunnel token: got %d, want 218 (falls back to TUNNEL_TOKEN)", rec.Code)
	}
}
