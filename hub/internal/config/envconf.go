// Package config loads hub settings from the environment and an optional .env
// file, applying the one rule that makes every secret injectable without a
// second mechanism: a value that starts with "/" and names an existing regular
// file is replaced by that file's contents.
//
// The Python client keeps its own envconf.py; this is the Go reimplementation
// of the same two rules, and both run docs/envconf-cases.json so they cannot
// disagree about the edges (spec.md P3/V4).
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Prefix namespaces every setting this project reads.
const Prefix = "MCP_SWITCHBOARD_"

// Error is a malformed or unusable setting.
type Error struct{ msg string }

func (e *Error) Error() string { return e.msg }

func errf(format string, args ...any) error { return &Error{msg: fmt.Sprintf(format, args...)} }

// LoadEnvFile populates the environment from a KEY=VALUE file. Real environment
// variables always win, so a .env is a default, never an override.
//
// Blank lines, comments and lines with no "=" are ignored; there is no
// interpolation and no "export" keyword. Deliberately minimal, matching the
// .env files people hand-write for this tool.
func LoadEnvFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return errf("cannot read env file %s: %v", path, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		key, value, _ := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `'"`)
		if key == "" {
			continue
		}
		if _, set := os.LookupEnv(key); !set {
			if err := os.Setenv(key, value); err != nil {
				return errf("cannot set %s: %v", key, err)
			}
		}
	}
	return nil
}

// Resolve applies the path indirection to one value.
//
// secret changes only what happens when the path does not exist: for a secret
// that is an error, because a token that happens to look like an absolute path
// is far less likely than a secret that failed to materialise, and
// authenticating with the literal string "/run/secrets/..." would be worse than
// failing to start.
func Resolve(value string, secret bool, name string) (string, error) {
	if !strings.HasPrefix(value, "/") {
		return value, nil
	}

	info, err := os.Stat(value)
	switch {
	case err == nil && info.Mode().IsRegular():
		data, readErr := os.ReadFile(value)
		if readErr != nil {
			return "", errf("cannot read %s from %s: %v", nameOr(name, "value"), value, readErr)
		}
		return strings.TrimSpace(string(data)), nil
	case secret:
		// Only regular files substitute, so a directory reaches here too - and
		// a secret pointing at a directory is just as broken as one pointing at
		// nothing.
		return "", errf(
			"%s looks like a file path (%s) but no readable file is there",
			nameOr(name, "setting"), value,
		)
	default:
		// A genuine path value such as DATA_DIR=/var/lib/mcp-switchboard.
		return value, nil
	}
}

func nameOr(name, fallback string) string {
	if name == "" {
		return fallback
	}
	return name
}

// Get reads Prefix+name with path indirection applied. An unset or empty value
// yields the default: empty is treated as unset so that clearing a variable in
// a unit file behaves the same as removing it.
func Get(name, def string, secret bool) (string, error) {
	key := Prefix + name
	raw := os.Getenv(key)
	if raw == "" {
		return def, nil
	}
	return Resolve(raw, secret, key)
}

// GetInt reads an integer setting, after path indirection.
func GetInt(name string, def int) (int, error) {
	raw, err := Get(name, "", false)
	if err != nil || raw == "" {
		return def, err
	}
	value, convErr := strconv.Atoi(raw)
	if convErr != nil {
		return 0, errf("%s%s must be an integer, got %q", Prefix, name, raw)
	}
	return value, nil
}

// GetFloat reads a float setting, after path indirection.
func GetFloat(name string, def float64) (float64, error) {
	raw, err := Get(name, "", false)
	if err != nil || raw == "" {
		return def, err
	}
	value, convErr := strconv.ParseFloat(raw, 64)
	if convErr != nil {
		return 0, errf("%s%s must be a number, got %q", Prefix, name, raw)
	}
	return value, nil
}

// GetBool reads a boolean setting, after path indirection. The accepted
// spellings match the Python side exactly; anything else is an error rather
// than a silent false, because a misspelled flag that quietly disables a
// feature is the kind of thing found weeks later.
func GetBool(name string, def bool) (bool, error) {
	raw, err := Get(name, "", false)
	if err != nil || raw == "" {
		return def, err
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	default:
		return false, errf("%s%s must be a boolean, got %q", Prefix, name, raw)
	}
}

// GetPath is Get for a filesystem path, cleaned but never substituted: a
// directory never matches the indirection rule in the first place.
func GetPath(name, def string) (string, error) {
	raw, err := Get(name, def, false)
	if err != nil {
		return "", err
	}
	return filepath.Clean(raw), nil
}
