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
	spec     Spec
	delegate StructToolDelegate[T]
}

type StructToolDelegate[T any] interface {
	OnStructToolCall(context.Context, *threads.Thread, Call, T) Item
}

// NewStructTool creates a single-tool toolbox for T. The tool is offered as the
// only allowed tool, marked required, and parallel tool use is disabled.
//
// If delegate is nil, successful calls return a simple "ok" tool result.
func NewStructTool[T any](name, desc string, delegate StructToolDelegate[T]) *StructTool[T] {
	return &StructTool[T]{
		spec: Spec{
			Name:        name,
			Description: desc,
			Payload:     PayloadFor[T](),
		},
		delegate: delegate,
	}
}

func (s *StructTool[T]) ToolsSnapshot(_ *threads.Thread) threads.ToolsSnapshot {
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

func (s *StructTool[T]) ResolveTool(ctx context.Context, thread *threads.Thread, call threads.ToolCall, _ json.RawMessage) (threads.ToolDispatch, error) {
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
	item := Item(ResultText(call, "ok"))
	if s.delegate != nil {
		item = s.delegate.OnStructToolCall(ctx, thread, call, args)
		if item == nil {
			return threads.ToolDispatch{}, fmt.Errorf("tool %q delegate returned nil item", call.Name)
		}
	}
	return threads.ToolDispatch{
		Started:  true,
		Continue: threads.ToolContinueManual,
		Items:    []threads.Item{item},
	}, nil
}
