package llm

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/config"
)

// Price is micro-currency per million tokens. Integers only (L5).
type Price struct {
	InputMicrosPerMTok      int64 `json:"input_micros_per_mtok"`
	OutputMicrosPerMTok     int64 `json:"output_micros_per_mtok"`
	CacheReadMicrosPerMTok  int64 `json:"cache_read_micros_per_mtok"`
	CacheWriteMicrosPerMTok int64 `json:"cache_write_micros_per_mtok"`
}

// UnmarshalJSON also accepts the short spellings cache_read / cache_write used
// in spec.md L5.
func (p *Price) UnmarshalJSON(b []byte) error {
	type plain Price
	var raw struct {
		plain
		CacheRead  *int64 `json:"cache_read"`
		CacheWrite *int64 `json:"cache_write"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*p = Price(raw.plain)
	if raw.CacheRead != nil {
		p.CacheReadMicrosPerMTok = *raw.CacheRead
	}
	if raw.CacheWrite != nil {
		p.CacheWriteMicrosPerMTok = *raw.CacheWrite
	}
	return nil
}

// Cost returns the cost of u in micros, rounded up to the next whole micro so a
// nonzero usage is never billed as free. Integer math throughout.
func (p Price) Cost(u Usage) int64 {
	total := int64(u.InputTokens)*p.InputMicrosPerMTok +
		int64(u.OutputTokens)*p.OutputMicrosPerMTok +
		int64(u.CacheReadTokens)*p.CacheReadMicrosPerMTok +
		int64(u.CacheWriteTokens)*p.CacheWriteMicrosPerMTok
	if total <= 0 {
		return 0
	}
	return (total + 999_999) / 1_000_000
}

// ModelSpec declares one usable model. Priced is true when the models file had
// a "prices" entry (a genuinely free local model can say all zeros).
type ModelSpec struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Prices   Price  `json:"prices"`
	Priced   bool   `json:"-"`

	// ContextWindow: in the models file, the per-model num_ctx override
	// (context_window); on discovered models, the model's own maximum.
	ContextWindow int   `json:"contextWindow,omitempty"`
	SupportsTools *bool `json:"supportsTools,omitempty"`
	Discovered    bool  `json:"discovered,omitempty"`
}

// UnmarshalJSON records whether a price entry was present.
func (m *ModelSpec) UnmarshalJSON(b []byte) error {
	var raw struct {
		Provider string           `json:"provider"`
		Model    string           `json:"model"`
		Prices   *json.RawMessage `json:"prices"`

		ContextWindow  int   `json:"contextWindow"`
		ContextWindow2 int   `json:"context_window"`
		SupportsTools  *bool `json:"supportsTools"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	m.Provider, m.Model = raw.Provider, raw.Model
	m.ContextWindow, m.SupportsTools = raw.ContextWindow, raw.SupportsTools
	if raw.ContextWindow2 > 0 {
		m.ContextWindow = raw.ContextWindow2
	}
	m.Prices, m.Priced = Price{}, false
	if raw.Prices != nil && string(*raw.Prices) != "null" {
		if err := json.Unmarshal(*raw.Prices, &m.Prices); err != nil {
			return err
		}
		m.Priced = true
	}
	return nil
}

// Cost prices u; the bool is false when the model has no price (cost 0,
// flagged cost_unknown).
func (m ModelSpec) Cost(u Usage) (micros int64, known bool) {
	if !m.Priced {
		return 0, false
	}
	return m.Prices.Cost(u), true
}

// Registry holds configured providers and declared models. Safe for
// concurrent use.
type Registry struct {
	mu        sync.RWMutex
	providers map[string]Provider
	models    []ModelSpec // declared

	discoverOff bool // MCP_SWITCHBOARD_LLM_OPENAI_COMPATIBLE_DISCOVER=false
	disc        discoveryState
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{providers: map[string]Provider{}} }

// NewRegistryFromSettings builds the real providers whose credentials are
// present and loads the models file (if any). anthropic and openai need a key;
// openai-compatible needs a base URL (its key is optional, e.g. Ollama).
func NewRegistryFromSettings(s config.Settings) (*Registry, error) {
	r := NewRegistry()
	if s.LLMAnthropicAPIKey != "" {
		r.Register(NewAnthropic(s.LLMAnthropicAPIKey, s.LLMAnthropicBaseURL))
	}
	if s.LLMOpenAIAPIKey != "" {
		r.Register(NewOpenAI(s.LLMOpenAIAPIKey, s.LLMOpenAIBaseURL))
	}
	if s.LLMOpenAICompatibleBaseURL != "" {
		r.Register(NewOpenAICompatible(s.LLMOpenAICompatibleAPIKey, s.LLMOpenAICompatibleBaseURL).
			WithOllama(s.LLMOpenAICompatibleKind, s.LLMOllamaNumCtx))
		r.discoverOff = !s.LLMOpenAICompatibleDiscover
	}
	if s.LLMModelsPath != "" {
		// The P3 path indirection has already replaced a path with the file's
		// contents by the time the setting gets here, so JSON is the usual case;
		// a bare path (settings built by hand) is read as before.
		var err error
		if t := strings.TrimSpace(s.LLMModelsPath); strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[") {
			err = r.LoadModelsJSON([]byte(t))
		} else {
			err = r.LoadModelsFile(s.LLMModelsPath)
		}
		if err != nil {
			return nil, err
		}
	}
	// Startup refresh: never blocks and never fails startup (L8).
	r.refreshAsync()
	return r, nil
}

// Register adds (or replaces) a provider, optionally declaring its models.
func (r *Registry) Register(p Provider, models ...ModelSpec) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[p.Name()] = p
	if o, ok := p.(*OpenAI); ok && o.compatible {
		o.ctxFor = r.declaredContext
	}
	for _, m := range models {
		m.Provider = p.Name()
		r.addLocked(m)
	}
}

