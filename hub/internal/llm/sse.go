package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// readSSE parses a text/event-stream, calling fn once per event. fn returning
// a non-nil error stops the parse with that error.
func readSSE(r io.Reader, fn func(event, data string) error) error {
	br := bufio.NewReaderSize(r, 64*1024)
	var event string
	var data []string
	flush := func() error {
		if len(data) == 0 {
			event = ""
			return nil
		}
		err := fn(event, strings.Join(data, "\n"))
		event, data = "", nil
		return err
	}
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			line = strings.TrimRight(line, "\r\n")
			switch {
			case line == "":
				if ferr := flush(); ferr != nil {
					return ferr
				}
			case strings.HasPrefix(line, ":"):
			default:
				field, value, _ := strings.Cut(line, ":")
				value = strings.TrimPrefix(value, " ")
				switch field {
				case "event":
					event = value
				case "data":
					data = append(data, value)
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				return flush()
			}
			return err
		}
	}
}

// postStream sends a JSON POST and returns the response for streaming. A status
// >= 400 becomes an error carrying a truncated body (never the request, so
// keys cannot leak).
func postStream(ctx context.Context, client *http.Client, url string, headers map[string]string, body any) (*http.Response, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range headers {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, &HTTPError{Status: resp.StatusCode, Body: strings.TrimSpace(string(snippet))}
	}
	return resp, nil
}

// HTTPError is a non-2xx answer from a provider.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("llm: provider returned HTTP %d: %s", e.Status, e.Body)
}

// emitter sends deltas, giving up when ctx is cancelled.
type emitter struct {
	ctx context.Context
	ch  chan<- Delta
}

func (e emitter) send(d Delta) bool {
	select {
	case e.ch <- d:
		return true
	case <-e.ctx.Done():
		return false
	}
}

func (e emitter) fail(err error) {
	if e.ctx.Err() != nil {
		return
	}
	e.send(Delta{Type: DeltaError, FinishReason: FinishError, Err: err})
}

// sanitizeTools applies the L6 rule: a tool the provider would reject is
// dropped with a warning, never failing the request. It returns the kept tools
// with a normalised schema (missing schema or type gets the object default).
func sanitizeTools(provider string, tools []Tool) []Tool {
	out := make([]Tool, 0, len(tools))
	for _, t := range tools {
		if reason := toolProblem(t); reason != "" {
			slog.Warn("llm: dropping tool the provider cannot accept",
				"provider", provider, "tool", t.Name, "reason", reason)
			continue
		}
		schema := t.InputSchema
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		} else if _, has := schema["type"]; !has {
			cp := make(map[string]any, len(schema)+1)
			for k, v := range schema {
				cp[k] = v
			}
			cp["type"] = "object"
			schema = cp
		}
		t.InputSchema = schema
		out = append(out, t)
	}
	return out
}

func toolProblem(t Tool) string {
	if !validToolName(t.Name) {
		return "name must match [a-zA-Z0-9_-]{1,64}"
	}
	if t.InputSchema == nil {
		return ""
	}
	if typ, has := t.InputSchema["type"]; has {
		if s, ok := typ.(string); !ok || s != "object" {
			return "schema root must be an object"
		}
	}
	for _, k := range []string{"oneOf", "anyOf", "allOf", "not", "enum"} {
		if _, has := t.InputSchema[k]; has {
			return "schema root may not use " + k
		}
	}
	if props, has := t.InputSchema["properties"]; has {
		if _, ok := props.(map[string]any); !ok {
			return "schema properties must be an object"
		}
	}
	return ""
}

func validToolName(n string) bool {
	if n == "" || len(n) > 64 {
		return false
	}
	for _, c := range n {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// parseArgs decodes a tool-call argument string. Unparseable JSON (typically a
// truncated stream) is kept under _raw_arguments so the tool call fails
// validation visibly instead of the run dying.
func parseArgs(raw string) map[string]any {
	if strings.TrimSpace(raw) == "" {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil || m == nil {
		return map[string]any{"_raw_arguments": raw}
	}
	return m
}
