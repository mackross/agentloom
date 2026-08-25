// Package fetchpagetool provides an AgentLoom-compatible web page fetch tool.
package fetchpagetool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/tools/beta/internal/htmltomdwasm"
	"github.com/mackross/agentloom/threads/tools/beta/internal/loomutil"
	"github.com/mackross/agentloom/threads/tools/beta/internal/toolcallcache"
	"github.com/mackross/agentloom/threads/tools/beta/internal/truncate"
)

const (
	name = "fetch_page"
	Name = name
)

// Config is the durable fetch_page tool configuration.
type Config struct {
	MaxLines int            `json:"maxLines,omitempty"`
	MaxBytes int            `json:"maxBytes,omitempty"`
	Async    bool           `json:"async,omitempty"`
	Thread   threads.Thread `json:"-"`
}

type args struct {
	URL      string `json:"url" jsonschema:"URL to fetch and extract content from"`
	MaxChars int    `json:"maxChars" jsonschema:"Maximum characters of extracted content to return (default 8000, max 30000)"`
}

// Tool implements threads.ToolProvider and threads.ToolResolver.
type Tool struct{ cfg Config }

// New creates a fetch_page tool.
func New(cfg Config) *Tool { return &Tool{cfg: cfg} }

// ToolsSnapshot returns the durable AgentLoom tool snapshot.
func (t *Tool) ToolsSnapshot(_ threads.Thread) threads.ToolsSnapshot {
	return loomutil.Snapshot(Name, "Fetch a web page and extract its content as clean markdown. Strips metadata front matter and skips images/SVG/base64 content.", threads.ToolPayloadFor[args](), t.cfg)
}

// ResolveTool executes a fetch_page call using the durable handler load data.
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
	if cfg.Async {
		return loomutil.AsyncResult(ctx, cfg.Thread, call, threads.ToolRecoveryUnsafe, func(ctx context.Context) threads.ToolCallResult {
			return run(ctx, call, cfg, a)
		})
	}
	return threads.ToolDispatch{Items: []threads.Item{run(ctx, call, cfg, a)}}, nil
}

func run(ctx context.Context, call threads.ToolCall, cfg Config, a args) threads.ToolCallResult {
	out, err := fetchPage(ctx, a)
	if err != nil {
		return loomutil.Error(call, err)
	}
	out = applyLimit(out, a.MaxChars)
	r := truncate.Head(out, cfg.MaxLines, cfg.MaxBytes)
	if r.Truncated {
		out = r.Content + r.Notice
	} else {
		out = r.Content
	}
	return loomutil.Result(call, out, nil)
}

func decodePayload(call threads.ToolCall) (args, error) {
	var a args
	if err := loomutil.DecodePayload(call, &a); err != nil {
		return a, err
	}
	a.URL = strings.TrimSpace(a.URL)
	if a.URL == "" {
		return a, fmt.Errorf("url is required")
	}
	if _, err := parsePublicHTTPURL(a.URL); err != nil {
		return a, err
	}
	if a.MaxChars == 0 {
		a.MaxChars = 8_000
	}
	if a.MaxChars < 1_000 || a.MaxChars > 30_000 {
		return a, fmt.Errorf("maxChars must be between 1000 and 30000")
	}
	return a, nil
}

func fetchPage(ctx context.Context, a args) (string, error) {
	u, err := parsePublicHTTPURL(a.URL)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/json,text/plain")
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; weaver-go/1.0)")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("fetch failed: HTTP %d %s", resp.StatusCode, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		return "", err
	}
	ct := strings.ToLower(resp.Header.Get("content-type"))
	var content string
	switch {
	case strings.Contains(ct, "application/json"):
		content = "```json\n" + string(body) + "\n```"
	case strings.Contains(ct, "text/plain"):
		content = string(body)
	case strings.Contains(ct, "application/pdf"):
		content = "[This URL is a PDF document. Content extraction is not supported.]"
	default:
		md, err := htmltomdwasm.HtmlToMdWithOptions(ctx, string(body), htmltomdwasm.Options{StripMetaFrontMatter: true})
		if err != nil {
			return "", err
		}
		content = md
	}
	return format(u.String(), content), nil
}

func format(rawURL, content string) string {
	return fmt.Sprintf("Page: %s\n%s\n---\n%s", pageTitle(content), rawURL, strings.TrimSpace(content))
}

func pageTitle(content string) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "# "))
		}
	}
	return "(untitled)"
}

func applyLimit(s string, maxChars int) string {
	if len(s) <= maxChars {
		return s
	}
	entry, err := toolcallcache.Put("fetch-page", s)
	if err != nil {
		return strings.TrimSpace(s[:smartCut(s, maxChars)]) + fmt.Sprintf("\n\n[... content truncated; failed to cache full page: %v]", err)
	}
	cut := smartCut(s, maxChars)
	return strings.TrimSpace(s[:cut]) + fmt.Sprintf("\n\n[... content truncated. Full content cached at %s]", entry.Path)
}

func smartCut(s string, max int) int {
	window := s[:max]
	if idx := strings.LastIndex(window, "\n\n"); idx > max*6/10 {
		return idx
	}
	if idx := strings.LastIndex(window, ". "); idx > max*6/10 {
		return idx + 1
	}
	if idx := strings.LastIndex(window, "\n"); idx > max*6/10 {
		return idx
	}
	return max
}

func parsePublicHTTPURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return nil, fmt.Errorf("invalid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("only http and https URLs are allowed")
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || host == "metadata.google.internal" || strings.HasPrefix(host, "127.") || strings.HasPrefix(host, "10.") || strings.HasPrefix(host, "192.168.") || strings.HasPrefix(host, "169.254.") || strings.HasPrefix(host, "0.") || strings.HasPrefix(host, "172.16.") || strings.HasPrefix(host, "172.17.") || strings.HasPrefix(host, "172.18.") || strings.HasPrefix(host, "172.19.") || strings.HasPrefix(host, "172.2") || strings.HasPrefix(host, "172.30.") || strings.HasPrefix(host, "172.31.") {
		return nil, fmt.Errorf("blocked URL: private/internal addresses are not allowed")
	}
	return u, nil
}

func errResult(call threads.ToolCall, err error) threads.ToolDispatch {
	return threads.ToolDispatch{Items: []threads.Item{loomutil.Error(call, err)}}
}
