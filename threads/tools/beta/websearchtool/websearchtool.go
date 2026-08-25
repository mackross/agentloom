package websearchtool

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/tools/beta/internal/loomutil"
	"github.com/mackross/agentloom/threads/tools/beta/internal/truncate"
)

const (
	name = "web_search"
	Name = name
)

type Config struct {
	APIKey   string         `json:"apiKey,omitempty"`
	MaxLines int            `json:"maxLines,omitempty"`
	MaxBytes int            `json:"maxBytes,omitempty"`
	Thread   threads.Thread `json:"-"`
}

type args struct {
	Query     string `json:"query" jsonschema:"Search query (e.g. 'latest AI news')"`
	Count     int    `json:"count" jsonschema:"Number of results to return, 1-10 (use 5 unless more context is needed)"`
	Page      int    `json:"page" jsonschema:"DuckDuckGo results page number, starting at 1. Ignored by Brave Search."`
	Offset    int    `json:"offset" jsonschema:"Brave Search result offset. Ignored by DuckDuckGo."`
	Freshness string `json:"freshness" jsonschema:"Optional recency filter: day, week, month, or year. Leave empty for no freshness filter."`
	Domain    string `json:"domain" jsonschema:"Limit results to a specific domain, or empty string for no domain filter"`
}

type searcher interface {
	Search(context.Context, args) (string, error)
	Payload() threads.ToolPayload
}

type Tool struct{ cfg Config }

func New(cfg Config) *Tool { return &Tool{cfg: cfg} }

func (t *Tool) ToolsSnapshot(_ threads.Thread) threads.ToolsSnapshot {
	backend := searchBackend(t.cfg)
	return loomutil.Snapshot(Name, "Search the web for general research.", backend.Payload(), t.cfg)
}

func (t *Tool) ResolveTool(ctx context.Context, thread threads.Thread, call threads.ToolCall, load json.RawMessage) (threads.ToolDispatch, error) {
	var cfg Config
	if err := loomutil.DecodeLoad(load, &cfg); err != nil {
		return threads.ToolDispatch{}, err
	}
	cfg.Thread = t.cfg.Thread
	a, err := decodePayload(call)
	if err != nil {
		return errResult(call, err), nil
	}
	if cfg.Thread == nil {
		return threads.ToolDispatch{}, fmt.Errorf("async web_search requires a thread with an event loop")
	}
	backend := searchBackend(cfg)
	return resolveAsync(ctx, cfg.Thread, call, cfg, backend, a)
}

func resolveAsync(ctx context.Context, thread threads.Thread, call threads.ToolCall, cfg Config, backend searcher, a args) (threads.ToolDispatch, error) {
	return loomutil.AsyncResult(ctx, thread, call, threads.ToolRecoveryUnsafe, func(ctx context.Context) threads.ToolCallResult {
		out, err := backend.Search(ctx, a)
		if err != nil {
			return loomutil.Error(call, err)
		}
		r := truncate.Head(out, cfg.MaxLines, cfg.MaxBytes)
		if r.Truncated {
			out = r.Content + r.Notice
		} else {
			out = r.Content
		}
		return loomutil.Result(call, out, nil)
	})
}

func searchBackend(cfg Config) searcher {
	key, err := braveAPIKey(cfg.APIKey)
	if err == nil {
		return braveSearch{apiKey: key}
	}
	return duckDuckGoSearch{}
}

func decodePayload(call threads.ToolCall) (args, error) {
	var a args
	if err := loomutil.DecodePayload(call, &a); err != nil {
		return a, err
	}
	a.Query = strings.TrimSpace(a.Query)
	if a.Query == "" {
		return a, fmt.Errorf("query is required")
	}
	if a.Count == 0 {
		a.Count = 5
	}
	if a.Count < 1 || a.Count > 10 {
		return a, fmt.Errorf("count must be between 1 and 10")
	}
	if a.Page == 0 {
		a.Page = 1
	}
	if a.Page < 1 {
		return a, fmt.Errorf("page must be >= 1")
	}
	if a.Offset < 0 {
		return a, fmt.Errorf("offset must be >= 0")
	}
	a.Freshness = strings.TrimSpace(strings.ToLower(a.Freshness))
	switch a.Freshness {
	case "", "day", "week", "month", "year":
	default:
		return a, fmt.Errorf("freshness must be empty or one of day, week, month, year")
	}
	a.Domain = strings.TrimSpace(a.Domain)
	return a, nil
}

func braveAPIKey(configKey string) (string, error) {
	if key := strings.TrimSpace(configKey); key != "" {
		return key, nil
	}
	if key := strings.TrimSpace(os.Getenv("BRAVE_API_KEY")); key != "" {
		return key, nil
	}
	return "", fmt.Errorf("no Brave API key configured")
}

