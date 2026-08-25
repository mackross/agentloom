package websearchtool

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/mackross/agentloom/threads"
)

type duckDuckGoSearch struct{}

type duckDuckGoArgs struct {
	Query     string `json:"query" jsonschema:"Search query (e.g. 'latest AI news')"`
	Count     int    `json:"count" jsonschema:"Number of results to return, 1-10 (use 5 unless more context is needed)"`
	Page      int    `json:"page" jsonschema:"DuckDuckGo results page number, starting at 1."`
	Freshness string `json:"freshness" jsonschema:"Optional recency filter: day, week, month, or year. Leave empty for no freshness filter."`
	Domain    string `json:"domain" jsonschema:"Limit results to a specific domain, or empty string for no domain filter"`
}

func (duckDuckGoSearch) Payload() threads.ToolPayload {
	return threads.ToolPayloadFor[duckDuckGoArgs]()
}

func (duckDuckGoSearch) Search(ctx context.Context, a args) (string, error) {
	effectiveQuery := effectiveQuery(a)
	u, _ := url.Parse("https://duckduckgo.com/html/")
	q := u.Query()
	q.Set("q", effectiveQuery)
	if a.Page > 1 {
		q.Set("s", fmt.Sprintf("%d", (a.Page-1)*a.Count))
	}
	if f := ddgFreshness(a.Freshness); f != "" {
		q.Set("df", f)
	}
	u.RawQuery = q.Encode()

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; weaver-go/1.0)")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("DuckDuckGo fallback failed: HTTP %d %s", resp.StatusCode, resp.Status)
	}
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return "", err
	}
	results := parseDDGResults(doc)
	results = dedupe(results)
	if len(results) > a.Count {
		results = results[:a.Count]
	}
	return formatDuckDuckGo(a.Query, effectiveQuery, a.Page, results), nil
}

func parseDDGResults(doc *goquery.Document) []braveResult {
	var results []braveResult
	doc.Find(".result").Each(func(_ int, sel *goquery.Selection) {
		titleLink := sel.Find(".result__title a").First()
		title := cleanSpace(titleLink.Text())
		href, _ := titleLink.Attr("href")
		resultURL := normalizeDDGURL(href)
		desc := cleanSpace(sel.Find(".result__snippet").First().Text())
		if title == "" || resultURL == "" || strings.Contains(strings.ToLower(title), "duckduckgo") {
			return
		}
		results = append(results, braveResult{Title: title, URL: resultURL, Description: desc})
	})
	return results
}

func normalizeDDGURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "//") {
		raw = "https:" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if strings.Contains(u.Hostname(), "duckduckgo.com") && strings.HasPrefix(u.Path, "/l/") {
		if target := u.Query().Get("uddg"); target != "" {
			if decoded, err := url.QueryUnescape(target); err == nil {
				return decoded
			}
			return target
		}
	}
	return u.String()
}

func formatDuckDuckGo(query, effectiveQuery string, page int, results []braveResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Web search results for %q", query)
	if effectiveQuery != query {
		fmt.Fprintf(&b, " (effective query: %q)", effectiveQuery)
	}
	b.WriteString("\nSource: DuckDuckGo HTML fallback\n")
	if len(results) == 0 {
		b.WriteString("No results found.\n")
		return b.String()
	}
	for i, r := range results {
		fmt.Fprintf(&b, "\n%d. %s\n", i+1, fallback(r.Title, "(untitled)"))
		fmt.Fprintf(&b, "   URL: %s\n", r.URL)
		if r.Description != "" {
			fmt.Fprintf(&b, "   %s\n", r.Description)
		}
	}
	if len(results) > 0 {
		fmt.Fprintf(&b, "\nMore results: call web_search with page=%d\n", page+1)
	}
	return b.String()
}

func ddgFreshness(f string) string {
	switch f {
	case "day":
		return "d"
	case "week":
		return "w"
	case "month":
		return "m"
	case "year":
		return "y"
	default:
		return ""
	}
}

func cleanSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
