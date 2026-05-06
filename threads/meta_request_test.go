package threads

import "testing"

func TestRequestBuilderAttachesItemMetaToPreviousEmittedItem(t *testing.T) {
	req := DefaultRequestBuilder.Build([]Item{
		UserText("a"),
		PreviousItemMetadata{"cache/id": "a"},
		UserText("b"),
		PreviousItemMetadata{"cache/id": "b", "cache/openai/prompt_cache_key": "k"},
	})
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
	})
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
