package threads

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	gschema "github.com/google/jsonschema-go/jsonschema"
)

type threadSnapshot struct {
	HeadSeq           uint32
	State             State
	Items             []Item
	Seqs              []ItemSeq
	Metadata          []map[string]any
	IPIndex           int
	QueueStartIndex   int
	StreamInsertIndex int
}

func TestEmptyToolResultDataRoundTrips(t *testing.T) {
	// Tool-result Data is payload, not item metadata. Preserve the v1 decoder
	// behavior for all persisted empty forms, including explicit {} and null.
	cases := []struct {
		name string
		raw  string
		want map[string]any
	}{
		{name: "omitted", raw: "", want: nil},
		{name: "empty object", raw: "{}", want: map[string]any{}},
		{name: "null", raw: "null", want: nil},
		{name: "nonempty object", raw: `{"k":"v"}`, want: map[string]any{"k": "v"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := snapshotItemToItem(SnapshotItem{
				Type: "tool_result",
				ID:   "c1",
				Data: tc.raw,
			})
			if err != nil {
				t.Fatalf("decode tool data %q: %v", tc.raw, err)
			}
			if data := got.(ToolCallResult).Data; !reflect.DeepEqual(data, tc.want) {
				t.Fatalf("decoded data = %#v, want %#v", data, tc.want)
			}
		})
	}

	// The normal encoder still canonicalizes nil and empty maps to the omitted
	// form used by newly written snapshots.
	for _, data := range []map[string]any{nil, {}} {
		raw, err := itemToSnapshotItem(ToolCallResult{CallID: "c1", Data: data})
		if err != nil {
			t.Fatalf("encode data %#v: %v", data, err)
		}
		if raw.Data != "" {
			t.Fatalf("encoded empty data = %q, want omitted", raw.Data)
		}
	}
}

func TestSnapshotRoundTripPreservesThreadSnapshot(t *testing.T) {
	thread := newThread()
	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
		b.Emit(AssistantText("world"))
	})
	thread.SetExecutor(NewThreadExecutor(streamer.Streamer()))

	thread.QueueItem(AssistantInstruction("be concise"))
	thread.QueueItem(UserText("hello"))
	thread.QueueItem(SendItem{})

	snap, err := thread.Snapshot()
	if err != nil {
		t.Fatalf("snapshot thread: %v", err)
	}
	decoded, err := RestoreThreadSnapshot(snap)
	if err != nil {
		t.Fatalf("restore snapshot: %v", err)
	}

	before := snapshotThread(thread)
	after := snapshotThread(decoded)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("snapshot mismatch\nbefore: %#v\nafter:  %#v", before, after)
	}
}

func TestSnapshotEncodeDecodeEncodeStableBytes(t *testing.T) {
	thread := newThread()
	thread.QueueItem(UserText("u1"))
	thread.QueueItem(UserText("u2"))
	thread.QueueItem(SendItem{})

	firstSnap, err := thread.Snapshot()
	if err != nil {
		t.Fatalf("first snapshot: %v", err)
	}
	first, err := json.Marshal(firstSnap)
	if err != nil {
		t.Fatalf("first marshal: %v", err)
	}

	var decodedSnap ThreadSnapshot
	if err := json.Unmarshal(first, &decodedSnap); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	decoded, err := RestoreThreadSnapshot(decodedSnap)
	if err != nil {
		t.Fatalf("restore decoded snapshot: %v", err)
	}
	secondSnap, err := decoded.Snapshot()
	if err != nil {
		t.Fatalf("second snapshot: %v", err)
	}
	second, err := json.Marshal(secondSnap)
	if err != nil {
		t.Fatalf("second marshal: %v", err)
	}

	if !bytes.Equal(first, second) {
		t.Fatalf("snapshot encode/decode/encode mismatch\nfirst:  %s\nsecond: %s", string(first), string(second))
	}
}

