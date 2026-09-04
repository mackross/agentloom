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

// ToolProviderFunc adapts a function to ToolProvider. Thread.SetToolProvider
// clones the returned snapshot before queueing it, so the function may return
// shared data.
type ToolProviderFunc func(Thread) ToolsSnapshot

func (f ToolProviderFunc) ToolsSnapshot(thread Thread) ToolsSnapshot {
	return f(thread)
}

// ToolResolverFunc adapts a function to ToolResolver. The handler load data is
// already a private copy taken from the thread's ToolsSnapshot, so the function
// may retain it.
type ToolResolverFunc func(context.Context, Thread, ToolCall, json.RawMessage) (ToolDispatch, error)

func (f ToolResolverFunc) ResolveTool(ctx context.Context, thread Thread, call ToolCall, handlerLoadData json.RawMessage) (ToolDispatch, error) {
	return f(ctx, thread, call, handlerLoadData)
}
