package tool

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mackross/agentloom/threads"
)

// StructTool is a single required JSON-schema tool backed by a Go struct type.
// It implements threads.ToolProvider and threads.ToolResolver, so it can be
// installed directly with Thread.SetToolProvider and Thread.SetToolResolver.
type StructTool[T any] struct {
	spec Spec
	fn   StructToolDelegateFunc[T]
}

type StructToolDelegateFunc[T any] func(context.Context, Call, T)

// NewStructTool creates a single-tool toolbox for T. The tool is offered as the
// only allowed tool, marked required, and parallel tool use is disabled.
//
// If fn is nil, successful calls return a simple "ok" tool result.
func NewStructTool[T any](name, desc string, fn StructToolDelegateFunc[T]) *StructTool[T] {
	return &StructTool[T]{
		spec: Spec{
			Name:        name,
			Description: desc,
			Payload:     PayloadFor[T](),
		},
		fn: fn,
	}
}

func (s *StructTool[T]) ToolsSnapshot() threads.ToolsSnapshot {
	parallel := false
	return threads.ToolsSnapshot{
		Snapshot: threads.ToolOfferSnapshot{
			Offered:  []threads.ToolSpec{s.spec},
			Allowed:  []string{s.spec.Name},
			Parallel: &parallel,
			Required: true,
		},
		Handlers: []threads.ToolHandlerBinding{{Name: s.spec.Name}},
	}
}

func (s *StructTool[T]) ResolveTool(ctx context.Context, call threads.ToolCall, _ json.RawMessage) (threads.ToolDispatch, error) {
	if call.Name != s.spec.Name {
		return threads.ToolDispatch{}, fmt.Errorf("tool %q not found", call.Name)
	}
	var args T
	if err := call.UnmarshalJSON(&args); err != nil {
		return threads.ToolDispatch{
			Started:  true,
			Continue: threads.ToolContinueManual,
			Items: []threads.Item{
				ResultError(call, fmt.Errorf("tool %q payload: %w", call.Name, err)),
			},
		}, nil
	}
	if s.fn != nil {
		s.fn(ctx, call, args)
	}
	return threads.ToolDispatch{
		Started:  true,
		Continue: threads.ToolContinueManual,
		Items:    []threads.Item{ResultText(call, "ok")},
	}, nil
}