type braveSearch struct{ apiKey string }

type braveArgs struct {
	Query     string `json:"query" jsonschema:"Search query (e.g. 'latest AI news')"`
	Count     int    `json:"count" jsonschema:"Number of results to return, 1-10 (use 5 unless more context is needed)"`
	Offset    int    `json:"offset" jsonschema:"Brave Search result offset."`
	Freshness string `json:"freshness" jsonschema:"Optional recency filter: day, week, month, or year. Leave empty for no freshness filter."`
	Domain    string `json:"domain" jsonschema:"Limit results to a specific domain, or empty string for no domain filter"`
}

func (braveSearch) Payload() threads.ToolPayload { return threads.ToolPayloadFor[braveArgs]() }

type braveResponse struct {
	Query *struct {
		Original             string `json:"original"`
		Altered              string `json:"altered"`
		MoreResultsAvailable bool   `json:"more_results_available"`
	} `json:"query"`
	Web *struct {
		Results []braveResult `json:"results"`
	} `json:"web"`
}

type braveResult struct {
	Title         string   `json:"title"`
	URL           string   `json:"url"`
	Description   string   `json:"description"`
	Age           string   `json:"age"`
	PageAge       string   `json:"page_age"`
	ExtraSnippets []string `json:"extra_snippets"`
}

func (s braveSearch) Search(ctx context.Context, a args) (string, error) {
	effectiveQuery := effectiveQuery(a)
	u, _ := url.Parse("https://api.search.brave.com/res/v1/web/search")
	q := u.Query()
	q.Set("q", effectiveQuery)
	q.Set("count", fmt.Sprintf("%d", a.Count))
	if a.Offset > 0 {
		q.Set("offset", fmt.Sprintf("%d", a.Offset))
	}
	q.Set("extra_snippets", "true")
	q.Set("text_decorations", "false")
	if f := braveFreshness(a.Freshness); f != "" {
		q.Set("freshness", f)
	}
	u.RawQuery = q.Encode()

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", s.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("Brave Search failed: HTTP %d %s", resp.StatusCode, resp.Status)
	}
	var data braveResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return "", err
	}
	results := dedupe(data.WebResults())
	if len(results) > a.Count {
		results = results[:a.Count]
	}
	return formatBrave(a.Query, effectiveQuery, data, results), nil
}

func (r braveResponse) WebResults() []braveResult {
	if r.Web == nil {
		return nil
	}
	return r.Web.Results
}

func braveFreshness(f string) string {
	switch f {
	case "day":
		return "pd"
	case "week":
		return "pw"
	case "month":
		return "pm"
	case "year":
		return "py"
	default:
		return ""
	}
}

func effectiveQuery(a args) string {
	if a.Domain != "" && !strings.Contains(strings.ToLower(a.Query), "site:") {
		return "site:" + a.Domain + " " + a.Query
	}
	return a.Query
}

func dedupe(in []braveResult) []braveResult {
	seen := map[string]bool{}
	out := make([]braveResult, 0, len(in))
	for _, r := range in {
		key := strings.TrimRight(strings.ToLower(r.URL), "/")
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, r)
	}
	return out
}

func formatBrave(query, effectiveQuery string, data braveResponse, results []braveResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Web search results for %q", query)
	if effectiveQuery != query {
		fmt.Fprintf(&b, " (effective query: %q)", effectiveQuery)
	}
	b.WriteString("\nSource: Brave Search\n")
	if data.Query != nil && data.Query.Altered != "" && data.Query.Altered != data.Query.Original {
		fmt.Fprintf(&b, "Query corrected: %s -> %s\n", data.Query.Original, data.Query.Altered)
	}
	if len(results) == 0 {
		b.WriteString("No results found.\n")
		return b.String()
	}
	for i, r := range results {
		fmt.Fprintf(&b, "\n%d. %s\n", i+1, fallback(r.Title, "(untitled)"))
		if age := fallback(r.Age, r.PageAge); age != "" {
			fmt.Fprintf(&b, "   Age: %s\n", age)
		}
		fmt.Fprintf(&b, "   URL: %s\n", r.URL)
		if r.Description != "" {
			fmt.Fprintf(&b, "   %s\n", r.Description)
		}
		for _, snip := range r.ExtraSnippets {
			if strings.TrimSpace(snip) != "" {
				fmt.Fprintf(&b, "   - %s\n", snip)
			}
		}
	}
	return b.String()
}

func fallback(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func errResult(call threads.ToolCall, err error) threads.ToolDispatch {
	return threads.ToolDispatch{Items: []threads.Item{loomutil.Error(call, err)}}
}
