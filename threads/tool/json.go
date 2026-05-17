package tool

import (
	"context"
	"fmt"

	"github.com/mackross/agentloom/threads"
)

type TypedHandlerFunc[T any] func(context.Context, *threads.Thread, Call, T) Item

func JSON[T any](name, desc string, fn TypedHandlerFunc[T]) (Spec, Handler) {
	return Spec{
		Name:        name,
		Description: desc,
		Payload:     PayloadFor[T](),
	}, JSONHandler(fn)
}

func JSONHandler[T any](fn TypedHandlerFunc[T]) Handler {
	if fn == nil {
		panic("tool.JSONHandler requires non-nil handler")
	}
	return HandlerFunc(func(ctx context.Context, thread *threads.Thread, call Call, ret ReturnItem) (Handling, error) {
		var args T
		if err := call.UnmarshalJSON(&args); err != nil {
			return Handling{}, ret(ResultError(call, fmt.Errorf("tool %q payload: %w", call.Name, err)))
		}
		return Handling{}, ret(fn(ctx, thread, call, args))
	})
}
