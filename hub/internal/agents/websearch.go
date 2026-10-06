package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

const (
	webSearchName    = "switchboard.web.search"
	webSearchDefault = 8
	webSearchMax     = 20
)

var searchClient = &http.Client{Timeout: 30 * time.Second}

// webSearchTool is offered (see buildCatalog) only while MCP_SWITCHBOARD_SEARXNG_URL
// is set. Like skill.load it needs no capability and is not in sbTools.
var webSearchTool = &sbTool{
	name: webSearchName,
	desc: "Search the internet (through the hub's SearXNG instance) and return a list of results, each with a title, URL and snippet. Use it for current information or anything you cannot find in your tools; it returns snippets only, not full pages.",
	schema: obj([]string{"query"}, map[string]any{
		"query":      typ("string", "the search query"),
		"categories": typ("string", "comma-separated SearXNG categories, e.g. general, news, science, it; default general"),
		"language":   typ("string", "language code such as en or de; default auto"),
		"time_range": map[string]any{"type": "string", "enum": []string{"day", "week", "month", "year"}, "description": "only results from this period"},
		"page":       typ("integer", "result page, starting at 1"),
		"limit":      typ("integer", fmt.Sprintf("maximum results to return, default %d, at most %d", webSearchDefault, webSearchMax)),
	}),
	ann:     &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: bp(true)},
	visible: func(store.Capabilities) bool { return true },
	run: func(m *Manager, ctx context.Context, _ *callCtx, args map[string]any) (any, error) {
		return searxSearch(ctx, m.set.SearxngURL, args)
	},
}

func init() {
	registerExtraTool(webSearchTool, func(m *Manager) bool { return m.set.SearxngURL != "" })
}

func searxSearch(ctx context.Context, base string, args map[string]any) (any, error) {
	q := strings.TrimSpace(argStr(args, "query"))
	if q == "" {
		return nil, fmt.Errorf("%w: query is required", ErrInvalid)
	}
	v := url.Values{"q": {q}, "format": {"json"}}
	for _, k := range []string{"categories", "language", "time_range"} {
		if s := strings.TrimSpace(argStr(args, k)); s != "" {
			v.Set(k, s)
		}
	}
	if n, ok := args["page"].(float64); ok && n >= 1 {
		v.Set("pageno", fmt.Sprint(int(n)))
	}
	limit := webSearchDefault
	if n, ok := args["limit"].(float64); ok && n >= 1 {
		limit = min(int(n), webSearchMax)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/search?"+v.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := searchClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("search failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("search failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		hint := ""
		if resp.StatusCode == http.StatusForbidden {
			hint = " (is the json format enabled in the instance's search.formats?)"
		}
		return nil, fmt.Errorf("search failed: SearXNG returned %s%s", resp.Status, hint)
	}
	var doc struct {
		Results []struct {
			Title         string `json:"title"`
			URL           string `json:"url"`
			Content       string `json:"content"`
			Engine        string `json:"engine"`
			PublishedDate string `json:"publishedDate"`
		} `json:"results"`
		Answers     []any    `json:"answers"`
		Suggestions []string `json:"suggestions"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("search failed: SearXNG did not return JSON: %w", err)
	}
	results := make([]map[string]any, 0, limit)
	for _, r := range doc.Results {
		if len(results) == limit {
			break
		}
		row := map[string]any{"title": r.Title, "url": r.URL, "snippet": r.Content}
		if r.PublishedDate != "" {
			row["published"] = r.PublishedDate
		}
		results = append(results, row)
	}
	out := map[string]any{"query": q, "results": results}
	if len(doc.Answers) > 0 {
		out["answers"] = doc.Answers
	}
	if len(doc.Suggestions) > 0 {
		out["suggestions"] = doc.Suggestions
	}
	return out, nil
}