func TestSnapshotRoundTripPreservesToolCallItems(t *testing.T) {
	thread := newThread()
	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
		b.Emit(ToolCallChunk{CallID: "c1", Name: "calc", PayloadDelta: `{"a":`})
		b.Emit(ToolCallChunk{CallID: "c1", PayloadDelta: `1}`})
		b.Emit(ToolCall{CallID: "c1", Name: "calc", Payload: `{"a":1}`})
	})
	thread.SetExecutor(NewThreadExecutor(streamer.Streamer()))
	thread.QueueItem(UserText("hello"))
	thread.QueueItem(SendItem{})

	snap, err := thread.Snapshot()
	if err != nil {
		t.Fatalf("snapshot thread: %v", err)
	}
	decoded, err := RestoreThreadSnapshot(snap)
	if err != nil {
		t.Fatalf("restore snapshot: %v", err)
	}

	before := snapshotThread(thread)
	after := snapshotThread(decoded)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("snapshot mismatch\nbefore: %#v\nafter:  %#v", before, after)
	}
}

func TestSnapshotRoundTripPreservesToolSnapshotItems(t *testing.T) {
	thread := newThread()
	thread.QueueItem(testToolsSnapshot("calc", "calculate"))
	thread.QueueItem(UserText("hello"))

	snap, err := thread.Snapshot()
	if err != nil {
		t.Fatalf("snapshot thread: %v", err)
	}
	decoded, err := RestoreThreadSnapshot(snap)
	if err != nil {
		t.Fatalf("restore snapshot: %v", err)
	}

	before := snapshotThread(thread)
	after := snapshotThread(decoded)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("snapshot mismatch\nbefore: %#v\nafter:  %#v", before, after)
	}
}

func TestSnapshotRoundTripPreservesToolCallStartedItems(t *testing.T) {
	thread := newThread()
	thread.QueueItem(ToolCall{CallID: "c1", Name: "calc", Payload: `{"a":1}`})
	thread.QueueItem(ToolCallStarted{CallID: "c1", Continue: ToolContinueManual})
	thread.cb.setState(StateAwaitingToolResults)

	snap, err := thread.Snapshot()
	if err != nil {
		t.Fatalf("snapshot thread: %v", err)
	}
	decoded, err := RestoreThreadSnapshot(snap)
	if err != nil {
		t.Fatalf("restore snapshot: %v", err)
	}

	before := snapshotThread(thread)
	after := snapshotThread(decoded)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("snapshot mismatch\nbefore: %#v\nafter:  %#v", before, after)
	}
}

func TestSnapshotRoundTripRestoresToolsSnapshotHandlerLoadData(t *testing.T) {
	thread := newThread()
	want := ToolsSnapshot{
		Snapshot: ToolOfferSnapshot{Offered: []ToolSpec{{
			Name:        "write_file",
			Description: "write contents",
			Payload:     ToolPayloadJSONSchema(gschema.Schema{Type: "object"}),
		}}},
		Handlers: []ToolHandlerBinding{
			{Name: "write_file", HandlerLoadData: []byte(`{"function":"tool/write-file@v1","filename":"notes.txt"}`)},
			{Name: "write_file_atomic", HandlerLoadData: []byte(`{"function":"tool/write-file/atomic@v1","filename":"notes.txt"}`)},
		},
	}
	thread.QueueItem(want)

	snap, err := thread.Snapshot()
	if err != nil {
		t.Fatalf("snapshot thread: %v", err)
	}
	decoded, err := RestoreThreadSnapshot(snap)
	if err != nil {
		t.Fatalf("restore snapshot: %v", err)
	}

	items := decoded.items.Slice()
	if len(items) != 1 {
		t.Fatalf("expected one restored item, got %#v", items)
	}
	got, ok := items[0].(ToolsSnapshot)
	if !ok {
		t.Fatalf("expected ToolsSnapshot, got %T", items[0])
	}
	if !reflect.DeepEqual(got.Handlers, want.Handlers) {
		t.Fatalf("unexpected restored handler load data\nwant: %#v\ngot:  %#v", want.Handlers, got.Handlers)
	}
}

