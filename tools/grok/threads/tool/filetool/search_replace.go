// Package filetool adapts AgentLoom's canonical filesystem tools to Grok's
// model-facing contracts.
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

const searchReplaceName = "search_replace"

// SearchReplaceTool exposes one canonical exact-text edit under Grok's
// search_replace call shape. Embedding the canonical tool keeps its mutation
// queue, atomicity, postprocessing, diagnostics, and result behavior intact.
type SearchReplaceTool struct {
	*canonical.EditTool
}

var (
	_ threads.ToolProvider = (*SearchReplaceTool)(nil)
	_ threads.ToolResolver = (*SearchReplaceTool)(nil)
)

// NewSearchReplaceTool constructs a search_replace wrapper around the
// canonical edit tool.
func NewSearchReplaceTool(cfg canonical.EditConfig) *SearchReplaceTool {
	return &SearchReplaceTool{EditTool: canonical.NewEditTool(cfg)}
}

// ToolsSnapshot advertises only search_replace, never the embedded edit name.
func (s *SearchReplaceTool) ToolsSnapshot(threads.Thread) threads.ToolsSnapshot {
	return threads.ToolsSnapshot{
		Snapshot: threads.ToolOfferSnapshot{Offered: []threads.ToolSpec{{
			Name:        searchReplaceName,
			Description: "Replace one uniquely matching exact string in a file.",
			Payload:     searchReplacePayload(),
		}}},
		Handlers: []threads.ToolHandlerBinding{{Name: searchReplaceName}},
	}
}

type searchReplaceArgs struct {
	FilePath  string `json:"file_path"`
	OldString string `json:"old_string"`
	NewString string `json:"new_string"`
}

type canonicalEditArgs struct {
	Path  string             `json:"path"`
	Edits []canonicalEditOne `json:"edits"`
}

type canonicalEditOne struct {
	OldText string `json:"oldText"`
	NewText string `json:"newText"`
}

// ResolveTool translates the narrow call into exactly one canonical edit.
func (s *SearchReplaceTool) ResolveTool(ctx context.Context, thread threads.Thread, call threads.ToolCall, load json.RawMessage) (threads.ToolDispatch, error) {
	if call.Name != searchReplaceName {
		return threads.ToolDispatch{}, fmt.Errorf("search_replace: cannot resolve tool %q", call.Name)
	}
	var args searchReplaceArgs
	if err := decode(call.Payload, &args); err != nil {
		return payloadError(call, err), nil
	}
	payload, err := json.Marshal(canonicalEditArgs{
		Path: args.FilePath,
		Edits: []canonicalEditOne{{
			OldText: args.OldString,
			NewText: args.NewString,
		}},
	})
	if err != nil {
		return threads.ToolDispatch{}, err
	}
	call.Name = "edit"
	call.Payload = string(payload)
	return s.EditTool.ResolveTool(ctx, thread, call, load)
}

func searchReplacePayload() tool.Payload {
	one := 1
	return tool.PayloadJSONSchema(gschema.Schema{
		Type: "object",
		Properties: map[string]*gschema.Schema{
			"file_path": {
				Type:        "string",
				Description: "Relative or absolute path of the file to modify.",
			},
			"old_string": {
				Type:        "string",
				MinLength:   &one,
				Description: "Exact text to replace. It must match exactly once.",
			},
			"new_string": {
				Type:        "string",
				Description: "Exact replacement text.",
			},
		},
		Required:      []string{"file_path", "old_string", "new_string"},
		PropertyOrder: []string{"file_path", "old_string", "new_string"},
		Extra:         map[string]any{"additionalProperties": false},
	})
}
