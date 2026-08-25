// Package bashtool adapts Weaver's shell tool to Grok's
// run_terminal_command contract.
package bashtool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	gschema "github.com/google/jsonschema-go/jsonschema"

	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/tool"
	canonical "github.com/mackross/agentloom/threads/tools/beta/bashtool"
)

const Name = "run_terminal_command"

// Config is Weaver's canonical shell configuration. Async is always disabled
// by New because run_terminal_command is strictly foreground-only.
type Config = canonical.Config

// Tool delegates command execution to Weaver's canonical shell tool.
type Tool struct {
	*canonical.Tool
}

var (
	_ threads.ToolProvider = (*Tool)(nil)
	_ threads.ToolResolver = (*Tool)(nil)
)

// New constructs a foreground-only run_terminal_command tool.
func New(cfg Config) *Tool {
	cfg.Async = false
	cfg.Thread = nil
	return &Tool{Tool: canonical.New(cfg)}
}

// ToolsSnapshot replaces the canonical bash contract with Grok's contract.
func (t *Tool) ToolsSnapshot(thread threads.Thread) threads.ToolsSnapshot {
	zero := float64(0)
	snapshot := t.Tool.ToolsSnapshot(thread)
	snapshot.Snapshot.Offered[0] = threads.ToolSpec{
		Name: Name,
		Description: "Run a terminal command in the current working directory. Use only as a last " +
			"resort; prefer specialized Go tools for Go discovery, diagnostics, tests, and refactors.",
		Payload: tool.PayloadJSONSchema(gschema.Schema{
			Type: "object",
			Properties: map[string]*gschema.Schema{
				"command": {
					Type:        "string",
					Description: "The terminal command to run.",
				},
				"timeout": {
					Type:        "integer",
					Minimum:     &zero,
					Description: "Optional timeout in milliseconds. Use 0 for no timeout.",
				},
				"description": {
					Type:        "string",
					Description: "Short explanation of why the command is being run.",
				},
			},
			Required:             []string{"command", "description"},
			AdditionalProperties: &gschema.Schema{Not: &gschema.Schema{}},
			PropertyOrder:        []string{"command", "timeout", "description"},
		}),
	}
	snapshot.Snapshot.Allowed = []string{Name}
	snapshot.Handlers[0].Name = Name
	return snapshot
}

type args struct {
	Command     string `json:"command"`
	Timeout     int    `json:"timeout,omitempty"`
	Description string `json:"description"`
}

// ResolveTool validates and translates the Grok call, then delegates all
// command behavior to Weaver. The wrapper applies the millisecond timeout via
// context so no lossy seconds conversion occurs.
func (t *Tool) ResolveTool(ctx context.Context, thread threads.Thread, call threads.ToolCall, load json.RawMessage) (threads.ToolDispatch, error) {
	if call.Name != Name {
		return threads.ToolDispatch{}, fmt.Errorf("tool %q not found", call.Name)
	}
	var input args
	dec := json.NewDecoder(bytes.NewBufferString(call.Payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&input); err != nil {
		return payloadError(call, err), nil
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple JSON values")
		}
		return payloadError(call, err), nil
	}
	if input.Command == "" {
		return payloadError(call, fmt.Errorf("command is required")), nil
	}
	if input.Description == "" {
		return payloadError(call, fmt.Errorf("description is required")), nil
	}
	if input.Timeout < 0 {
		return payloadError(call, fmt.Errorf("timeout must be greater than or equal to 0")), nil
	}

	if input.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(input.Timeout)*time.Millisecond)
		defer cancel()
	}
	// Description is wrapper metadata and intentionally does not affect
	// canonical execution.
	payload, err := json.Marshal(struct {
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
	}{Command: input.Command})
	if err != nil {
		return threads.ToolDispatch{}, err
	}
	call.Name = "bash"
	call.Payload = string(payload)
	return t.Tool.ResolveTool(ctx, thread, call, load)
}

func payloadError(call threads.ToolCall, err error) threads.ToolDispatch {
	message := fmt.Sprintf("tool %q payload: %v", call.Name, err)
	return threads.ToolDispatch{Items: []threads.Item{threads.ToolCallResult{
		CallID: call.CallID,
		Output: message,
		Data:   map[string]any{"error": message},
	}}}
}
