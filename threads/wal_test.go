package threads

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"testing"

	gschema "github.com/google/jsonschema-go/jsonschema"
)

func TestRestoreFromCheckpointAndWALPreservesCoalescedUserItems(t *testing.T) {
	thread := newThread()
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

func TestRestoreFromCheckpointAndWALPreservesSyntheticToolExchange(t *testing.T) {
	thread := newThread()
	base, err := thread.Checkpoint(CheckpointOptions{Policy: InflightSkip})
	if err != nil {
		t.Fatalf("base checkpoint: %v", err)
	}

	thread.QueueSyntheticToolExchange(ToolCall{CallID: "synthetic", Name: "calc", Payload: `{}`}, ToolCallResult{Output: "ok"})
	restored, err := RestoreFromCheckpointAndWAL(base, thread.WALAfter(base.Seq), RestoreOptions{})
	if err != nil {
		t.Fatalf("restore from checkpoint + wal: %v", err)
	}
	if !reflect.DeepEqual(snapshotThread(thread), snapshotThread(restored)) {
		t.Fatalf("snapshot mismatch\nbefore=%#v\nafter=%#v", snapshotThread(thread), snapshotThread(restored))
	}
}

func TestRestoreFromCheckpointAndWALReplaysStreamLifecycle(t *testing.T) {
	thread := newThread()
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
	thread := newThread()
	if err := thread.ReplayWAL([]WALEvent{{Seq: 2, Op: walOpQueueItem, Item: SnapshotItem{Seq: 2, Type: "user_text", Text: "a"}}, {Seq: 1, Op: walOpQueueItem, Item: SnapshotItem{Seq: 1, Type: "user_text", Text: "b"}}}); err == nil {
		t.Fatal("expected non-monotonic replay error")
	}
}

func TestRestoreFromCheckpointAndWALTrimsUnsafeTailByDefault(t *testing.T) {
	thread := newThread()
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

func TestRestoreFromCheckpointAndWALDoesNotReuseTrimmedItemSeq(t *testing.T) {
	thread := newThread()
	base, err := thread.Checkpoint(CheckpointOptions{Policy: InflightSkip})
	if err != nil {
		t.Fatalf("base checkpoint: %v", err)
	}
	thread.QueueItem(UserText("hello"))
	thread.QueueItem(SendItem{})
	discardedSeq := thread.items.Tail().Seq

	restored, err := RestoreFromCheckpointAndWAL(base, thread.WALAfter(base.Seq), RestoreOptions{})
	if err != nil {
		t.Fatalf("restore with unsafe-tail trim: %v", err)
	}
	if got := restored.items.Slice(); len(got) != 1 || got[0] != UserText("hello") {
		t.Fatalf("items after trim = %#v, want only the user item", got)
	}

	// The discarded send's identity must never identify a different item after
	// recovery. Use a different role so the new item cannot coalesce with the user.
	restored.QueueItem(AssistantText("different item"))
	if got := restored.items.Tail().Seq; got <= discardedSeq {
		t.Fatalf("new item sequence = %d, want greater than discarded sequence %d", got, discardedSeq)
	}
}

func TestRestoreFromCheckpointAndWALPreservesSafeCheckpointHighWaterMark(t *testing.T) {
	thread := newThread()
	thread.QueueItem(UserText("hello"))
	base, err := thread.Checkpoint(CheckpointOptions{Policy: InflightSkip})
	if err != nil {
		t.Fatalf("base checkpoint: %v", err)
	}
	// There are no safe WAL events after the base: the entire tail is trimmed.
	thread.QueueItem(SendItem{})
	headSeq := thread.Seq()
	restored, err := RestoreFromCheckpointAndWAL(base, thread.WALAfter(base.Seq), RestoreOptions{})
	if err != nil {
		t.Fatalf("restore with unsafe-tail trim: %v", err)
	}

	// Starting another request must use the recovered safe snapshot, including
	// its high-water mark, when asked to skip the new inflight tail.
	restored.QueueItem(SendItem{})
	safe, err := restored.Checkpoint(CheckpointOptions{Policy: InflightSkip})
	if err != nil {
		t.Fatalf("recovered safe checkpoint: %v", err)
	}
	if safe.Seq != headSeq || safe.Snapshot.HeadSeq != headSeq {
		t.Fatalf("safe checkpoint sequences = %d/%d, want recovered high-water mark %d", safe.Seq, safe.Snapshot.HeadSeq, headSeq)
	}
	reopened, err := RestoreCheckpoint(safe, RestoreOptions{})
	if err != nil {
		t.Fatalf("restore recovered safe checkpoint: %v", err)
	}
	reopened.QueueItem(AssistantText("different item"))
	if got := reopened.items.Tail().Seq; got <= ItemSeq(headSeq) {
		t.Fatalf("new item sequence after checkpoint restore = %d, want greater than %d", got, headSeq)
	}
}

func TestReplayWALPreservesMetadataPatchInSafeCheckpoint(t *testing.T) {
	thread := newThread()
	base, err := thread.Checkpoint(CheckpointOptions{Policy: InflightSkip})
	if err != nil {
		t.Fatalf("base checkpoint: %v", err)
	}
	thread.QueueItem(UserText("hello"))
	thread.QueueItem(PatchItemMetadata{Metadata: map[string]any{"cache/id": "session"}})
	thread.QueueItem(SendItem{})
	live, err := thread.Checkpoint(CheckpointOptions{Policy: InflightSkip})
	if err != nil {
		t.Fatalf("live safe checkpoint: %v", err)
	}

	// Replay the whole batch, including the send that leaves the thread inflight.
	// The last safe boundary must include the preceding metadata-only mutation.
	restored, err := RestoreFromCheckpointAndWAL(base, thread.WALAfter(base.Seq), RestoreOptions{AllowUnsafe: true})
	if err != nil {
		t.Fatalf("restore checkpoint and WAL: %v", err)
	}
	replayed, err := restored.Checkpoint(CheckpointOptions{Policy: InflightSkip})
	if err != nil {
		t.Fatalf("replayed safe checkpoint: %v", err)
	}
	if got, want := replayed.Seq, live.Seq; got != want {
		t.Errorf("replayed safe checkpoint sequence = %d, want live sequence %d", got, want)
	}
	if len(replayed.Snapshot.Items) != 1 || len(live.Snapshot.Items) != 1 {
		t.Fatalf("safe checkpoint item counts: replayed = %d, live = %d; want 1 each", len(replayed.Snapshot.Items), len(live.Snapshot.Items))
	}
	if got, want := replayed.Snapshot.Items[0].Metadata, live.Snapshot.Items[0].Metadata; got != want {
		t.Errorf("replayed safe checkpoint metadata = %q, want live metadata %q", got, want)
	}
}

func TestRestoreFromCheckpointAndWALTrimsPendingStartedToolTailByDefault(t *testing.T) {
	thread := newThread()
	thread.SetToolProvider(staticToolProvider{snap: testToolsSnapshot("edit", "edit files")})
	thread.SetToolResolver(toolResolverFunc(func(context.Context, Thread, ToolCall, json.RawMessage) (ToolDispatch, error) {
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
	if got := restored.State(); got != StateAwaitingToolResults {
		t.Fatalf("expected awaiting tool results after trim, got %q", got)
	}
	if got := toolLifecycleTypes(restored, "c1"); !reflect.DeepEqual(got, []string{"tool_call"}) {
		t.Fatalf("unexpected restored tool lifecycle after trim: %#v", got)
	}
}

func TestRestoreFromCheckpointAndWALAllowsCompletedStartedTool(t *testing.T) {
	thread := newThread()
	thread.SetToolProvider(staticToolProvider{snap: testToolsSnapshot("edit", "edit files")})
	thread.SetToolResolver(toolResolverFunc(func(context.Context, Thread, ToolCall, json.RawMessage) (ToolDispatch, error) {
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
		{Seq: 1, Op: walOpQueueItem, Item: SnapshotItem{Seq: 1, Type: "tool_call", ID: "c1", Name: "edit", Args: `{}`}},
		{Seq: 2, Op: walOpQueueItem, Item: SnapshotItem{Seq: 2, Type: "tool_call_resolving", ID: "c1"}},
		{Seq: 3, Op: walOpQueueItem, Item: SnapshotItem{Seq: 3, Type: "tool_call_started", ID: "c1", Recovery: string(ToolRecoveryUnsafe)}},
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
	thread := newThread()
	thread.SetToolProvider(staticToolProvider{snap: testToolsSnapshot("calc", "calculate")})
	thread.SetToolResolver(toolResolverFunc(func(context.Context, Thread, ToolCall, json.RawMessage) (ToolDispatch, error) {
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
	thread := newThread()
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
	thread := newThread()
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
	thread := newThread()
	base, err := thread.Checkpoint(CheckpointOptions{Policy: InflightSkip})
	if err != nil {
		t.Fatalf("base checkpoint: %v", err)
	}
	streamer := newFakeStreamer()
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

func TestReplayEndStreamDerivesAwaitingToolResultsFromItems(t *testing.T) {
	base := Checkpoint{Snapshot: ThreadSnapshot{
		Version:         serializedThreadVersion,
		State:           StateIdle,
		IPIndex:         -1,
		QueueStartIndex: -1,
		StreamInsIndex:  -1,
	}}
	wal := []WALEvent{
		{Seq: 1, Op: walOpQueueItem, Item: SnapshotItem{Seq: 1, Type: "user_text", Text: "hello"}},
		{Seq: 2, Op: walOpQueueItem, Item: SnapshotItem{Seq: 2, Type: "send"}},
		{Seq: 3, Op: walOpBeginStream},
		{Seq: 4, Op: walOpAppendStreamItem, Item: SnapshotItem{Seq: 4, Type: "tool_call", ID: "c1", Name: "calc", Args: `{}`}},
		{Seq: 5, Op: walOpEndStream},
	}

	restored, err := RestoreFromCheckpointAndWAL(base, wal, RestoreOptions{})
	if err != nil {
		t.Fatalf("restore wal: %v", err)
	}
	if got := restored.State(); got != StateAwaitingToolResults {
		t.Fatalf("restored state = %q, want %q", got, StateAwaitingToolResults)
	}
}

func TestReplayWALRejectsEventAfterSequenceExhaustion(t *testing.T) {
	// A branch whose mutation sequence is exhausted must fail closed during
	// replay instead of letting prev+1 wraparound to zero and panicking.
	base := Checkpoint{
		Seq: math.MaxUint32,
		Snapshot: ThreadSnapshot{
			Version:         serializedThreadVersion,
			HeadSeq:         math.MaxUint32,
			State:           StateIdle,
			IPIndex:         -1,
			QueueStartIndex: -1,
			StreamInsIndex:  -1,
		},
	}
	// Zero is the wraparound successor of MaxUint32; it would pass the old
	// ev.Seq != prev+1 comparison and only fail later inside a mutation panic.
	wal := []WALEvent{
		{Seq: 0, Op: walOpQueueItem, Item: SnapshotItem{Seq: 0, Type: "user_text", Text: "boom"}},
	}
	if _, err := RestoreFromCheckpointAndWAL(base, wal, RestoreOptions{}); !errors.Is(err, ErrReplayWALSequence) {
		t.Fatalf("replay error = %v, want %v", err, ErrReplayWALSequence)
	}
}

func TestReplayWALRejectsV1StyleSparseItemCandidate(t *testing.T) {
	base := Checkpoint{Snapshot: ThreadSnapshot{
		Version:         serializedThreadVersion,
		State:           StateIdle,
		IPIndex:         -1,
		QueueStartIndex: -1,
		StreamInsIndex:  -1,
	}}
	// Item candidate sequence does not match its event sequence: must fail closed.
	wal := []WALEvent{
		{Seq: 1, Op: walOpQueueItem, Item: SnapshotItem{Seq: 7, Type: "user_text", Text: "hello"}},
	}
	if _, err := RestoreFromCheckpointAndWAL(base, wal, RestoreOptions{}); err == nil {
		t.Fatal("expected replay failure for item candidate sequence not matching event sequence")
	}
}

func TestReplayWALRejectsPatchWithEmptyOrNullMetadata(t *testing.T) {
	base := Checkpoint{Snapshot: ThreadSnapshot{
		Version:         serializedThreadVersion,
		State:           StateIdle,
		IPIndex:         -1,
		QueueStartIndex: -1,
		StreamInsIndex:  -1,
	}}
	wal := []WALEvent{
		{Seq: 1, Op: walOpQueueItem, Item: SnapshotItem{Seq: 1, Type: "user_text", Text: "hello"}},
	}
	for _, name := range []string{"empty string", "json null", "empty object"} {
		meta := ""
		switch name {
		case "json null":
			meta = "null"
		case "empty object":
			meta = "{}"
		}
		withPatch := append(append([]WALEvent{}, wal...), WALEvent{Seq: 2, Op: walOpPatchItemMetadata, Target: 1, Metadata: meta})
		if _, err := RestoreFromCheckpointAndWAL(base, withPatch, RestoreOptions{}); err == nil {
			t.Fatalf("%s patch metadata: expected replay failure", name)
		}
	}
}

func TestReplayWALRejectsPatchWithoutExplicitTarget(t *testing.T) {
	base := Checkpoint{Snapshot: ThreadSnapshot{
		Version:         serializedThreadVersion,
		State:           StateIdle,
		IPIndex:         -1,
		QueueStartIndex: -1,
		StreamInsIndex:  -1,
	}}
	wal := []WALEvent{
		{Seq: 1, Op: walOpQueueItem, Item: SnapshotItem{Seq: 1, Type: "user_text", Text: "hello"}},
		{Seq: 2, Op: walOpPatchItemMetadata, Target: 0, Metadata: `{"k":"v"}`},
	}
	if _, err := RestoreFromCheckpointAndWAL(base, wal, RestoreOptions{}); err == nil {
		t.Fatal("expected replay failure for patch metadata with no explicit target")
	}
}

func TestRestoreFromCheckpointAndWALReplaysToolsSnapshotHandlerLoadData(t *testing.T) {
	thread := newThread()
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

func TestWALAfterDeleteKeysDoesNotAliasThreadWAL(t *testing.T) {
	thread := newThread()
	thread.QueueItem(UserText("a"), PatchItemMetadata{Metadata: map[string]any{"k": "v"}})
	thread.QueueItem(PatchItemMetadata{Metadata: map[string]any{"k": DeleteValue}})

	first := thread.WALAfter(0)
	first[len(first)-1].DeleteKeys[0] = "mutated"
	second := thread.WALAfter(0)
	if got := second[len(second)-1].DeleteKeys; !reflect.DeepEqual(got, []string{"k"}) {
		t.Fatalf("WALAfter mutation aliased thread WAL: %#v", got)
	}
}

func TestReplayWALRejectsConflictingDeletePatch(t *testing.T) {
	base := Checkpoint{Snapshot: ThreadSnapshot{
		Version:         serializedThreadVersion,
		State:           StateIdle,
		IPIndex:         -1,
		QueueStartIndex: -1,
		StreamInsIndex:  -1,
	}}
	prefix := []WALEvent{
		{Seq: 1, Op: walOpQueueItem, Item: SnapshotItem{Seq: 1, Type: "user_text", Text: "hello"}},
	}
	patches := []WALEvent{
		{Seq: 2, Op: walOpPatchItemMetadata, Target: 1, DeleteKeys: []string{"k", "k"}},
		{Seq: 2, Op: walOpPatchItemMetadata, Target: 1, Metadata: `{"k":"v"}`, DeleteKeys: []string{"k"}},
	}
	for _, patch := range patches {
		wal := append(append([]WALEvent(nil), prefix...), patch)
		if _, err := RestoreFromCheckpointAndWAL(base, wal, RestoreOptions{}); err == nil {
			t.Fatalf("expected replay failure for conflicting delete patch %#v", patch)
		}
	}
}
