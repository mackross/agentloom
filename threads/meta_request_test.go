package threads

import (
	"reflect"
	"strings"
	"testing"
)

func TestRequestBuilderAttachesItemMetaToPreviousEmittedItem(t *testing.T) {
	req := DefaultRequestBuilder.Build([]Item{
		UserText("a"),
		PatchItemMetadata{Metadata: map[string]any{"cache/id": "a"}},
		UserText("b"),
		PatchItemMetadata{Metadata: map[string]any{"cache/id": "b", "cache/openai/prompt_cache_key": "k"}},
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
		PatchItemMetadata{Metadata: map[string]any{"cache/id": "same"}},
		UserText("b"),
		PatchItemMetadata{Metadata: map[string]any{"cache/id": "same"}},
		UserText("c"),
		PatchItemMetadata{Metadata: map[string]any{"cache/id": "other"}},
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

func TestRequestBuilderPanicsOnNonzeroTargetInRawProjection(t *testing.T) {
	assertPanics(t, func() {
		DefaultRequestBuilder.Build([]Item{
			UserText("a"),
			PatchItemMetadata{Target: 5, Metadata: map[string]any{"cache/id": "a"}},
		}, StreamerCapabilities{})
	}, "nonzero patch target in raw projection")
}

func TestRequestBuilderValidatesTargetBeforeRollbackProjection(t *testing.T) {
	// Rollback projection removes this tool call/result and its adjacent
	// metadata. Validation must happen first so the unsupported target cannot be
	// silently discarded with the projected item.
	assertPanics(t, func() {
		DefaultRequestBuilder.Build([]Item{
			UserText("hello"),
			ToolCall{CallID: "c1", Name: "calc", Payload: "bad"},
			ToolCallResult{
				CallID:       "c1",
				Output:       "invalid JSON",
				SafeRollback: &ToolCallSafeRollback{SteeringHint: "retry"},
			},
			PatchItemMetadata{Target: 5, Metadata: map[string]any{"cache/id": "a"}},
		}, StreamerCapabilities{AssistantPrefix: true})
	}, "nonzero patch target removed by rollback projection")
}

func TestRequestBuilderPanicsOnStandaloneNonzeroTargetPatch(t *testing.T) {
	// A nonzero target cannot be resolved from a raw []Item projection, so it
	// must panic regardless of position: first, after non-emitting items, or
	// following non-emitting control items.
	buildWith := func(rest ...Item) []Item {
		return append(append([]Item{}, rest...), PatchItemMetadata{Target: 5, Metadata: map[string]any{"cache/id": "a"}})
	}
	cases := [][]Item{
		buildWith(PatchItemMetadata{Target: 5, Metadata: map[string]any{"cache/id": "a"}}),
		buildWith(AssistantInstruction("be concise")),
		buildWith(ToolsSnapshot{}),
		buildWith(ReasoningItem{Provider: "x", Text: "think"}),
	}
	for _, items := range cases {
		assertPanics(t, func() {
			DefaultRequestBuilder.Build(items, StreamerCapabilities{})
		}, "standalone nonzero patch target")
	}
}

func TestRequestBuilderAppliesDeleteValue(t *testing.T) {
	req := DefaultRequestBuilder.Build([]Item{
		UserText("a"),
		PatchItemMetadata{Metadata: map[string]any{"keep": "yes", "remove": "old"}},
		PatchItemMetadata{Metadata: map[string]any{"remove": DeleteValue}},
	}, StreamerCapabilities{})
	if len(req.ItemMeta) != 1 || !reflect.DeepEqual(req.ItemMeta[0], map[string]any{"keep": "yes"}) {
		t.Fatalf("request metadata = %#v", req.ItemMeta)
	}
}

func TestRequestBuilderAcceptsStandaloneZeroTargetPatch(t *testing.T) {
	req := DefaultRequestBuilder.Build([]Item{
		UserText("a"),
		PatchItemMetadata{Metadata: map[string]any{"cache/id": "a"}},
		PatchItemMetadata{Metadata: map[string]any{"cache/id": "b"}},
	}, StreamerCapabilities{})
	if len(req.Items) != 1 || len(req.ItemMeta) != 1 {
		t.Fatalf("req = items %#v meta %#v", req.Items, req.ItemMeta)
	}
	if got := req.ItemMeta[0]["cache/id"]; got != "b" {
		t.Fatalf("merged metadata = %#v, want cache/id=b", req.ItemMeta[0])
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
		PatchItemMetadata{Metadata: map[string]any{"cache/id": "failed-result"}},
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
	thread.QueueItem(PatchItemMetadata{Metadata: map[string]any{"cache/id": "a"}})
	thread.QueueItem(UserText("b"))
	items := thread.items.Slice()
	if len(items) != 2 {
		t.Fatalf("thread items len = %d, want 2 (metadata patch must not add a node)", len(items))
	}
	if items[0] != UserText("a") || items[1] != UserText("b") {
		t.Fatalf("items = %#v, want [a b] uncoalesced", items)
	}
	head := thread.items.Head()
	if head == nil || head.Metadata["cache/id"] != "a" {
		t.Fatalf("patched node metadata = %#v, want cache/id=a", head.Metadata)
	}
	if head.Next == nil || len(head.Next.Metadata) != 0 {
		t.Fatalf("second node metadata = %#v, want empty", head.Next.Metadata)
	}
	if head.Seq == 0 || head.Seq == head.Next.Seq {
		t.Fatalf("node seqs = %d, %d; want distinct nonzero", head.Seq, head.Next.Seq)
	}
}

func TestTextCoalescingRetainsFirstSeq(t *testing.T) {
	thread := newThread()
	thread.QueueItem(UserText("hello"))
	thread.QueueItem(UserText("world"))
	head := thread.items.Head()
	if head == nil || head.Seq != 1 {
		t.Fatalf("coalesced node seq = %v, want 1 (first event)", head.Seq)
	}
	if got := thread.items.Slice(); len(got) != 1 || got[0] != UserText("helloworld") {
		t.Fatalf("coalesced items = %#v", got)
	}
}

func TestPatchTargetZeroAppliesToCurrentTail(t *testing.T) {
	thread := newThread()
	thread.QueueItem(UserText("first"))
	thread.QueueItem(UserText("second"))
	tail := thread.items.Tail()
	if tail == nil {
		t.Fatal("no tail")
	}
	thread.QueueItem(PatchItemMetadata{Metadata: map[string]any{"cache/id": "tail"}})
	if got := tail.Metadata["cache/id"]; got != "tail" {
		t.Fatalf("zero-target patch did not apply to tail: %#v", tail.Metadata)
	}
	if len(thread.items.Slice()) != 1 {
		t.Fatalf("zero-target patch added a node: %#v", thread.items.Slice())
	}
	// Later text must not coalesce into the annotated tail item.
	thread.QueueItem(UserText(" later"))
	if got := thread.items.Slice(); len(got) != 2 || got[1] != UserText(" later") {
		t.Fatalf("annotated tail absorbed later text: %#v", got)
	}
}

func TestExplicitPatchUpdatesOlderItemAndNextRequestReflectsIt(t *testing.T) {
	thread := newThread()
	thread.QueueItem(UserText("old"))
	first := thread.items.Head()
	thread.QueueItem(UserText("newer"))
	thread.QueueItem(PatchItemMetadata{Target: first.Seq, Metadata: map[string]any{"cache/id": "patched"}})
	if got := first.Metadata["cache/id"]; got != "patched" {
		t.Fatalf("explicit patch did not update older item: %#v", first.Metadata)
	}
	req := thread.requestSnapshot()
	if len(req.Items) != 1 || len(req.ItemMeta) != 1 {
		t.Fatalf("request items = %#v meta = %#v", req.Items, req.ItemMeta)
	}
	if got := req.ItemMeta[0]["cache/id"]; got != "patched" {
		t.Fatalf("request meta cache/id = %#v, want patched", got)
	}
}

func TestPatchingAbsentTargetFailsBeforeMutation(t *testing.T) {
	thread := newThread()
	thread.QueueItem(UserText("a"))
	seqBefore := thread.mutationSeq
	assertPanics(t, func() {
		thread.QueueItem(PatchItemMetadata{Target: 999, Metadata: map[string]any{"k": "v"}})
	}, "patch absent target")
	if thread.mutationSeq != seqBefore {
		t.Fatalf("mutation seq advanced on failed patch: %d -> %d", seqBefore, thread.mutationSeq)
	}
}

func TestMetadataDeepCopyPreventsCallerMutation(t *testing.T) {
	thread := newThread()
	meta := map[string]any{"cache/id": "session", "nested": map[string]any{"flag": true}}
	thread.QueueItem(UserText("a"), PatchItemMetadata{Metadata: meta})
	nested, _ := meta["nested"].(map[string]any)
	nested["flag"] = false
	head := thread.items.Head()
	got, _ := head.Metadata["nested"].(map[string]any)
	if got["flag"] != true {
		t.Fatalf("queued nested metadata was mutated by caller: %#v", head.Metadata)
	}
	snap, err := thread.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(snap.Items) != 1 || snap.Items[0].Metadata == "" || !strings.Contains(snap.Items[0].Metadata, `true`) {
		t.Fatalf("snapshot metadata = %q", snap.Items[0].Metadata)
	}
}

func TestCanonicalItemsContainNoPatchItemMetadataNodes(t *testing.T) {
	thread := newThread()
	thread.QueueItem(UserText("a"), PatchItemMetadata{Metadata: map[string]any{"k": "v"}})
	thread.QueueItem(PatchItemMetadata{Metadata: map[string]any{"later": "x"}})
	for _, item := range thread.items.Slice() {
		if _, ok := item.(PatchItemMetadata); ok {
			t.Fatalf("canonical list contains PatchItemMetadata node: %#v", item)
		}
	}
}

func TestInitialMetadataProducesOneWALEvent(t *testing.T) {
	thread := newThread()
	thread.QueueItem(UserText("a"), PatchItemMetadata{Metadata: map[string]any{"k": "v"}})
	events := thread.WALAfter(0)
	if len(events) != 1 {
		t.Fatalf("wal events = %d, want 1", len(events))
	}
	if events[0].Op != walOpQueueItem {
		t.Fatalf("wal op = %q, want queue_item", events[0].Op)
	}
	if events[0].Item.Metadata == "" {
		t.Fatal("wal item metadata not persisted")
	}
	restored := newThread()
	if err := restored.ReplayWAL(events); err != nil {
		t.Fatalf("replay wal: %v", err)
	}
	if restored.items.Head() == nil || restored.items.Head().Metadata["k"] != "v" {
		t.Fatalf("restored metadata = %#v", restored.items.Head().Metadata)
	}
}
