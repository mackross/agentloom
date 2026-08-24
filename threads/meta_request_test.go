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

func TestRequestBuilderProjectsRepeatedToolFailuresWithLatestHint(t *testing.T) {
	req := DefaultRequestBuilder.Build([]Item{
		UserText("hello"),
		ToolCall{CallID: "c1", Name: "calc", Payload: "bad"},
		ToolCallResult{CallID: "c1", Output: "invalid JSON", SafeRollback: &ToolCallSafeRollback{SteeringHint: "retry"}},
		ToolCall{CallID: "c2", Name: "calc", Payload: "still bad"},
		ToolCallResult{CallID: "c2", Output: "still invalid JSON", SafeRollback: &ToolCallSafeRollback{SteeringHint: "retry again"}},
	}, StreamerCapabilities{AssistantPrefix: true})
	want := []Item{UserText("helloretry again")}
	if !reflect.DeepEqual(req.Items, want) {
		t.Fatalf("repeated failures were not replaced by the latest exact hint:\n got: %#v\nwant: %#v", req.Items, want)
	}
}

func TestRequestBuilderDoesNotProjectFailureAcrossNewerUserMessage(t *testing.T) {
	base := []Item{
		UserText("original request"),
		ToolCall{CallID: "c1", Name: "calc", Payload: "bad"},
		UserText("never mind; do something else"),
		ToolCallResult{CallID: "c1", Output: "invalid JSON", SafeRollback: &ToolCallSafeRollback{
			SteeringHint: "retry calc",
			RetryAttempt: 1,
			MaxRetries:   2,
		}},
	}

	req := DefaultRequestBuilder.Build(base, StreamerCapabilities{AssistantPrefix: true})
	if !reflect.DeepEqual(req.Items, base) {
		t.Fatalf("failure crossed a newer user-message boundary:\n got: %#v\nwant: %#v", req.Items, base)
	}
}

func TestRequestBuilderDoesNotProjectIncompleteParallelBatch(t *testing.T) {
	failure := ToolCallResult{
		CallID: "c1",
		Output: "invalid JSON",
		SafeRollback: &ToolCallSafeRollback{
			SteeringHint: "retry with valid JSON",
			RetryAttempt: 1,
			MaxRetries:   2,
		},
	}
	base := []Item{
		UserText("hello"),
		ToolCall{CallID: "c1", Name: "calc", Payload: "bad"},
		ToolCall{CallID: "c2", Name: "background", Payload: "slow"},
		failure,
	}

	req := DefaultRequestBuilder.Build(base, StreamerCapabilities{AssistantPrefix: true})
	if !reflect.DeepEqual(req.Items, base) {
		t.Fatalf("incomplete parallel transcript was projected:\n got: %#v\nwant: %#v", req.Items, base)
	}
}

func TestRequestBuilderRollsBackParallelFailureOnlyAfterSiblingResult(t *testing.T) {
	hint := "\nretry calc with valid JSON"
	base := []Item{
		UserText("hello"),
		ToolCall{CallID: "c1", Name: "calc", Payload: "bad"},
		ToolCall{CallID: "c2", Name: "background", Payload: "slow"},
		ToolCallResult{CallID: "c1", Output: "invalid JSON", SafeRollback: &ToolCallSafeRollback{
			SteeringHint: hint,
			RetryAttempt: 1,
			MaxRetries:   2,
		}},
	}
	caps := StreamerCapabilities{AssistantPrefix: true}

	pending := DefaultRequestBuilder.Build(base, caps)
	if !reflect.DeepEqual(pending.Items, base) {
		t.Fatalf("pending parallel sibling was projected:\n got: %#v\nwant: %#v", pending.Items, base)
	}

	complete := append(append([]Item(nil), base...), ToolCallResult{CallID: "c2", Output: "background success"})
	got := DefaultRequestBuilder.Build(complete, caps)
	want := []Item{
		UserText("hello"),
		ToolCall{CallID: "c2", Name: "background", Payload: "slow"},
		ToolCallResult{CallID: "c2", Output: "background success"},
		UserText(hint),
	}
	if !reflect.DeepEqual(got.Items, want) {
		t.Fatalf("completed parallel failure was not projected independently:\n got: %#v\nwant: %#v", got.Items, want)
	}
}