func TestSnapshotRoundTripRestoresToolResultItemsAsCanonicalThreadBlocks(t *testing.T) {
	thread := newThread()
	thread.QueueItem(ToolCallResult{
		CallID: "c1",
		Output: `{"ok":true}`,
		Data:   map[string]any{"json": map[string]any{"ok": true}},
	})

	snap, err := thread.Snapshot()
	if err != nil {
		t.Fatalf("snapshot thread: %v", err)
	}
	decoded, err := RestoreThreadSnapshot(snap)
	if err != nil {
		t.Fatalf("restore snapshot: %v", err)
	}

	items := decoded.items.Slice()
	if len(items) != 1 {
		t.Fatalf("expected one restored item, got %#v", items)
	}
	got, ok := items[0].(ToolCallResult)
	if !ok {
		t.Fatalf("expected canonical ToolCallResult, got %T", items[0])
	}
	if got.CallID != "c1" || got.Output != `{"ok":true}` {
		t.Fatalf("unexpected restored tool result: %#v", got)
	}
	if want := map[string]any{"json": map[string]any{"ok": true}}; !reflect.DeepEqual(got.Data, want) {
		t.Fatalf("unexpected restored tool result data: %#v", got.Data)
	}
}

func TestRestoreLegacyIdleSnapshotNormalizesPendingToolCallsToAwaiting(t *testing.T) {
	snapshot := ThreadSnapshot{
		Version: serializedThreadVersion,
		HeadSeq: 3,
		State:   StateIdle,
		Items: []SnapshotItem{
			{Seq: 1, Type: "user_text", Text: "hello"},
			{Seq: 2, Type: "send"},
			{Seq: 3, Type: "tool_call", ID: "c1", Name: "calc", Args: `{}`},
		},
		IPIndex:         2,
		QueueStartIndex: -1,
		StreamInsIndex:  -1,
	}

	restored, err := RestoreThreadSnapshot(snapshot)
	if err != nil {
		t.Fatalf("restore legacy snapshot: %v", err)
	}
	if got := restored.State(); got != StateAwaitingToolResults {
		t.Fatalf("restored state = %q, want %q", got, StateAwaitingToolResults)
	}
}

func TestWALReplayPreservesRollbackableToolResult(t *testing.T) {
	const hint = `<tool_call_hint tool="calc">Retry with valid JSON.</tool_call_hint>`

	thread := newThread()
	thread.QueueItem(ToolCallResult{
		CallID: "c1",
		Output: "invalid JSON",
		SafeRollback: &ToolCallSafeRollback{
			SteeringHint: hint,
			RetryAttempt: 2,
			MaxRetries:   3,
		},
	})

	events := thread.WALAfter(0)
	if len(events) != 1 {
		t.Fatalf("expected one WAL event, got %#v", events)
	}
	if got := events[0].Item; got.SafeRollback == nil || got.SafeRollback.SteeringHint != hint || got.SafeRollback.RetryAttempt != 2 || got.SafeRollback.MaxRetries != 3 {
		t.Fatalf("WAL item lost rollback metadata: %#v", got)
	}

	restored := newThread()
	if err := restored.ReplayWAL(events); err != nil {
		t.Fatalf("replay WAL: %v", err)
	}
	items := restored.items.Slice()
	if len(items) != 1 {
		t.Fatalf("expected one restored item, got %#v", items)
	}
	got, ok := items[0].(ToolCallResult)
	if !ok {
		t.Fatalf("expected ToolCallResult, got %T", items[0])
	}
	if got.SafeRollback == nil || got.SafeRollback.SteeringHint != hint || got.SafeRollback.RetryAttempt != 2 || got.SafeRollback.MaxRetries != 3 {
		t.Fatalf("restored result lost rollback metadata: %#v", got)
	}
}

