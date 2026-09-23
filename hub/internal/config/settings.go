package config

import (
	"encoding/json"
	"fmt"
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

	// Orchestrator settings (spec.md E6). AgentsEnabled defaults to false and
	// the hub must keep working with no LLM configured at all (E7).
	AgentsEnabled bool

	// LLMModelsPath is the models JSON file (L4); empty means none declared.
	LLMModelsPath string

	// Provider credentials (L3, X7). Keys are never logged or returned; see
	// Settings.String. Empty means the provider is not configured.
	LLMAnthropicAPIKey         string
	LLMAnthropicBaseURL        string
	LLMOpenAIAPIKey            string
	LLMOpenAIBaseURL           string
	LLMOpenAICompatibleAPIKey  string
	LLMOpenAICompatibleBaseURL string

	// LLMOpenAICompatibleKind: auto (default) | ollama | generic (L7).
	LLMOpenAICompatibleKind string
	// LLMOpenAICompatibleDiscover: list models from the provider (L8).
	LLMOpenAICompatibleDiscover bool
	// LLMOllamaNumCtx: context window sent to Ollama, 0 = auto (L7).
	LLMOllamaNumCtx int

	AgentMaxDepth             int
	AgentMaxChildren          int
	AgentMaxConcurrentRuns    int
	AgentMaxParallelToolCalls int

	AgentDefaultBudget AgentBudget
	AgentDefaultGrants []GrantSpec

	// Seconds.
	AgentReplyTimeout int
	ApprovalTimeout   int

	InboxRetentionDays int
	// ShutdownGrace is in seconds.
	ShutdownGrace int

	// Web Push (spec.md 8.7). Both must be set for push to be available; when
	// either is empty, push is simply disabled - the /api/push/* routes 404
	// and nothing ever tries to send a notification. There is no separate
	// "enabled" flag, because a pair of keys already says everything a flag
	// would.
	PushVAPIDPublicKey  string
	PushVAPIDPrivateKey string
}

// AgentBudget is the default per-agent budget (spec.md B-series, E6).
type AgentBudget struct {
	MaxTurns              int   `json:"max_turns"`
	MaxToolCalls          int   `json:"max_tool_calls"`
	MaxTokens             int64 `json:"max_tokens"`
	MaxCostMicros         int64 `json:"max_cost_micros"`
	MaxWallSeconds        int   `json:"max_wall_seconds"`
	MaxLifetimeCostMicros int64 `json:"max_lifetime_cost_micros"`
}

// DefaultAgentBudget is what AGENT_DEFAULT_BUDGET overrides field by field.
func DefaultAgentBudget() AgentBudget {
	return AgentBudget{
		MaxTurns:              32,
		MaxToolCalls:          200,
		MaxTokens:             1_000_000,
		MaxCostMicros:         5_000_000,
		MaxWallSeconds:        1800,
		MaxLifetimeCostMicros: 50_000_000,
	}
}

// GrantSpec is one default grant: label/project/server patterns.
type GrantSpec struct {
	Label   string `json:"label"`
	Project string `json:"project"`
	Server  string `json:"server"`
}

// String redacts every credential so a printed Settings is safe to log.
func (s Settings) String() string {
	mask := func(v string) string {
		if v == "" {
			return "<unset>"
		}
		return "<redacted>"
	}
	return fmt.Sprintf(
		"Settings{tunnel=%s:%d private=%s:%d data=%s tunnelToken=%s privateToken=%s "+
			"agents=%v anthropicKey=%s openaiKey=%s openaiCompatibleKey=%s}",
		s.TunnelHost, s.TunnelPort, s.PrivateHost, s.PrivatePort, s.DataDir,
		mask(s.TunnelToken), mask(s.PrivateToken), s.AgentsEnabled,
		mask(s.LLMAnthropicAPIKey), mask(s.LLMOpenAIAPIKey), mask(s.LLMOpenAICompatibleAPIKey),
	)
}

// GoString keeps %#v from bypassing the redaction.
func (s Settings) GoString() string { return s.String() }

// DBPath is where the call log lives.
func (s Settings) DBPath() string { return filepath.Join(s.DataDir, "calls.db") }