func TestRequestBuilderDoesNotMoveRolledBackMetadataOntoParallelSibling(t *testing.T) {
	hint := "\nretry calc with valid JSON"
	req := DefaultRequestBuilder.Build([]Item{
		UserText("hello"),
		ToolCall{CallID: "c1", Name: "calc", Payload: "bad"},
		ToolCall{CallID: "c2", Name: "background", Payload: "slow"},
		ToolCallResult{CallID: "c2", Output: "background success"},
		ToolCallResult{CallID: "c1", Output: "invalid JSON", SafeRollback: &ToolCallSafeRollback{
			SteeringHint: hint,
			RetryAttempt: 1,
			MaxRetries:   2,
		}},
		PreviousItemMetadata{"cache/id": "failed-result"},
	}, StreamerCapabilities{AssistantPrefix: true})

	wantItems := []Item{
		UserText("hello"),
		ToolCall{CallID: "c2", Name: "background", Payload: "slow"},
		ToolCallResult{CallID: "c2", Output: "background success"},
		UserText(hint),
	}
	if !reflect.DeepEqual(req.Items, wantItems) {
		t.Fatalf("unexpected projected items:\n got: %#v\nwant: %#v", req.Items, wantItems)
	}
	if got := req.ItemMeta[2]; got != nil {
		t.Fatalf("retained sibling result inherited rolled-back metadata: %#v", got)
	}
}

func TestRequestBuilderPreservesAllParallelRollbackHints(t *testing.T) {
	hint1 := "\nretry calc with valid JSON"
	hint2 := "\nretry lookup with valid JSON"
	req := DefaultRequestBuilder.Build([]Item{
		UserText("hello"),
		ToolCall{CallID: "c1", Name: "calc", Payload: "bad"},
		ToolCall{CallID: "c2", Name: "lookup", Payload: "also bad"},
		ToolCallResult{CallID: "c1", Output: "invalid calc JSON", SafeRollback: &ToolCallSafeRollback{
			SteeringHint: hint1,
			RetryAttempt: 1,
			MaxRetries:   2,
		}},
		ToolCallResult{CallID: "c2", Output: "invalid lookup JSON", SafeRollback: &ToolCallSafeRollback{
			SteeringHint: hint2,
			RetryAttempt: 1,
			MaxRetries:   2,
		}},
	}, StreamerCapabilities{AssistantPrefix: true})
	want := []Item{UserText("hello" + hint1 + hint2)}
	if !reflect.DeepEqual(req.Items, want) {
		t.Fatalf("parallel rollback hints were not preserved exactly:\n got: %#v\nwant: %#v", req.Items, want)
	}
}

func TestRequestBuilderSameNameParallelSuccessDoesNotClearSiblingFailureHint(t *testing.T) {
	hint := "\nretry one calc call with valid JSON"
	req := DefaultRequestBuilder.Build([]Item{
		UserText("hello"),
		ToolCall{CallID: "c1", Name: "calc", Payload: "bad"},
		ToolCall{CallID: "c2", Name: "calc", Payload: "good"},
		ToolCallResult{CallID: "c1", Output: "invalid JSON", SafeRollback: &ToolCallSafeRollback{
			SteeringHint: hint,
			RetryAttempt: 1,
			MaxRetries:   2,
		}},
		ToolCallResult{CallID: "c2", Output: "success"},
	}, StreamerCapabilities{AssistantPrefix: true})
	want := []Item{
		UserText("hello"),
		ToolCall{CallID: "c2", Name: "calc", Payload: "good"},
		ToolCallResult{CallID: "c2", Output: "success"},
		UserText(hint),
	}
	if !reflect.DeepEqual(req.Items, want) {
		t.Fatalf("same-name sibling success cleared the failure hint:\n got: %#v\nwant: %#v", req.Items, want)
	}
}

