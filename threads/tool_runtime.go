package threads

import (
	"context"
	"encoding/json"
)

type ToolProvider interface {
	ToolsSnapshot() ToolsSnapshot
}

type ToolContinue string

const (
	// ToolContinueAuto is the zero value.
	// When a tool dispatch produces a terminal tool result, the thread will
	// ensure a follow-up SendItem is queued automatically.
	ToolContinueAuto ToolContinue = ""
	// ToolContinueManual suppresses automatic follow-up send scheduling.
	// The tool result is still recorded on the tape, but some later explicit
	// SendItem is required before the model continues.
	ToolContinueManual ToolContinue = "manual"
)

type ToolDispatch struct {
	Started bool
	// Continue defaults to ToolContinueAuto when left unset.
	Continue ToolContinue
	Items    []Item
}

type ToolResolver interface {
	ResolveTool(context.Context, ToolCall, json.RawMessage) (ToolDispatch, error)
}

type ToolCallResultable interface {
	ToolCallID() string
	ToolOutput() string
	ToolData() map[string]any
}
