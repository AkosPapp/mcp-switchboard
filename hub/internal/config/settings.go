package config

import (
	"path/filepath"
	"strconv"
	"strings"
)

// Default ports. The tunnel listener is the one meant to face the network, so
// it is the one that always carries a token; both still bind loopback until
// told otherwise, because a hub that is reachable by accident is worse than one
// that is unreachable by accident.
const (
	DefaultTunnelPort  = 8097
	DefaultPrivatePort = 8099
)

// Settings is the whole configuration surface, every field of which comes from
// an MCP_SWITCHBOARD_* variable and is therefore expressible in the NixOS
// module without new machinery (spec.md E4).
type Settings struct {
	// The public-facing listener: only /tunnel/v1 and /health live here, and a
	// token is always required.
	TunnelHost  string
	TunnelPort  int
	TunnelToken string

	// The private listener: console, /api, /mcp and /metrics. Unauthenticated
	// by default because it is meant to bind loopback and never be exposed;
	// once agents are enabled it also holds provider keys and every
	// conversation, which is what §10 is about.
	PrivateHost  string
	PrivatePort  int
	PrivateToken string

	DataDir  string
	LogLevel string

	CallTimeout  float64
	ToolsTimeout float64

	// How long a server must hold the running state before the hub opens a
	// session for it. See spec.md H4: this is what turns a burst of restarts
	// into one session rather than one per state frame.
	ServerSettleDelay float64

	RetentionDays int
	MaxRows       int

	LokiEnabled bool
	LokiURL     string
	LokiLabels  map[string]string

	// What the console prints in its endpoint list, so the URL it shows is one
	// that actually resolves from wherever the console is being read.
	LocalBaseURL string

	// The tunnel listener's externally reachable address. There is no way to
	// derive this from how the hub binds locally, so it is only ever what you
	// configure; it exists to build the copyable "connect a client" command.
	PublicURL string

	// Orchestrator settings are absent from this struct until the orchestrator
	// lands. AGENTS_ENABLED defaults to false and the hub must keep working
	// with no LLM configured at all (spec.md E6/E7).
}

// DBPath is where the call log lives.
func (s Settings) DBPath() string { return filepath.Join(s.DataDir, "calls.db") }

// EffectiveLocalBaseURL falls back to loopback plus the private port.
func (s Settings) EffectiveLocalBaseURL() string {
	if s.LocalBaseURL != "" {
		return s.LocalBaseURL
	}
	return "http://127.0.0.1:" + strconv.Itoa(s.PrivatePort)
}

// Load reads every setting, applying path indirection throughout. envFile is
// optional and never overrides a real environment variable.
func Load(envFile string) (Settings, error) {
	var s Settings

	if envFile != "" {
		if err := LoadEnvFile(envFile); err != nil {
			return s, err
		}
	}

	var err error
	fail := func(e error) (Settings, error) { return Settings{}, e }

	if s.TunnelToken, err = Get("TUNNEL_TOKEN", "", true); err != nil {
		return fail(err)
	}
	if s.TunnelToken == "" {
		return fail(errf(
			"%sTUNNEL_TOKEN is required - it authenticates the tunnel listener, "+
				"which is the one intended to be publicly reachable", Prefix,
		))
	}

	if s.TunnelHost, err = Get("TUNNEL_HOST", "127.0.0.1", false); err != nil {
		return fail(err)
	}
	if s.TunnelPort, err = GetInt("TUNNEL_PORT", DefaultTunnelPort); err != nil {
		return fail(err)
	}
	if s.PrivateHost, err = Get("PRIVATE_HOST", "127.0.0.1", false); err != nil {
		return fail(err)
	}
	if s.PrivatePort, err = GetInt("PRIVATE_PORT", DefaultPrivatePort); err != nil {
		return fail(err)
	}
	if s.PrivateToken, err = Get("PRIVATE_TOKEN", "", true); err != nil {
		return fail(err)
	}
	if s.DataDir, err = GetPath("DATA_DIR", "/var/lib/mcp-switchboard"); err != nil {
		return fail(err)
	}

	logLevel, err := Get("LOG_LEVEL", "INFO", false)
	if err != nil {
		return fail(err)
	}
	s.LogLevel = strings.ToUpper(logLevel)

	if s.CallTimeout, err = GetFloat("CALL_TIMEOUT", 120.0); err != nil {
		return fail(err)
	}
	if s.ToolsTimeout, err = GetFloat("TOOLS_TIMEOUT", 30.0); err != nil {
		return fail(err)
	}
	if s.ServerSettleDelay, err = GetFloat("SERVER_SETTLE_DELAY", 0.15); err != nil {
		return fail(err)
	}
	if s.RetentionDays, err = GetInt("RETENTION_DAYS", 30); err != nil {
		return fail(err)
	}
	if s.MaxRows, err = GetInt("MAX_ROWS", 100_000); err != nil {
		return fail(err)
	}
	if s.LokiEnabled, err = GetBool("LOKI_ENABLED", false); err != nil {
		return fail(err)
	}

	lokiURL, err := Get("LOKI_URL", "http://127.0.0.1:3100", false)
	if err != nil {
		return fail(err)
	}
	s.LokiURL = strings.TrimRight(lokiURL, "/")

	rawLabels, err := Get("LOKI_LABELS", "", false)
	if err != nil {
		return fail(err)
	}
	s.LokiLabels, err = parseLabels(rawLabels)
	if err != nil {
		return fail(err)
	}

	localBase, err := Get("LOCAL_BASE_URL", "", false)
	if err != nil {
		return fail(err)
	}
	s.LocalBaseURL = strings.TrimRight(localBase, "/")

	publicURL, err := Get("PUBLIC_URL", "", false)
	if err != nil {
		return fail(err)
	}
	s.PublicURL = strings.TrimRight(publicURL, "/")

	return s, nil
}

// parseLabels turns `k=v,k2=v2` into the Loki stream labels, always including
// the service label so a hub's lines are findable without configuration.
func parseLabels(raw string) (map[string]string, error) {
	labels := map[string]string{"service": "mcp-switchboard"}
	if raw == "" {
		return labels, nil
	}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, value, found := strings.Cut(part, "=")
		if !found {
			return nil, errf("LOKI_LABELS entry %q must be key=value", part)
		}
		labels[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return labels, nil
}
