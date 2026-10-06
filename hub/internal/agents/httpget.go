package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

const (
	httpGetName    = "switchboard.http.get"
	httpGetBodyCap = 200 << 10
)

var httpGetTool = &sbTool{
	name: httpGetName,
	desc: "Send an HTTP GET request to one of the API hosts the hub operator has allowlisted (internal hosts included) and return the status, content type and body; JSON bodies come back parsed. Only allowlisted hosts work, redirects are not followed, and Authorization or Cookie headers cannot be set. " +
		`Example: {"url": "http://inventory.internal:8080/api/items", "query": {"limit": "10"}}`,
	schema: obj([]string{"url"}, map[string]any{
		"url":     typ("string", "full http or https URL on an allowlisted host"),
		"query":   map[string]any{"type": "object", "description": "query parameters to append, name to string value", "additionalProperties": map[string]any{"type": "string"}},
		"headers": map[string]any{"type": "object", "description": "extra request headers, name to string value (not Authorization or Cookie)", "additionalProperties": map[string]any{"type": "string"}},
	}),
	ann:     &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: bp(true)},
	visible: func(store.Capabilities) bool { return true },
	run: func(m *Manager, ctx context.Context, _ *callCtx, args map[string]any) (any, error) {
		return httpGet(ctx, httpGetClient, m.set.HTTPGetAllowlist, args)
	},
}

func init() {
	registerExtraTool(httpGetTool, func(m *Manager) bool { return len(m.set.HTTPGetAllowlist) > 0 })
}

var httpGetClient = &http.Client{
	Timeout:       20 * time.Second,
	Transport:     &http.Transport{Proxy: nil, DisableKeepAlives: true},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// hostAllowed matches u against the allowlist: an entry is either a bare
// hostname (any port) or host:port (that port only, default ports implied).
func hostAllowed(u *url.URL, allow []string) bool {
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
	}
	for _, e := range allow {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == host || e == net.JoinHostPort(host, port) {
			return true
		}
	}
	return false
}

var forbiddenHeaders = map[string]bool{"authorization": true, "cookie": true, "proxy-authorization": true, "host": true}

func httpGet(ctx context.Context, client *http.Client, allow []string, args map[string]any) (any, error) {
	raw := strings.TrimSpace(argStr(args, "url"))
	u, err := url.Parse(raw)
	if raw == "" || err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return nil, fmt.Errorf("%w: url must be an absolute http or https URL", ErrInvalid)
	}
	if !hostAllowed(u, allow) {
		return nil, fmt.Errorf("%w: host %q is not in the http.get allowlist", ErrInvalid, u.Host)
	}
	if q, ok := args["query"].(map[string]any); ok && len(q) > 0 {
		v := u.Query()
		for k, val := range q {
			v.Set(k, fmt.Sprint(val))
		}
		u.RawQuery = v.Encode()
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	req.Header.Set("Accept", "application/json, text/plain;q=0.9, */*;q=0.5")
	if h, ok := args["headers"].(map[string]any); ok {
		for k, val := range h {
			if forbiddenHeaders[strings.ToLower(strings.TrimSpace(k))] {
				return nil, fmt.Errorf("%w: header %q may not be set", ErrInvalid, k)
			}
			req.Header.Set(k, fmt.Sprint(val))
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, httpGetBodyCap+1))
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	truncated := len(body) > httpGetBodyCap
	if truncated {
		body = body[:httpGetBodyCap]
	}
	ctype := resp.Header.Get("Content-Type")
	out := map[string]any{"status": resp.StatusCode, "content_type": ctype, "truncated": truncated}
	if loc := resp.Header.Get("Location"); loc != "" {
		out["location"] = loc
	}
	mt := strings.ToLower(strings.SplitN(ctype, ";", 2)[0])
	var parsed any
	if (mt == "application/json" || strings.HasSuffix(mt, "+json")) && !truncated && json.Unmarshal(body, &parsed) == nil {
		out["body"] = parsed
	} else {
		out["body"] = strings.ToValidUTF8(string(body), "")
	}
	return out, nil
}