func TestMemoryDurableStoreLoadDoesNotAliasData(t *testing.T) {
	t.Run("checkpoint", func(t *testing.T) {
		store := NewMemoryDurableStore(Checkpoint{Snapshot: ThreadSnapshot{
			Version:         serializedThreadVersion,
			State:           StateIdle,
			IPIndex:         -1,
			QueueStartIndex: -1,
			StreamInsIndex:  -1,
			Items: []SnapshotItem{{
				Type: "tool_result",
				ID:   "c1",
				SafeRollback: &ToolCallSafeRollback{
					SteeringHint: "original",
					RetryAttempt: 1,
					MaxRetries:   2,
				},
			}},
		}})

		loaded, _ := store.Load()
		loaded.Snapshot.Items[0].SafeRollback.SteeringHint = "mutated"
		loadedAgain, _ := store.Load()
		if got := loadedAgain.Snapshot.Items[0].SafeRollback.SteeringHint; got != "original" {
			t.Fatalf("loaded checkpoint mutated stored rollback metadata: got %q", got)
		}
	})

	t.Run("wal", func(t *testing.T) {
		store := NewMemoryDurableStore(Checkpoint{})
		store.AppendWALDiff([]WALEvent{{
			Seq: 1,
			Op:  walOpQueueItem,
			Item: SnapshotItem{
				Seq:  1,
				Type: "tool_result",
				ID:   "c1",
				SafeRollback: &ToolCallSafeRollback{
					SteeringHint: "original",
					RetryAttempt: 1,
					MaxRetries:   2,
				},
			},
		}})

		_, loaded := store.Load()
		loaded[0].Item.SafeRollback.SteeringHint = "mutated"
		_, loadedAgain := store.Load()
		if got := loadedAgain[0].Item.SafeRollback.SteeringHint; got != "original" {
			t.Fatalf("loaded WAL mutated stored rollback metadata: got %q", got)
		}
	})

	t.Run("wal delete keys", func(t *testing.T) {
		store := NewMemoryDurableStore(Checkpoint{})
		store.AppendWALDiff([]WALEvent{{
			Seq:        1,
			Op:         walOpPatchItemMetadata,
			Target:     1,
			DeleteKeys: []string{"k"},
		}})

		_, loaded := store.Load()
		loaded[0].DeleteKeys[0] = "mutated"
		_, loadedAgain := store.Load()
		if got := loadedAgain[0].DeleteKeys; !reflect.DeepEqual(got, []string{"k"}) {
			t.Fatalf("loaded WAL mutated stored delete keys: %#v", got)
		}
	})
}

func snapshotThread(t *thread) threadSnapshot {
	index := map[*item[Item]]int{}
	items := make([]Item, 0)
	seqs := make([]ItemSeq, 0)
	meta := make([]map[string]any, 0)
	i := 0
	for n := t.items.Head(); n != nil; n = n.Next {
		index[n] = i
		items = append(items, n.Item)
		seqs = append(seqs, n.Seq)
		meta = append(meta, n.Metadata)
		i++
	}
	return threadSnapshot{
		HeadSeq:           t.Seq(),
		State:             t.State(),
		Items:             items,
		Seqs:              seqs,
		Metadata:          meta,
		IPIndex:           indexOrNil(index, t.cb.ip),
		QueueStartIndex:   indexOrNil(index, t.cb.queueStartItem),
		StreamInsertIndex: indexOrNil(index, t.cb.streamInsertionPoint),
	}
}

func indexOrNil(index map[*item[Item]]int, n *item[Item]) int {
	if n == nil {
		return -1
	}
	if i, ok := index[n]; ok {
		return i
	}
	return -1
}

type fakeDurableStore struct {
	snapshots   []Checkpoint
	appended    []WALEvent
	panicAppend any
}

func (f *fakeDurableStore) ReplaceSnapshot(cp Checkpoint) {
	f.snapshots = append(f.snapshots, cp)
}

func (f *fakeDurableStore) AppendWALDiff(diff []WALEvent) {
	if f.panicAppend != nil {
		panic(f.panicAppend)
	}
	f.appended = append(f.appended, diff...)
}

func (f *fakeDurableStore) Load() (Checkpoint, []WALEvent) {
	var cp Checkpoint
	if n := len(f.snapshots); n > 0 {
		cp = f.snapshots[n-1]
	}
	return cp, append([]WALEvent(nil), f.appended...)
}

