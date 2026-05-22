package threads

import (
	"reflect"
	"testing"
)

func TestRequestBuilderAttachesItemMetaToPreviousEmittedItem(t *testing.T) {
	req := DefaultRequestBuilder.Build([]Item{
		UserText("a"),
		PreviousItemMetadata{"cache/id": "a"},
		UserText("b"),
		PreviousItemMetadata{"cache/id": "b", "cache/openai/prompt_cache_key": "k"},
	}, StreamerCapabilities{})
	if len(req.Items) != 2 {
		t.Fatalf("len(req.Items) = %d, want 2", len(req.Items))
	}
	if got, ok := req.Items[0].(UserText); !ok || got != "a" {
		t.Fatalf("first item = %#v, want UserText a", req.Items[0])
	}
	if got := req.ItemMeta[0]["cache/id"]; got != "a" {
		t.Fatalf("first meta cache/id = %#v, want a", got)
	}
	if got := req.ItemMeta[1]["cache/id"]; got != "b" {
		t.Fatalf("second meta cache/id = %#v, want b", got)
	}
	if got := req.ItemMeta[1]["cache/openai/prompt_cache_key"]; got != "k" {
		t.Fatalf("second meta openai cache key = %#v, want k", got)
	}
}

func TestRequestBuilderCoalescesOnlyWhenMetadataEqual(t *testing.T) {
	req := DefaultRequestBuilder.Build([]Item{
		UserText("a"),
		PreviousItemMetadata{"cache/id": "same"},
		UserText("b"),
		PreviousItemMetadata{"cache/id": "same"},
		UserText("c"),
		PreviousItemMetadata{"cache/id": "other"},
	}, StreamerCapabilities{})
	if len(req.Items) != 2 {
		t.Fatalf("len(req.Items) = %d, want 2", len(req.Items))
	}
	if got := req.Items[0].(UserText); got != "ab" {
		t.Fatalf("first item = %q, want ab", got)
	}
	if got := req.Items[1].(UserText); got != "c" {
		t.Fatalf("second item = %q, want c", got)
	}
}

func TestRequestBuilderDoesNotRollbackRepeatedToolFailures(t *testing.T) {
	req := DefaultRequestBuilder.Build([]Item{
		UserText("hello"),
		ToolCall{CallID: "c1", Name: "calc", Payload: "bad"},
		ToolCallResult{CallID: "c1", Output: "invalid JSON", SafeRollback: &ToolCallSafeRollback{SteeringHint: "retry"}},
		ToolCall{CallID: "c2", Name: "calc", Payload: "still bad"},
		ToolCallResult{CallID: "c2", Output: "still invalid JSON", SafeRollback: &ToolCallSafeRollback{SteeringHint: "retry again"}},
	}, StreamerCapabilities{AssistantPrefix: true})
	if len(req.Items) != 5 {
		t.Fatalf("len(req.Items) = %d, want 5", len(req.Items))
	}
	if _, ok := req.Items[1].(ToolCall); !ok {
		t.Fatalf("second item = %#v, want ToolCall", req.Items[1])
	}
	if _, ok := req.Items[3].(ToolCall); !ok {
		t.Fatalf("fourth item = %#v, want ToolCall", req.Items[3])
	}
}

func TestRequestBuilderRollsBackToolFailureAfterNewUserMessage(t *testing.T) {
	req := DefaultRequestBuilder.Build([]Item{
		UserText("hello"),
		ToolCall{CallID: "c1", Name: "calc", Payload: "bad"},
		ToolCallResult{CallID: "c1", Output: "invalid JSON", SafeRollback: &ToolCallSafeRollback{SteeringHint: "retry"}},
		UserText("new request"),
		ToolCall{CallID: "c2", Name: "calc", Payload: "still bad"},
		ToolCallResult{CallID: "c2", Output: "still invalid JSON", SafeRollback: &ToolCallSafeRollback{SteeringHint: "\nretry again"}},
	}, StreamerCapabilities{AssistantPrefix: true})
	want := []Item{
		UserText("hello"),
		ToolCall{CallID: "c1", Name: "calc", Payload: "bad"},
		ToolCallResult{CallID: "c1", Output: "invalid JSON", SafeRollback: &ToolCallSafeRollback{SteeringHint: "retry"}},
		UserText("new request\nretry again"),
	}
	if !reflect.DeepEqual(req.Items, want) {
		t.Fatalf("unexpected items:\n got: %#v\nwant: %#v", req.Items, want)
	}
}

func TestRequestBuilderRollsBackToolFailureAfterSuccessfulToolCall(t *testing.T) {
	req := DefaultRequestBuilder.Build([]Item{
		UserText("hello"),
		ToolCall{CallID: "c1", Name: "calc", Payload: "bad"},
		ToolCallResult{CallID: "c1", Output: "invalid JSON", SafeRollback: &ToolCallSafeRollback{SteeringHint: "retry"}},
		ToolCall{CallID: "c2", Name: "lookup", Payload: "ok"},
		ToolCallResult{CallID: "c2", Output: "success"},
		ToolCall{CallID: "c3", Name: "calc", Payload: "still bad"},
		ToolCallResult{CallID: "c3", Output: "still invalid JSON", SafeRollback: &ToolCallSafeRollback{SteeringHint: "\nretry again"}},
	}, StreamerCapabilities{AssistantPrefix: true})
	want := []Item{
		UserText("hello"),
		ToolCall{CallID: "c1", Name: "calc", Payload: "bad"},
		ToolCallResult{CallID: "c1", Output: "invalid JSON", SafeRollback: &ToolCallSafeRollback{SteeringHint: "retry"}},
		ToolCall{CallID: "c2", Name: "lookup", Payload: "ok"},
		ToolCallResult{CallID: "c2", Output: "success"},
		UserText("\nretry again"),
	}
	if !reflect.DeepEqual(req.Items, want) {
		t.Fatalf("unexpected items:\n got: %#v\nwant: %#v", req.Items, want)
	}
}

func TestRequestBuilderRemovesSteeringHintAfterSuccessfulRetry(t *testing.T) {
	req := DefaultRequestBuilder.Build([]Item{
		UserText("hello"),
		ToolCall{CallID: "c1", Name: "calc", Payload: "bad"},
		ToolCallResult{CallID: "c1", Output: "invalid JSON", SafeRollback: &ToolCallSafeRollback{SteeringHint: "\nretry with valid JSON"}},
		ToolCall{CallID: "c2", Name: "calc", Payload: "good"},
		ToolCallResult{CallID: "c2", Output: "success"},
	}, StreamerCapabilities{AssistantPrefix: true})
	want := []Item{
		UserText("hello"),
		ToolCall{CallID: "c1", Name: "calc", Payload: "bad"},
		ToolCallResult{CallID: "c1", Output: "invalid JSON", SafeRollback: &ToolCallSafeRollback{SteeringHint: "\nretry with valid JSON"}},
		ToolCall{CallID: "c2", Name: "calc", Payload: "good"},
		ToolCallResult{CallID: "c2", Output: "success"},
	}
	if !reflect.DeepEqual(req.Items, want) {
		t.Fatalf("unexpected items:\n got: %#v\nwant: %#v", req.Items, want)
	}
}

func TestItemMetaPreventsControlBlockCoalescing(t *testing.T) {
	thread := New()
	thread.QueueItem(UserText("a"))
	thread.QueueItem(PreviousItemMetadata{"cache/id": "a"})
	thread.QueueItem(UserText("b"))
	items := thread.items.Slice()
	if len(items) != 3 {
		t.Fatalf("thread items len = %d, want 3", len(items))
	}
}
