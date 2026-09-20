package main

import (
	"log/slog"
	"testing"
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