func TestThreadDurableStoreReceivesWALDiffs(t *testing.T) {
	thread := newThread()
	store := &fakeDurableStore{}
	thread.SetDurableStore(store)
	if len(store.snapshots) != 1 {
		t.Fatalf("expected initial snapshot replace, got %d", len(store.snapshots))
	}

	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
		b.Emit(AssistantText("w1"))
		b.Emit(AssistantText("w2"))
	})
	thread.SetExecutor(NewThreadExecutor(streamer.Streamer()))
	thread.QueueItem(UserText("hello"))
	thread.QueueItem(SendItem{})

	if len(store.appended) != 6 {
		t.Fatalf("expected 6 wal appends, got %d", len(store.appended))
	}
	gotOps := make([]string, 0, len(store.appended))
	for _, ev := range store.appended {
		gotOps = append(gotOps, ev.Op)
	}
	wantOps := []string{walOpQueueItem, walOpQueueItem, walOpBeginStream, walOpAppendStreamItem, walOpAppendStreamItem, walOpEndStream}
	if !reflect.DeepEqual(gotOps, wantOps) {
		t.Fatalf("unexpected wal op sequence: %#v", gotOps)
	}
}

func TestThreadAppendWALPanicsWithContextOnDurabilityFailure(t *testing.T) {
	thread := newThread()
	thread.SetDurableStore(&fakeDurableStore{panicAppend: "disk full"})

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic")
		}
		msg := fmt.Sprint(r)
		if !strings.Contains(msg, "append wal") {
			t.Fatalf("expected append-wal panic context, got %q", msg)
		}
	}()

	thread.QueueItem(UserText("boom"))
}

func TestMutationSequenceHelpersGuardOverflow(t *testing.T) {
	// Normal advancement allocates strictly increasing nonzero sequences.
	t1 := newThread()
	if got := t1.advanceMutationSeq(); got != 1 {
		t.Fatalf("first advance = %d, want 1", got)
	}
	if got := t1.advanceMutationSeq(); got != 2 {
		t.Fatalf("second advance = %d, want 2", got)
	}
	if t1.mutationSeq != 2 {
		t.Fatalf("mutationSeq = %d, want 2", t1.mutationSeq)
	}

	// A candidate does not commit.
	t2 := newThread()
	got, err := t2.candidateMutationSeq()
	if err != nil || got != 1 {
		t.Fatalf("candidate = %d, %v; want 1, nil", got, err)
	}
	if t2.mutationSeq != 0 {
		t.Fatalf("candidate committed: mutationSeq = %d, want 0", t2.mutationSeq)
	}

	// A candidate reports overflow without committing; normal advancement has
	// no error return and therefore panics on the same exhausted sequence.
	t3 := newThread()
	t3.mutationSeq = math.MaxUint32
	if seq, err := t3.candidateMutationSeq(); !errors.Is(err, errItemSequenceOverflow) || seq != 0 {
		t.Fatalf("overflow candidate = %d, %v; want 0, overflow", seq, err)
	}
	assertPanics(t, func() { t3.advanceMutationSeq() }, "advance sequence overflow")
	if t3.mutationSeq != math.MaxUint32 {
		t.Fatalf("overflow mutation mutated sequence: %d", t3.mutationSeq)
	}
}

func TestQueueItemOverflowPanicsBeforeMutation(t *testing.T) {
	thread := newThread()
	thread.mutationSeq = math.MaxUint32

	assertPanics(t, func() { thread.QueueItem(UserText("never queued")) }, "QueueItem sequence overflow")
	if thread.mutationSeq != math.MaxUint32 {
		t.Fatalf("overflow changed mutation sequence: %d", thread.mutationSeq)
	}
	if thread.items.Head() != nil {
		t.Fatalf("overflow queued an item: %#v", thread.items.Slice())
	}
	if len(thread.wal) != 0 {
		t.Fatalf("overflow appended WAL: %#v", thread.wal)
	}
}