func TestRequestBuilderRemovesParallelRecoveryHintAfterSuccess(t *testing.T) {
	hint := "\nretry calc with valid JSON"
	req := DefaultRequestBuilder.Build([]Item{
		UserText("hello"),
		ToolCall{CallID: "c1", Name: "calc", Payload: "bad"},
		ToolCall{CallID: "c2", Name: "background", Payload: "slow"},
		ToolCallResult{CallID: "c1", Output: "invalid JSON", SafeRollback: &ToolCallSafeRollback{
			SteeringHint: hint,
			RetryAttempt: 1,
			MaxRetries:   2,
		}},
		ToolCallResult{CallID: "c2", Output: "background success"},
		ToolCall{CallID: "c3", Name: "calc", Payload: "good"},
		ToolCallResult{CallID: "c3", Output: "calc success"},
	}, StreamerCapabilities{AssistantPrefix: true})
	want := []Item{
		UserText("hello"),
		ToolCall{CallID: "c2", Name: "background", Payload: "slow"},
		ToolCallResult{CallID: "c2", Output: "background success"},
		ToolCall{CallID: "c3", Name: "calc", Payload: "good"},
		ToolCallResult{CallID: "c3", Output: "calc success"},
	}
	if !reflect.DeepEqual(req.Items, want) {
		t.Fatalf("successful parallel repair retained its failed exchange or hint:\n got: %#v\nwant: %#v", req.Items, want)
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
		ToolCall{CallID: "c2", Name: "calc", Payload: "good"},
		ToolCallResult{CallID: "c2", Output: "success"},
	}
	if !reflect.DeepEqual(req.Items, want) {
		t.Fatalf("unexpected items:\n got: %#v\nwant: %#v", req.Items, want)
	}
}

func TestRequestBuilderRemovesAllRollbackableFailuresAfterSuccessfulRetry(t *testing.T) {
	req := DefaultRequestBuilder.Build([]Item{
		UserText("hello"),
		ToolCall{CallID: "c1", Name: "calc", Payload: "bad"},
		ToolCallResult{CallID: "c1", Output: "invalid JSON", SafeRollback: &ToolCallSafeRollback{
			SteeringHint: "retry with valid JSON",
			RetryAttempt: 1,
			MaxRetries:   2,
		}},
		ToolCall{CallID: "c2", Name: "calc", Payload: "still bad"},
		ToolCallResult{CallID: "c2", Output: "still invalid JSON", SafeRollback: &ToolCallSafeRollback{
			SteeringHint: "retry again with valid JSON",
			RetryAttempt: 2,
			MaxRetries:   2,
		}},
		ToolCall{CallID: "c3", Name: "calc", Payload: "good"},
		ToolCallResult{CallID: "c3", Output: "success"},
	}, StreamerCapabilities{AssistantPrefix: true})

	want := []Item{
		UserText("hello"),
		ToolCall{CallID: "c3", Name: "calc", Payload: "good"},
		ToolCallResult{CallID: "c3", Output: "success"},
	}
	if !reflect.DeepEqual(req.Items, want) {
		t.Fatalf("successful retry retained rollbackable failures:\n got: %#v\nwant: %#v", req.Items, want)
	}
}

func TestItemMetaPreventsControlBlockCoalescing(t *testing.T) {
	thread := newThread()
	thread.QueueItem(UserText("a"))
	thread.QueueItem(PreviousItemMetadata{"cache/id": "a"})
	thread.QueueItem(UserText("b"))
	items := thread.items.Slice()
	if len(items) != 3 {
		t.Fatalf("thread items len = %d, want 3", len(items))
	}
}
