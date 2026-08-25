// Package fetchpagetool adapts Weaver's page fetcher to Grok's web_fetch
// contract.
package fetchpagetool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/mackross/agentloom/threads"
	canonical "github.com/mackross/agentloom/threads/tools/beta/fetchpagetool"
)

const (
	Name            = "web_fetch"
	defaultMaxChars = 8_000
)

// Config contains the runtime policy hidden from model calls.
type Config struct {
	MaxChars int
	canonical.Config
}

type args struct {
	URL string `json:"url" jsonschema:"The public HTTP or HTTPS URL to fetch."`
}

// Tool exposes web_fetch while delegating execution to Weaver's canonical
// fetch_page implementation.
type Tool struct {
	*canonical.Tool
	maxChars int
}

// New creates a web_fetch tool.
func New(cfg Config) *Tool {
	maxChars := cfg.MaxChars
	if maxChars == 0 {
		maxChars = defaultMaxChars
	}
	return &Tool{
		Tool:     canonical.New(cfg.Config),
		maxChars: maxChars,
	}
}

// ToolsSnapshot replaces only the canonical tool's model-facing contract.
func (t *Tool) ToolsSnapshot(thread threads.Thread) threads.ToolsSnapshot {
	snapshot := t.Tool.ToolsSnapshot(thread)
	snapshot.Snapshot.Offered[0] = threads.ToolSpec{
		Name:        Name,
		Description: "Fetch the content of a public URL and return extracted Markdown.",
		Payload:     threads.ToolPayloadFor[args](),
	}
	snapshot.Snapshot.Allowed = []string{Name}
	snapshot.Handlers[0].Name = Name
	return snapshot
}

// ResolveTool injects runtime output policy and delegates all fetching behavior.
func (t *Tool) ResolveTool(ctx context.Context, thread threads.Thread, call threads.ToolCall, load json.RawMessage) (threads.ToolDispatch, error) {
	var input args
	dec := json.NewDecoder(bytes.NewBufferString(call.Payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&input); err != nil {
		message := fmt.Sprintf("tool %q payload: %v", call.Name, err)
		return threads.ToolDispatch{Items: []threads.Item{threads.ToolCallResult{
			CallID: call.CallID,
			Output: message,
			Data:   map[string]any{"error": message},
		}}}, nil
	}
	payload, err := json.Marshal(struct {
		URL      string `json:"url"`
		MaxChars int    `json:"maxChars"`
	}{URL: input.URL, MaxChars: t.maxChars})
	if err != nil {
		return threads.ToolDispatch{}, err
	}
	call.Name = canonical.Name
	call.Payload = string(payload)
	return t.Tool.ResolveTool(ctx, thread, call, load)
}