// PushEnabled reports whether both VAPID keys are configured.
func (s Settings) PushEnabled() bool {
	return s.PushVAPIDPublicKey != "" && s.PushVAPIDPrivateKey != ""
}

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

	if err := loadAgentSettings(&s); err != nil {
		return fail(err)
	}

	// Neither is a secret in the Resolve sense that a missing file should be
	// fatal (both are false here), but PUSH_VAPID_PRIVATE_KEY is treated as
	// path-indirectable the same as every other value: `Get` already handles
	// that uniformly. There is no separate "PUSH_ENABLED" - PushEnabled()
	// derives it from whether both keys are present.
	if s.PushVAPIDPublicKey, err = Get("PUSH_VAPID_PUBLIC_KEY", "", false); err != nil {
		return fail(err)
	}
	if s.PushVAPIDPrivateKey, err = Get("PUSH_VAPID_PRIVATE_KEY", "", true); err != nil {
		return fail(err)
	}

	return s, nil
}

func loadAgentSettings(s *Settings) error {
	var err error
	if s.AgentsEnabled, err = GetBool("AGENTS_ENABLED", false); err != nil {
		return err
	}
	if s.LLMModelsPath, err = Get("LLM_MODELS", "", false); err != nil {
		return err
	}
	for _, p := range []struct {
		name         string
		key, baseURL *string
	}{
		{"ANTHROPIC", &s.LLMAnthropicAPIKey, &s.LLMAnthropicBaseURL},
		{"OPENAI", &s.LLMOpenAIAPIKey, &s.LLMOpenAIBaseURL},
		{"OPENAI_COMPATIBLE", &s.LLMOpenAICompatibleAPIKey, &s.LLMOpenAICompatibleBaseURL},
	} {
		if *p.key, err = Get("LLM_"+p.name+"_API_KEY", "", true); err != nil {
			return err
		}
		base, err := Get("LLM_"+p.name+"_BASE_URL", "", false)
		if err != nil {
			return err
		}
		*p.baseURL = strings.TrimRight(base, "/")
	}
	kind, err := Get("LLM_OPENAI_COMPATIBLE_KIND", "auto", false)
	if err != nil {
		return err
	}
	kind = strings.ToLower(strings.TrimSpace(kind))
	switch kind {
	case "auto", "ollama", "generic":
		s.LLMOpenAICompatibleKind = kind
	default:
		return errf("%sLLM_OPENAI_COMPATIBLE_KIND must be auto, ollama or generic, got %q", Prefix, kind)
	}
	if s.LLMOpenAICompatibleDiscover, err = GetBool("LLM_OPENAI_COMPATIBLE_DISCOVER", true); err != nil {
		return err
	}
	ints := []struct {
		name string
		dst  *int
		def  int
	}{
		{"LLM_OLLAMA_NUM_CTX", &s.LLMOllamaNumCtx, 0},
		{"AGENT_MAX_DEPTH", &s.AgentMaxDepth, 4},
		{"AGENT_MAX_CHILDREN", &s.AgentMaxChildren, 8},
		{"AGENT_MAX_CONCURRENT_RUNS", &s.AgentMaxConcurrentRuns, 16},
		{"AGENT_MAX_PARALLEL_TOOL_CALLS", &s.AgentMaxParallelToolCalls, 8},
		{"AGENT_REPLY_TIMEOUT", &s.AgentReplyTimeout, 300},
		{"APPROVAL_TIMEOUT", &s.ApprovalTimeout, 3600},
		{"INBOX_RETENTION_DAYS", &s.InboxRetentionDays, 7},
		{"SHUTDOWN_GRACE", &s.ShutdownGrace, 10},
	}
	for _, f := range ints {
		if *f.dst, err = GetInt(f.name, f.def); err != nil {
			return err
		}
	}

	s.AgentDefaultBudget = DefaultAgentBudget()
	raw, err := Get("AGENT_DEFAULT_BUDGET", "", false)
	if err != nil {
		return err
	}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &s.AgentDefaultBudget); err != nil {
			return errf("%sAGENT_DEFAULT_BUDGET must be a JSON object: %v", Prefix, err)
		}
	}

	s.AgentDefaultGrants = []GrantSpec{{Label: "*", Project: "*", Server: "*"}}
	raw, err = Get("AGENT_DEFAULT_GRANTS", "", false)
	if err != nil {
		return err
	}
	if raw != "" {
		var grants []GrantSpec
		if err := json.Unmarshal([]byte(raw), &grants); err != nil {
			return errf("%sAGENT_DEFAULT_GRANTS must be a JSON list of {label,project,server}: %v", Prefix, err)
		}
		for i := range grants {
			// An omitted field is left unrestricted only by saying "*".
			if grants[i].Label == "" || grants[i].Project == "" || grants[i].Server == "" {
				return errf("%sAGENT_DEFAULT_GRANTS entry %d must set label, project and server", Prefix, i)
			}
		}
		s.AgentDefaultGrants = grants
	}
	return nil
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
