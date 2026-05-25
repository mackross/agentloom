package threads

import (
	"context"
	"fmt"
)

// StartToolResult is a convenience helper for the common case where a resolver
// wants to mark a tool call started, run work in a goroutine, and eventually
// queue exactly one ToolCallResult through the event loop if the call is still
// outstanding. fn is invoked synchronously so callers can choose the durable
// recovery classification at dispatch time while still deferring the expensive
// work to the returned worker.
//
// Do not use this helper for customized execution lifecycles, multiple/early
// return values, non-standard dispatch metadata, or custom queueing behavior;
// implement ToolResolver/ToolProvider directly for those cases.
func (l *EventLoop) StartToolResult(ctx context.Context, call ToolCall, fn func() (ToolRecovery, func(context.Context) ToolCallResult)) ToolDispatch {
	recovery, worker := fn()
	if worker == nil {
		panic("threads event loop start tool result requires a worker")
	}
	go func() {
		ready := false
		if err := l.doLocal(ctx, func(thread *thread) error {
			var err error
			ready, err = toolCallOutstanding(thread, call.CallID)
			return err
		}); err != nil || !ready {
			return
		}

		item := worker(ctx)
		item.CallID = call.CallID
		_ = l.doLocal(context.Background(), func(thread *thread) error {
			outstanding, err := toolCallOutstanding(thread, call.CallID)
			if err != nil || !outstanding {
				return err
			}
			thread.QueueItem(item)
			return nil
		})
	}()
	return ToolDispatch{Started: true, Recovery: recovery}
}

func (t *thread) ReturnAsyncToolItem(ctx context.Context, callID string, item Item) error {
	if item == nil {
		return fmt.Errorf("threads return nil tool item")
	}
	if t.loop == nil {
		return fmt.Errorf("threads async tool result requires event loop")
	}
	return t.loop.doLocal(ctx, func(thread *thread) error {
		outstanding, err := toolCallOutstanding(thread, callID)
		if err != nil || !outstanding {
			return err
		}
		thread.QueueItem(item)
		return nil
	})
}

func toolCallOutstanding(thread *thread, callID string) (bool, error) {
	snap, err := thread.Snapshot()
	if err != nil {
		return false, err
	}
	requested, started, completed := toolCallState(snap, callID)
	return requested && started && !completed, nil
}

func toolCallState(snap ThreadSnapshot, callID string) (requested, started, completed bool) {
	for _, item := range snap.Items {
		if item.ID != callID {
			continue
		}
		switch item.Type {
		case "tool_call":
			requested = true
		case "tool_call_started":
			started = true
		case "tool_result":
			completed = true
		}
	}
	return requested, started, completed
}
