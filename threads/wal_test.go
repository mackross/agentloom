package threads

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	gschema "github.com/google/jsonschema-go/jsonschema"
)

func TestRestoreFromCheckpointAndWALPreservesCoalescedUserItems(t *testing.T) {
	thread := New()
	base, err := thread.Checkpoint(CheckpointOptions{Policy: InflightSkip})
	if err != nil {
		t.Fatalf("base checkpoint: %v", err)
	}

	thread.QueueItem(UserText("hello"))
	thread.QueueItem(UserText("world"))

	wal := thread.WALAfter(base.Seq)
	restored, err := RestoreFromCheckpointAndWAL(base, wal, RestoreOptions{})
	if err != nil {
		t.Fatalf("restore from checkpoint + wal: %v", err)
	}

	if got := restored.items.Slice(); len(got) != 1 || got[0] != UserText("helloworld") {
		t.Fatalf("expected one coalesced item, got %#v", got)
	}

	before := snapshotThread(thread)
	after := snapshotThread(restored)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("snapshot mismatch\nbefore=%#v\nafter=%#v", before, after)
	}
}

func TestRestoreFromCheckpointAndWALReplaysStreamLifecycle(t *testing.T) {
	thread := New()
	base, err := thread.Checkpoint(CheckpointOptions{Policy: InflightSkip})
	if err != nil {
		t.Fatalf("base checkpoint: %v", err)
	}

	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
		b.Emit(AssistantText("w1"))
		b.Emit(AssistantText("w2"))
	})
	thread.SetExecutor(NewThreadExecutor(streamer.Streamer()))
	thread.QueueItem(UserText("hello"))
	thread.QueueItem(SendItem{})

	wal := thread.WALAfter(base.Seq)
	restored, err := RestoreFromCheckpointAndWAL(base, wal, RestoreOptions{})
	if err != nil {
		t.Fatalf("restore from checkpoint + wal: %v", err)
	}

	before := snapshotThread(thread)
	after := snapshotThread(restored)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("snapshot mismatch\nbefore=%#v\nafter=%#v", before, after)
	}
}

func TestReplayWALRejectsNonMonotonicSequence(t *testing.T) {
	thread := New()
	if err := thread.ReplayWAL([]WALEvent{{Seq: 2, Op: walOpQueueItem, Item: SnapshotItem{Type: "user_text", Text: "a"}}, {Seq: 1, Op: walOpQueueItem, Item: SnapshotItem{Type: "user_text", Text: "b"}}}); err == nil {
		t.Fatal("expected non-monotonic replay error")
	}
}

func TestRestoreFromCheckpointAndWALTrimsUnsafeTailByDefault(t *testing.T) {
	thread := New()
	base, err := thread.Checkpoint(CheckpointOptions{Policy: InflightSkip})
	if err != nil {
		t.Fatalf("base checkpoint: %v", err)
	}

	thread.QueueItem(UserText("hello"))
	thread.QueueItem(SendItem{})

	wal := thread.WALAfter(base.Seq)
	if len(wal) != 2 {
		t.Fatalf("expected two wal events, got %d", len(wal))
	}
	restored, err := RestoreFromCheckpointAndWAL(base, wal, RestoreOptions{})
	if err != nil {
		t.Fatalf("restore from checkpoint + wal: %v", err)
	}
	if got := restored.State(); got != StateIdle {
		t.Fatalf("expected idle after unsafe-tail trim, got %q", got)
	}
	items := restored.items.Slice()
	if len(items) != 1 || items[0] != UserText("hello") {
		t.Fatalf("unexpected restored items after trim: %#v", items)
	}
}

func TestRestoreFromCheckpointAndWALTrimsPendingStartedToolTailByDefault(t *testing.T) {
	thread := New()
	thread.SetToolProvider(staticToolProvider{snap: testToolsSnapshot("edit", "edit files")})
	thread.SetToolResolver(toolResolverFunc(func(context.Context, *Thread, ToolCall, json.RawMessage) (ToolDispatch, error) {
		return ToolDispatch{
			Started:  true,
			Recovery: ToolRecoveryUnsafe,
			Items:    []Item{ToolCallResult{CallID: "c1", Output: "edited"}},
		}, nil
	}))

	base, err := thread.Checkpoint(CheckpointOptions{Policy: InflightSkip})
	if err != nil {
		t.Fatalf("base checkpoint: %v", err)
	}

	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
		b.Emit(ToolCall{CallID: "c1", Name: "edit", Payload: `{}`})
	})
	thread.SetExecutor(NewThreadExecutor(streamer.Streamer()))
	thread.QueueItem(UserText("hello"))
	thread.QueueItem(SendItem{})
	fullWAL := thread.WALAfter(base.Seq)

	var prefix []WALEvent
	for _, ev := range fullWAL {
		prefix = append(prefix, ev)
		if ev.Item.Type == "tool_call_started" {
			break
		}
	}
	if len(prefix) == len(fullWAL) {
		t.Fatalf("expected a WAL prefix ending at tool_call_started")
	}

	restored, err := RestoreFromCheckpointAndWAL(base, prefix, RestoreOptions{})
	if err != nil {
		t.Fatalf("restore from checkpoint + wal: %v", err)
	}
	if got := restored.State(); got != StateIdle {
		t.Fatalf("expected idle after trim, got %q", got)
	}
	if got := toolLifecycleTypes(restored, "c1"); !reflect.DeepEqual(got, []string{"tool_call"}) {
		t.Fatalf("unexpected restored tool lifecycle after trim: %#v", got)
	}
}

