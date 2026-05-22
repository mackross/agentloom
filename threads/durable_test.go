package threads

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	gschema "github.com/google/jsonschema-go/jsonschema"
)

type threadSnapshot struct {
	State             State
	Items             []Item
	IPIndex           int
	QueueStartIndex   int
	StreamInsertIndex int
}

func TestSnapshotRoundTripPreservesThreadSnapshot(t *testing.T) {
	thread := New()
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
	thread := New()
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
	thread := New()
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
	thread := New()
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
	thread := New()
	thread.QueueItem(ToolCall{CallID: "c1", Name: "calc", Payload: `{"a":1}`})
	thread.QueueItem(ToolCallStarted{CallID: "c1", Continue: ToolContinueManual})

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
	thread := New()
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
	thread := New()
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

func TestWALReplayPreservesRollbackableToolResult(t *testing.T) {
	const hint = `<tool_call_hint tool="calc">Retry with valid JSON.</tool_call_hint>`

	thread := New()
	thread.QueueItem(ToolCallResult{
		CallID: "c1",
		Output: "invalid JSON",
		SafeRollback: &ToolCallSafeRollback{
			SteeringHint: hint,
		},
	})

	events := thread.WALAfter(0)
	if len(events) != 1 {
		t.Fatalf("expected one WAL event, got %#v", events)
	}
	if got := events[0].Item; got.SafeRollback == nil || got.SafeRollback.SteeringHint != hint {
		t.Fatalf("WAL item lost rollback metadata: %#v", got)
	}

	restored := New()
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
	if got.SafeRollback == nil || got.SafeRollback.SteeringHint != hint {
		t.Fatalf("restored result lost rollback metadata: %#v", got)
	}
}

func snapshotThread(t *Thread) threadSnapshot {
	index := map[*item[Item]]int{}
	items := make([]Item, 0)
	i := 0
	for n := t.items.Head(); n != nil; n = n.Next {
		index[n] = i
		items = append(items, n.Item)
		i++
	}
	return threadSnapshot{
		State:             t.State(),
		Items:             items,
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
	thread := New()
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
	thread := New()
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
