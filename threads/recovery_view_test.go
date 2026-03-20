package threads

import (
	"reflect"
	"testing"
)

func TestOutstandingToolCallsExposeRecoveryFacingState(t *testing.T) {
	thread := newTestThread(t)
	thread.QueueItem(ToolsSnapshot{Handlers: []ToolHandlerBinding{
		{Name: "calc", HandlerLoadData: []byte(`{"snapshot":"old"}`)},
	}})
	thread.QueueItem(ToolCall{CallID: "c1", Name: "missing", Payload: `{"a":1}`})
	thread.QueueItem(ToolCall{CallID: "c2", Name: "calc", Payload: `{"a":2}`})
	thread.QueueItem(ToolCallStarted{CallID: "c2", Continue: ToolContinueManual})
	thread.QueueItem(ToolCall{CallID: "c3", Name: "calc", Payload: `{"a":3}`})
	thread.QueueItem(ToolCallResult{CallID: "c3", Output: "3"})

	got := thread.OutstandingToolCalls()
	want := []OutstandingToolCall{
		{
			Call:  ToolCall{CallID: "c1", Name: "missing", Payload: `{"a":1}`},
			State: OutstandingToolCallRequested,
		},
		{
			Call:            ToolCall{CallID: "c2", Name: "calc", Payload: `{"a":2}`},
			State:           OutstandingToolCallStarted,
			Bound:           true,
			HandlerLoadData: []byte(`{"snapshot":"old"}`),
			Continue:        ToolContinueManual,
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected outstanding tool calls:\n got %#v\nwant %#v", got, want)
	}
}

func TestOutstandingToolCallsUseNearestSnapshotAndCloneLoadData(t *testing.T) {
	thread := newTestThread(t)
	thread.QueueItem(ToolsSnapshot{Handlers: []ToolHandlerBinding{
		{Name: "calc", HandlerLoadData: []byte(`{"snapshot":"old"}`)},
	}})
	thread.QueueItem(ToolCall{CallID: "c1", Name: "calc", Payload: `{"a":1}`})
	thread.QueueItem(ToolsSnapshot{Handlers: []ToolHandlerBinding{
		{Name: "calc", HandlerLoadData: []byte(`{"snapshot":"new"}`)},
	}})
	thread.QueueItem(ToolCall{CallID: "c2", Name: "calc", Payload: `{"a":2}`})

	got := thread.OutstandingToolCalls()
	if len(got) != 2 {
		t.Fatalf("expected two outstanding tool calls, got %#v", got)
	}
	if string(got[0].HandlerLoadData) != `{"snapshot":"old"}` {
		t.Fatalf("unexpected old snapshot binding: %#v", got[0])
	}
	if string(got[1].HandlerLoadData) != `{"snapshot":"new"}` {
		t.Fatalf("unexpected new snapshot binding: %#v", got[1])
	}

	got[0].HandlerLoadData[0] = 'X'
	again := thread.OutstandingToolCalls()
	if string(again[0].HandlerLoadData) != `{"snapshot":"old"}` {
		t.Fatalf("expected cloned handler load data, got %#v", again[0].HandlerLoadData)
	}
}
