// Package filetool provides Grok-facing adapters for AgentLoom's canonical
// filesystem tools.
package filetool

import (
	"context"
	"encoding/json"
	"fmt"

	gschema "github.com/google/jsonschema-go/jsonschema"

	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/tool"
	canonical "github.com/mackross/agentloom/threads/tool/filetool"
)

const readFileToolName = "read_file"

// ReadConfig is the canonical filesystem reader configuration.
type ReadConfig = canonical.ReadConfig

// ReadFileTool exposes the canonical filesystem reader using Grok's
// read_file name and target_file argument.
//
// The concrete embedding is intentional: all filesystem behavior remains in
// the canonical reader; this type only adapts its model-facing contract.
type ReadFileTool struct {
	*canonical.ReadTool
}

var (
	_ threads.ToolProvider = (*ReadFileTool)(nil)
	_ threads.ToolResolver = (*ReadFileTool)(nil)
)

// NewReadFileTool constructs a Grok-facing wrapper around the canonical
// filesystem read tool.
func NewReadFileTool(cfg ReadConfig) *ReadFileTool {
	return &ReadFileTool{ReadTool: canonical.NewReadTool(cfg)}
}

// ToolsSnapshot exposes only read_file, never the canonical read name.
func (r *ReadFileTool) ToolsSnapshot(_ threads.Thread) threads.ToolsSnapshot {
	zero := float64(0)
	spec := threads.ToolSpec{
		Name:        readFileToolName,
		Description: "Read text contents of a file.",
		Payload: tool.PayloadJSONSchema(gschema.Schema{
			Type: "object",
			Properties: map[string]*gschema.Schema{
				"target_file": {
					Type:        "string",
					Description: "Relative or absolute path of the file to read.",
				},
				"offset": {
					Type:        "integer",
					Description: "One-based line number to start reading from.",
				},
				"limit": {
					Type:        "integer",
					Minimum:     &zero,
					Description: "Number of lines to read. Use 0 for the default limit.",
				},
			},
			Required:             []string{"target_file"},
			AdditionalProperties: &gschema.Schema{Not: &gschema.Schema{}},
			PropertyOrder:        []string{"target_file", "offset", "limit"},
		}),
	}
	return threads.ToolsSnapshot{
		Snapshot: threads.ToolOfferSnapshot{Offered: []threads.ToolSpec{spec}},
		Handlers: []threads.ToolHandlerBinding{{Name: readFileToolName}},
	}
}

type readFileArgs struct {
	TargetFile string `json:"target_file"`
	Offset     int    `json:"offset,omitempty"`
	Limit      int    `json:"limit,omitempty"`
}

type canonicalReadArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

// ResolveTool translates the Grok-facing call and delegates it to the
// canonical reader.
func (r *ReadFileTool) ResolveTool(ctx context.Context, thread threads.Thread, call threads.ToolCall, load json.RawMessage) (threads.ToolDispatch, error) {
	if call.Name != readFileToolName {
		return threads.ToolDispatch{}, fmt.Errorf("tool %q not found", call.Name)
	}

	var args readFileArgs
	if err := decode(call.Payload, &args); err != nil {
		return payloadError(call, err), nil
	}
	if args.TargetFile == "" {
		return payloadError(call, fmt.Errorf("target_file is required")), nil
	}
	if args.Limit < 0 {
		return payloadError(call, fmt.Errorf("limit must be greater than or equal to 0")), nil
	}

	payload, err := json.Marshal(canonicalReadArgs{
		Path:   args.TargetFile,
		Offset: args.Offset,
		Limit:  args.Limit,
	})
	if err != nil {
		return threads.ToolDispatch{}, err
	}
	call.Name = "read"
	call.Payload = string(payload)
	return r.ReadTool.ResolveTool(ctx, thread, call, load)
}

func payloadError(call threads.ToolCall, err error) threads.ToolDispatch {
	message := fmt.Sprintf("tool %q payload: %v", call.Name, err)
	return threads.ToolDispatch{
		Started: true,
		Items: []threads.Item{threads.ToolCallResult{
			CallID: call.CallID,
			Output: message,
			Data:   map[string]any{"error": message},
		}},
	}
}
