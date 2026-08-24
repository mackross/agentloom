package tool

import (
	"context"

	"github.com/mackross/agentloom/threads"
)

type TypedHandlerFunc[T any] func(context.Context, threads.Thread, Call, T) Item

type jsonHandler[T any] struct {
	JSONValidation
	fn TypedHandlerFunc[T]
}

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
	payload := PayloadFor[T]().(PayloadJSONSchema)
	return &jsonHandler[T]{
		JSONValidation: NewJSONValidation(payload, DefaultJSONValidationMaxRetries),
		fn:             fn,
	}
}

func (h *jsonHandler[T]) HandleToolCall(ctx context.Context, thread threads.Thread, call Call, ret ReturnItem) (Handling, error) {
	var args T
	if result, continueMode := h.ValidateInto(thread, call, &args, nil); result != nil {
		return Handling{Continue: continueMode}, ret(*result)
	}
	return Handling{}, ret(h.fn(ctx, thread, call, args))
}
