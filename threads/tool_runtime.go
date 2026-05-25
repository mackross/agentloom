package threads

import (
	"context"
	"encoding/json"
)

type ToolProvider interface {
	ToolsSnapshot(Thread) ToolsSnapshot
}

type ToolContinue string

const (
	// ToolContinueAuto is the zero value.
	// When a tool dispatch produces a terminal tool result, the thread will
	// ensure a follow-up SendItem is queued automatically.
	ToolContinueAuto ToolContinue = ""
	// ToolContinueManual suppresses automatic follow-up send scheduling.
	// The tool result is still recorded in the thread items, but some later explicit
	// SendItem is required before the model continues.
	ToolContinueManual ToolContinue = "manual"
)

type ToolRecovery string

const (
	// ToolRecoverySafe marks a started tool dispatch as safe to recover/replay.
	ToolRecoverySafe ToolRecovery = "safe"
	// ToolRecoveryUnsafe marks a started tool dispatch as unsafe to recover/replay.
	ToolRecoveryUnsafe ToolRecovery = "unsafe"
)

type ToolDispatch struct {
	Started bool
	// Continue defaults to ToolContinueAuto when left unset.
	Continue ToolContinue
	// Recovery is only persisted when Started is true and a ToolCallStarted item
	// is durably recorded in the thread items.
	Recovery ToolRecovery
	Items    []Item
}

type ToolResolver interface {
	// ResolveTool receives a context canceled by CancelCurrentTurn when the
	// canceled LLM streamer turn produced this tool call.
	ResolveTool(context.Context, Thread, ToolCall, json.RawMessage) (ToolDispatch, error)
}
