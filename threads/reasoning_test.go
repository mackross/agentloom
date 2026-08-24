package threads

import (
	"reflect"
	"testing"
)

func TestSnapshotRoundTripPreservesReasoningItem(t *testing.T) {
	thread := newThread()
	thread.QueueItem(ReasoningItem{
		Provider:   "deepseek.responses",
		ID:         "reasoning-1",
		Visibility: ReasoningVisibilitySummary,
		Text:       "private chain",
		Summary:    "brief rationale",
		Opaque:     []byte{0, 1, 2, 255},
	})

	snapshot, err := thread.Snapshot()
	if err != nil {
		t.Fatalf("snapshot thread: %v", err)
	}
	restored, err := RestoreThreadSnapshot(snapshot)
	if err != nil {
		t.Fatalf("restore snapshot: %v", err)
	}

	if got, want := snapshotThread(restored), snapshotThread(thread); !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestDefaultRequestBuilderProjectsReasoningForProvider(t *testing.T) {
	items := []Item{
		UserText("question"),
		ReasoningItem{Provider: "deepseek.responses", ID: "d1"},
		AssistantText("calling tool"),
		ReasoningItem{Provider: "openai.responses", ID: "o1"},
		ToolCall{CallID: "c1", Name: "lookup", Payload: `{}`},
	}

	withoutStrategy := DefaultRequestBuilder.Build(items, StreamerCapabilities{})
	if want := []Item{UserText("question"), AssistantText("calling tool"), ToolCall{CallID: "c1", Name: "lookup", Payload: `{}`}}; !reflect.DeepEqual(withoutStrategy.Items, want) {
		t.Fatalf("default projection\n got: %#v\nwant: %#v", withoutStrategy.Items, want)
	}

	withProvider := DefaultRequestBuilder.Build(items, StreamerCapabilities{
		Reasoning: ReasoningForProvider("deepseek.responses"),
	})
	want := []Item{
		UserText("question"),
		ReasoningItem{Provider: "deepseek.responses", ID: "d1"},
		AssistantText("calling tool"),
		ToolCall{CallID: "c1", Name: "lookup", Payload: `{}`},
	}
	if !reflect.DeepEqual(withProvider.Items, want) {
		t.Fatalf("provider projection\n got: %#v\nwant: %#v", withProvider.Items, want)
	}
}

func TestDefaultRequestBuilderProjectsCurrentTurnReasoningForProvider(t *testing.T) {
	items := []Item{
		UserText("earlier"),
		ReasoningItem{Provider: "deepseek.responses", ID: "historical-tool"},
		ToolCall{CallID: "old-call", Name: "lookup", Payload: `{}`},
		ToolCallResult{CallID: "old-call", Output: "old result"},
		ReasoningItem{Provider: "deepseek.responses", ID: "completed-no-tool"},
		AssistantText("earlier answer"),
		UserText("latest"),
		ReasoningItem{Provider: "openai.responses", ID: "foreign"},
		ReasoningItem{Provider: "deepseek.responses", ID: "current"},
		ToolCall{CallID: "c1", Name: "lookup", Payload: `{}`},
		ToolCallResult{CallID: "c1", Output: "found"},
		ReasoningItem{Provider: "deepseek.responses", ID: "current-no-tool"},
		AssistantText("current answer"),
	}

	got := DefaultRequestBuilder.Build(items, StreamerCapabilities{
		Reasoning: ReasoningForCurrentTurn("deepseek.responses"),
	}).Items
	want := []Item{
		UserText("earlier"),
		ToolCall{CallID: "old-call", Name: "lookup", Payload: `{}`},
		ToolCallResult{CallID: "old-call", Output: "old result"},
		AssistantText("earlier answer"),
		UserText("latest"),
		ReasoningItem{Provider: "deepseek.responses", ID: "current"},
		ToolCall{CallID: "c1", Name: "lookup", Payload: `{}`},
		ToolCallResult{CallID: "c1", Output: "found"},
		ReasoningItem{Provider: "deepseek.responses", ID: "current-no-tool"},
		AssistantText("current answer"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("current-turn projection\n got: %#v\nwant: %#v", got, want)
	}
}