func TestRestoreFromCheckpointAndWALAllowsCompletedStartedTool(t *testing.T) {
	thread := New()
	thread.SetToolProvider(staticToolProvider{snap: testToolsSnapshot("edit", "edit files")})
	thread.SetToolResolver(toolResolverFunc(func(context.Context, *Thread, ToolCall, json.RawMessage) (ToolDispatch, error) {
		return ToolDispatch{
			Started:  true,
			Recovery: ToolRecoveryUnsafe,
			Items:    []Item{ToolCallResult{CallID: "c1", Output: "edited"}},
		}, nil
	}))

	base, err := thread.Checkpoint(CheckpointOptions{Policy: InflightSkip})
	if err != nil {
		t.Fatalf("base checkpoint: %v", err)
	}

	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
		b.Emit(ToolCall{CallID: "c1", Name: "edit", Payload: `{}`})
	})
	thread.SetExecutor(NewThreadExecutor(streamer.Streamer()))
	thread.QueueItem(UserText("hello"))
	thread.QueueItem(SendItem{})

	restored, err := RestoreFromCheckpointAndWAL(base, thread.WALAfter(base.Seq), RestoreOptions{})
	if err != nil {
		t.Fatalf("restore from checkpoint + wal: %v", err)
	}
	before := snapshotThread(thread)
	after := snapshotThread(restored)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("snapshot mismatch\nbefore=%#v\nafter=%#v", before, after)
	}
}

func TestRestoreFromCheckpointAndWALAllowsUnsafeStartedToolWhenRequested(t *testing.T) {
	base := Checkpoint{Seq: 0, Snapshot: ThreadSnapshot{Version: serializedThreadVersion, State: StateIdle, IPIndex: -1, QueueStartIndex: -1, StreamInsIndex: -1}}
	wal := []WALEvent{
		{Seq: 1, Op: walOpQueueItem, Item: SnapshotItem{Type: "tool_call", ID: "c1", Name: "edit", Args: `{}`}},
		{Seq: 2, Op: walOpQueueItem, Item: SnapshotItem{Type: "tool_call_resolving", ID: "c1"}},
		{Seq: 3, Op: walOpQueueItem, Item: SnapshotItem{Type: "tool_call_started", ID: "c1", Recovery: string(ToolRecoveryUnsafe)}},
	}

	restored, err := RestoreFromCheckpointAndWAL(base, wal, RestoreOptions{AllowUnsafe: true})
	if err != nil {
		t.Fatalf("restore from checkpoint + wal: %v", err)
	}
	if got := toolLifecycleTypes(restored, "c1"); !reflect.DeepEqual(got, []string{"tool_call", "tool_call_resolving", "tool_call_started"}) {
		t.Fatalf("unexpected restored tool lifecycle: %#v", got)
	}
}

func TestRestoreFromCheckpointAndWALReplaysLateAutoSend(t *testing.T) {
	thread := New()
	thread.SetToolProvider(staticToolProvider{snap: testToolsSnapshot("calc", "calculate")})
	thread.SetToolResolver(toolResolverFunc(func(context.Context, *Thread, ToolCall, json.RawMessage) (ToolDispatch, error) {
		return ToolDispatch{
			Started:  true,
			Recovery: ToolRecoveryUnsafe,
			Items:    []Item{ToolCallResult{CallID: "c1", Output: "edited"}},
		}, nil
	}))

	base, err := thread.Checkpoint(CheckpointOptions{Policy: InflightSkip})
	if err != nil {
		t.Fatalf("base checkpoint: %v", err)
	}

	streamer := newFakeStreamer().
		Reply(func(b *streamBuilder) {
			b.Emit(ToolCall{CallID: "c1", Name: "calc", Payload: `{"a":1}`})
		}).
		Reply(func(b *streamBuilder) {})
	thread.SetExecutor(NewThreadExecutor(streamer.Streamer()))
	thread.QueueItem(UserText("hello"))
	thread.QueueItem(SendItem{})

	wal := thread.WALAfter(base.Seq)
	restored, err := RestoreFromCheckpointAndWAL(base, wal, RestoreOptions{})
	if err != nil {
		t.Fatalf("restore from checkpoint + wal: %v", err)
	}

	before := snapshotThread(thread)
	after := snapshotThread(restored)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("snapshot mismatch\nbefore=%#v\nafter=%#v", before, after)
	}
}

