package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// clearEnv keeps cases independent: a variable one case sets is still set for
// the next unless the whole namespace is cleared first.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, entry := range os.Environ() {
		if key, _, _ := strings.Cut(entry, "="); strings.HasPrefix(key, Prefix) {
			t.Setenv(key, "")
			os.Unsetenv(key)
		}
	}
}

func TestLoadRequiresATunnelToken(t *testing.T) {
	clearEnv(t)
	_, err := Load("")
	if err == nil {
		t.Fatal("a hub with no tunnel token must refuse to start")
	}
	if !strings.Contains(err.Error(), "TUNNEL_TOKEN") {
		t.Errorf("the error should name the missing setting, got %q", err)
	}
}

func TestLoadDefaults(t *testing.T) {
	clearEnv(t)
	t.Setenv("MCP_SWITCHBOARD_TUNNEL_TOKEN", "t0ken")

	s, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if s.TunnelHost != "127.0.0.1" || s.PrivateHost != "127.0.0.1" {
		t.Error("both listeners must default to loopback")
	}
	if s.TunnelPort != DefaultTunnelPort || s.PrivatePort != DefaultPrivatePort {
		t.Errorf("ports = %d/%d, want %d/%d", s.TunnelPort, s.PrivatePort, DefaultTunnelPort, DefaultPrivatePort)
	}
	if s.PrivateToken != "" {
		t.Error("the private listener is unauthenticated unless a token is set")
	}
	if got := s.EffectiveLocalBaseURL(); got != "http://127.0.0.1:8099" {
		t.Errorf("local base URL = %q", got)
	}
	if s.LokiLabels["service"] != "mcp-switchboard" {
		t.Error("the service label must always be present")
	}
	if s.DBPath() != filepath.Join("/var/lib/mcp-switchboard", "calls.db") {
		t.Errorf("db path = %q", s.DBPath())
	}
}

func TestLoadReadsSecretsFromFiles(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("from-a-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MCP_SWITCHBOARD_TUNNEL_TOKEN", path)
	t.Setenv("MCP_SWITCHBOARD_DATA_DIR", dir)

	s, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if s.TunnelToken != "from-a-file" {
		t.Errorf("token = %q, want the file's contents", s.TunnelToken)
	}
	if s.DataDir != dir {
		t.Errorf("a directory must survive the substitution rule, got %q", s.DataDir)
	}
}

func TestLoadTrimsTrailingSlashes(t *testing.T) {
	clearEnv(t)
	t.Setenv("MCP_SWITCHBOARD_TUNNEL_TOKEN", "t")
	t.Setenv("MCP_SWITCHBOARD_PUBLIC_URL", "https://hub.example/")
	t.Setenv("MCP_SWITCHBOARD_LOKI_URL", "http://loki:3100/")

	s, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if s.PublicURL != "https://hub.example" || s.LokiURL != "http://loki:3100" {
		t.Errorf("URLs not trimmed: %q %q", s.PublicURL, s.LokiURL)
	}
}

func TestLokiLabelsMustBeKeyValue(t *testing.T) {
	clearEnv(t)
	t.Setenv("MCP_SWITCHBOARD_TUNNEL_TOKEN", "t")
	t.Setenv("MCP_SWITCHBOARD_LOKI_LABELS", "env=prod,broken")
	if _, err := Load(""); err == nil {
		t.Fatal("a malformed label list must be rejected, not silently dropped")
	}
}

func TestServerSettleDelayIsConfigurable(t *testing.T) {
	clearEnv(t)
	t.Setenv("MCP_SWITCHBOARD_TUNNEL_TOKEN", "t")

	s, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if s.ServerSettleDelay != 0.15 {
		t.Errorf("default settle delay = %v, want 0.15", s.ServerSettleDelay)
	}

	t.Setenv("MCP_SWITCHBOARD_SERVER_SETTLE_DELAY", "0.5")
	s, err = Load("")
	if err != nil {
		t.Fatal(err)
	}
	if s.ServerSettleDelay != 0.5 {
		t.Errorf("settle delay = %v, want 0.5", s.ServerSettleDelay)
	}
}

// The hub this replaces defaulted --env-file to ./.env, so running it in a
// checkout picked up the repo's settings without being told to. Every editor
// task, shell alias and README snippet assumes that.
func TestLoadReadsAnEnvFile(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	body := "MCP_SWITCHBOARD_TUNNEL_TOKEN=from-the-file\nMCP_SWITCHBOARD_PUBLIC_URL=https://hub.example\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.TunnelToken != "from-the-file" {
		t.Errorf("token = %q, want the file's value", s.TunnelToken)
	}
	if s.PublicURL != "https://hub.example" {
		t.Errorf("public url = %q", s.PublicURL)
	}
}

// Not every deployment has one - the container and the NixOS unit pass settings
// as real environment variables - so an absent default file is not an error.
func TestLoadIgnoresAnAbsentEnvFile(t *testing.T) {
	clearEnv(t)
	t.Setenv("MCP_SWITCHBOARD_TUNNEL_TOKEN", "from-the-environment")

	s, err := Load(filepath.Join(t.TempDir(), ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if s.TunnelToken != "from-the-environment" {
		t.Errorf("token = %q", s.TunnelToken)
	}
}

// A real environment variable beats the file, so `MCP_SWITCHBOARD_X=... hub`
// overrides a checkout's .env rather than being silently ignored.
func TestTheEnvironmentBeatsTheEnvFile(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("MCP_SWITCHBOARD_TUNNEL_TOKEN=from-the-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MCP_SWITCHBOARD_TUNNEL_TOKEN", "from-the-environment")

	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.TunnelToken != "from-the-environment" {
		t.Errorf("token = %q, want the environment to win", s.TunnelToken)
	}
}
