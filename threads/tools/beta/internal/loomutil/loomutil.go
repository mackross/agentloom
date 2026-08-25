package loomutil

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mackross/agentloom/threads"
)

// Snapshot builds a one-tool durable snapshot with matching handler load data.
func Snapshot(name, desc string, payload threads.ToolPayload, load any) threads.ToolsSnapshot {
	data, err := json.Marshal(load)
	if err != nil {
		panic(err)
	}
	return threads.ToolsSnapshot{
		Snapshot: threads.ToolOfferSnapshot{
			Offered: []threads.ToolSpec{{
				Name:        name,
				Description: desc,
				Payload:     payload,
			}},
		},
		Handlers: []threads.ToolHandlerBinding{{
			Name:            name,
			HandlerLoadData: data,
		}},
	}
}

// DecodeLoad unmarshals handler load data into v.
func DecodeLoad(load json.RawMessage, v any) error {
	return json.Unmarshal(load, v)
}

// DecodePayload unmarshals a tool call payload into v.
func DecodePayload(call threads.ToolCall, v any) error {
	return call.UnmarshalJSON(v)
}

// Result returns a successful tool call result.
func Result(call threads.ToolCall, output string, data map[string]any) threads.ToolCallResult {
	return threads.ToolCallResult{CallID: call.CallID, Output: output, Data: data}
}

// Error returns a structured tool call error result.
func Error(call threads.ToolCall, err error) threads.ToolCallResult {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	return Result(call, "error: "+msg, map[string]any{"error": msg})
}

// AsyncResult starts worker in a goroutine and returns its item through the
// thread's event loop when complete.
func AsyncResult(ctx context.Context, thread threads.Thread, call threads.ToolCall, recovery threads.ToolRecovery, worker func(context.Context) threads.ToolCallResult) (threads.ToolDispatch, error) {
	if thread == nil {
		return threads.ToolDispatch{}, fmt.Errorf("async %s requires a thread with an event loop", call.Name)
	}
	go func() {
		item := worker(ctx)
		if item.CallID == "" {
			item.CallID = call.CallID
		}
		_ = thread.ReturnAsyncToolItem(context.Background(), call.CallID, item)
	}()
	return threads.ToolDispatch{Started: true, Recovery: recovery}, nil
}