func TestRestoreFromCheckpointAndWALReplaysToolCallChunkFinalize(t *testing.T) {
	thread := New()
	base, err := thread.Checkpoint(CheckpointOptions{Policy: InflightSkip})
	if err != nil {
		t.Fatalf("base checkpoint: %v", err)
	}

	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
		b.Emit(ToolCallChunk{CallID: "c1", Name: "calc", PayloadDelta: `{"a":`})
		b.Emit(ToolCallChunk{CallID: "c1", PayloadDelta: `1}`})
		b.Emit(ToolCall{CallID: "c1", Name: "calc", Payload: `{"a":1}`})
	})
	thread.SetExecutor(NewThreadExecutor(streamer.Streamer()))
	thread.QueueItem(UserText("hello"))
	thread.QueueItem(SendItem{})

	wal := thread.WALAfter(base.Seq)
	restored, err := RestoreFromCheckpointAndWAL(base, wal, RestoreOptions{})
	if err != nil {
		t.Fatalf("restore from checkpoint + wal: %v", err)
	}

	before := snapshotThread(thread)
	after := snapshotThread(restored)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("snapshot mismatch\nbefore=%#v\nafter=%#v", before, after)
	}
}

func TestRestoreFromCheckpointAndWALReplaysToolSnapshotControlItem(t *testing.T) {
	thread := New()
	base, err := thread.Checkpoint(CheckpointOptions{Policy: InflightSkip})
	if err != nil {
		t.Fatalf("base checkpoint: %v", err)
	}

	thread.QueueItem(testToolsSnapshot("calc", "calculate"))
	thread.QueueItem(UserText("hello"))

	wal := thread.WALAfter(base.Seq)
	restored, err := RestoreFromCheckpointAndWAL(base, wal, RestoreOptions{})
	if err != nil {
		t.Fatalf("restore from checkpoint + wal: %v", err)
	}

	before := snapshotThread(thread)
	after := snapshotThread(restored)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("snapshot mismatch\nbefore=%#v\nafter=%#v", before, after)
	}
}

func TestRestoreFromCheckpointAndWALReplaysAwaitingToolResults(t *testing.T) {
	thread := New()
	base, err := thread.Checkpoint(CheckpointOptions{Policy: InflightSkip})
	if err != nil {
		t.Fatalf("base checkpoint: %v", err)
	}
	streamer := newFakeStreamer()
	streamer.capabilities.ToolResultSendPolicy = ToolResultSendRequiresComplete
	streamer.Reply(func(b *streamBuilder) {
		b.Emit(ToolCall{CallID: "c1", Name: "calc", Payload: `{"a":1}`})
	})
	thread.SetExecutor(NewThreadExecutor(streamer.Streamer()))
	thread.QueueItem(UserText("hello"))
	thread.QueueItem(SendItem{})

	restored, err := RestoreFromCheckpointAndWAL(base, thread.WALAfter(base.Seq), RestoreOptions{})
	if err != nil {
		t.Fatalf("restore from checkpoint + wal: %v", err)
	}
	if got := restored.State(); got != StateAwaitingToolResults {
		t.Fatalf("restored state = %q, want %q", got, StateAwaitingToolResults)
	}
}

func TestRestoreFromCheckpointAndWALReplaysToolsSnapshotHandlerLoadData(t *testing.T) {
	thread := New()
	base, err := thread.Checkpoint(CheckpointOptions{Policy: InflightSkip})
	if err != nil {
		t.Fatalf("base checkpoint: %v", err)
	}

	want := ToolsSnapshot{
		Snapshot: ToolOfferSnapshot{Offered: []ToolSpec{{
			Name:        "write_file",
			Description: "write contents",
			Payload:     ToolPayloadJSONSchema(gschema.Schema{Type: "object"}),
		}}},
		Handlers: []ToolHandlerBinding{{
			Name:            "write_file",
			HandlerLoadData: []byte(`{"function":"tool/write-file@v1","filename":"notes.txt"}`),
		}},
	}
	thread.QueueItem(want)

	wal := thread.WALAfter(base.Seq)
	restored, err := RestoreFromCheckpointAndWAL(base, wal, RestoreOptions{})
	if err != nil {
		t.Fatalf("restore from checkpoint + wal: %v", err)
	}

	items := restored.items.Slice()
	if len(items) != 1 {
		t.Fatalf("expected one restored item, got %#v", items)
	}
	got, ok := items[0].(ToolsSnapshot)
	if !ok {
		t.Fatalf("expected ToolsSnapshot, got %T", items[0])
	}
	if !reflect.DeepEqual(got.Handlers, want.Handlers) {
		t.Fatalf("unexpected restored handler load data\nwant=%#v\ngot=%#v", want.Handlers, got.Handlers)
	}
}