// AddModel declares a model (its provider need not be registered yet).
func (r *Registry) AddModel(m ModelSpec) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.addLocked(m)
}

func (r *Registry) addLocked(m ModelSpec) {
	for i, e := range r.models {
		if e.Provider == m.Provider && e.Model == m.Model {
			r.models[i] = m
			return
		}
	}
	r.models = append(r.models, m)
}

// LoadModelsFile reads the L4 models file: {"models":[...]} or a bare array.
func (r *Registry) LoadModelsFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("llm: cannot read models file %s: %w", path, err)
	}
	return r.LoadModelsJSON(data)
}

// LoadModelsJSON parses models JSON (see LoadModelsFile).
func (r *Registry) LoadModelsJSON(data []byte) error {
	var wrapped struct {
		Models []ModelSpec `json:"models"`
	}
	models := wrapped.Models
	if err := json.Unmarshal(data, &wrapped); err == nil && wrapped.Models != nil {
		models = wrapped.Models
	} else if err := json.Unmarshal(data, &models); err != nil {
		return fmt.Errorf("llm: models file must be {\"models\":[...]} or an array: %w", err)
	}
	for i, m := range models {
		if m.Provider == "" || m.Model == "" {
			return fmt.Errorf("llm: models entry %d needs provider and model", i)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range models {
		r.addLocked(m)
	}
	return nil
}

// Provider returns a registered provider by name.
func (r *Registry) Provider(name string) (Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[name]
	return p, ok
}

// Providers lists registered provider names, sorted.
func (r *Registry) Providers() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.providers))
	for n := range r.providers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Models lists usable models: declared ones whose provider is registered,
// then discovered ones (L8), merged per mergedLocked.
func (r *Registry) Models() []ModelSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.mergedLocked()
}

// mergedLocked merges declared and discovered models. A declared entry wins for
// prices and the context override; it inherits discovered facts it lacks
// (context window for display, tool support, zero price of a local model).
func (r *Registry) mergedLocked() []ModelSpec {
	out := []ModelSpec{}
	disc := r.disc.models
	used := map[string]bool{}
	for _, m := range r.models {
		if _, ok := r.providers[m.Provider]; !ok {
			continue
		}
		for _, d := range disc {
			if d.Provider == m.Provider && d.Model == m.Model {
				used[d.Provider+"\x00"+d.Model] = true
				if m.ContextWindow == 0 {
					m.ContextWindow = d.ContextWindow
				}
				if m.SupportsTools == nil {
					m.SupportsTools = d.SupportsTools
				}
				if !m.Priced && d.Priced {
					m.Priced, m.Prices = true, d.Prices
				}
			}
		}
		out = append(out, m)
	}
	for _, d := range disc {
		if _, ok := r.providers[d.Provider]; ok && !used[d.Provider+"\x00"+d.Model] {
			out = append(out, d)
		}
	}
	return out
}

// declaredContext is the models-file num_ctx override of a model, 0 if none.
func (r *Registry) declaredContext(model string) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, m := range r.models {
		if m.Provider == "openai-compatible" && m.Model == model {
			return m.ContextWindow
		}
	}
	return 0
}

// Lookup finds a usable model. costKnown is false when the model has no price.
func (r *Registry) Lookup(provider, model string) (spec ModelSpec, costKnown bool, ok bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if _, has := r.providers[provider]; !has {
		return ModelSpec{}, false, false
	}
	for _, m := range r.mergedLocked() {
		if m.Provider == provider && m.Model == model {
			return m, m.Priced, true
		}
	}
	return ModelSpec{}, false, false
}
