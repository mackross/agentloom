package threads

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// This file covers the acceptance criteria in PLAN.md's identity, coalescing,
// metadata, durability, and branching sections that are not already exercised
// by the existing test suite.

func TestStreamedAssistantChunksRetainFirstSeq(t *testing.T) {
	thread := newThread()
	thread.QueueItem(UserText("u"))
	thread.QueueItem(SendItem{})
	if err := thread.beginStreaming(); err != nil {
		t.Fatal(err)
	}
	if err := thread.appendStreamItem(AssistantText("a1")); err != nil {
		t.Fatal(err)
	}
	if err := thread.appendStreamItem(AssistantText("a2")); err != nil {
		t.Fatal(err)
	}
	if err := thread.endStreaming(); err != nil {
		t.Fatal(err)
	}
	n := thread.items.Head() // u
	if n == nil {
		t.Fatal("no head")
	}
	n = n.Next // send
	n = n.Next // assistant
	if n == nil {
		t.Fatal("no assistant node")
	}
	if got := n.Item; got != AssistantText("a1a2") {
		t.Fatalf("assistant item = %#v, want a1a2", got)
	}
	if n.Seq != 4 {
		t.Fatalf("assistant node seq = %d, want 4 (first chunk seq)", n.Seq)
	}
}

func TestToolChunkFinalizationRetainsSeqAndMetadata(t *testing.T) {
	thread := newThread()
	thread.QueueItem(UserText("u"))
	thread.QueueItem(SendItem{})
	if err := thread.beginStreaming(); err != nil {
		t.Fatal(err)
	}
	if err := thread.appendStreamItem(ToolCallChunk{CallID: "c1", Name: "calc", PayloadDelta: `{"a":`}); err != nil {
		t.Fatal(err)
	}
	if err := thread.appendStreamItem(PatchItemMetadata{Metadata: map[string]any{"chunk": "meta"}}); err != nil {
		t.Fatal(err)
	}
	if err := thread.appendStreamItem(ToolCallChunk{CallID: "c1", PayloadDelta: `1}`}); err != nil {
		t.Fatal(err)
	}
	if err := thread.appendStreamItem(ToolCall{CallID: "c1", Name: "calc", Payload: `{"a":1}`}); err != nil {
		t.Fatal(err)
	}
	if err := thread.endStreaming(); err != nil {
		t.Fatal(err)
	}
	n := thread.items.Head().Next.Next
	if n == nil {
		t.Fatal("no tool node")
	}
	tc, ok := n.Item.(ToolCall)
	if !ok {
		t.Fatalf("final node item = %T, want ToolCall", n.Item)
	}
	if tc.Payload != `{"a":1}` {
		t.Fatalf("final tool payload = %q", tc.Payload)
	}
	if n.Seq != 4 {
		t.Fatalf("tool node seq = %d, want 4 (first chunk seq)", n.Seq)
	}
	if got := n.Metadata["chunk"]; got != "meta" {
		t.Fatalf("tool node metadata = %#v, want chunk=meta", n.Metadata)
	}
}

func TestInsertBeforeSendHasLargerSeqBeforeSmaller(t *testing.T) {
	thread := newThread()
	thread.QueueItem(UserText("hello"))
	thread.QueueItem(SendItem{})
	if err := thread.beginStreaming(); err != nil {
		t.Fatal(err)
	}
	if err := thread.appendStreamItem(ToolCall{CallID: "c1", Name: "calc", Payload: `{}`}); err != nil {
		t.Fatal(err)
	}
	if err := thread.endStreaming(); err != nil {
		t.Fatal(err)
	}
	thread.QueueItem(SendItem{}) // blocked by the pending tool call
	thread.QueueItem(ToolCallResult{CallID: "c1", Output: "1"})

	var seqs []ItemSeq
	for n := thread.items.Head(); n != nil; n = n.Next {
		seqs = append(seqs, n.Seq)
	}
	want := []ItemSeq{1, 2, 4, 7, 6}
	if len(seqs) != len(want) {
		t.Fatalf("node seqs = %v, want %v", seqs, want)
	}
	for i := range want {
		if seqs[i] != want[i] {
			t.Fatalf("node seqs = %v, want %v", seqs, want)
		}
	}
	// Transcript order must be authoritative: the result (seq 7) precedes the
	// blocked send (seq 6).
	for i := range want {
		if i > 0 && seqs[i] == want[i-1] {
			t.Fatalf("duplicate seq %d in transcript", want[i])
		}
	}
}

func TestRestoreThreadSnapshotDoesNotReuseItemSeq(t *testing.T) {
	thread := newThread()
	thread.QueueItem(UserText("a"))
	thread.QueueItem(UserText("b"))
	snap, err := thread.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.HeadSeq != 2 {
		t.Fatalf("head seq = %d, want 2", snap.HeadSeq)
	}
	restored, err := RestoreThreadSnapshot(snap)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if restored.Seq() != 2 {
		t.Fatalf("restored seq = %d, want 2", restored.Seq())
	}
	// Queue a non-coalescable item: it must receive a fresh sequence above HeadSeq.
	restored.QueueItem(SendItem{})
	seen := map[ItemSeq]bool{}
	var got ItemSeq
	for n := restored.items.Head(); n != nil; n = n.Next {
		if n.Seq == 0 {
			t.Fatalf("zero seq node")
		}
		if seen[n.Seq] {
			t.Fatalf("reused seq %d", n.Seq)
		}
		seen[n.Seq] = true
		got = n.Seq
	}
	if got != 3 {
		t.Fatalf("newly queued item seq = %d, want 3", got)
	}
}

func TestRestoreThreadSnapshotRejectsZeroDuplicateOutOfRangeSeq(t *testing.T) {
	mk := func(head uint32, items []SnapshotItem) ThreadSnapshot {
		return ThreadSnapshot{
			Version:         serializedThreadVersion,
			HeadSeq:         head,
			State:           StateIdle,
			Items:           items,
			IPIndex:         len(items) - 1,
			QueueStartIndex: -1,
			StreamInsIndex:  -1,
		}
	}
	cases := []struct {
		name string
		snap ThreadSnapshot
	}{
		{"zero seq", mk(1, []SnapshotItem{{Seq: 0, Type: "user_text", Text: "a"}})},
		{"duplicate seq", mk(2, []SnapshotItem{
			{Seq: 1, Type: "user_text", Text: "a"},
			{Seq: 1, Type: "send"},
		})},
		{"out of range seq", mk(2, []SnapshotItem{{Seq: 5, Type: "user_text", Text: "a"}})},
		{"null metadata", mk(1, []SnapshotItem{{Seq: 1, Type: "user_text", Text: "a", Metadata: "null"}})},
		{"non-object metadata", mk(1, []SnapshotItem{{Seq: 1, Type: "user_text", Text: "a", Metadata: `"scalar"`}})},
		{"v1 version", ThreadSnapshot{Version: 1, State: StateIdle, IPIndex: -1, QueueStartIndex: -1, StreamInsIndex: -1}},
	}
	for _, c := range cases {
		if _, err := RestoreThreadSnapshot(c.snap); err == nil {
			t.Fatalf("%s: expected restore failure", c.name)
		}
	}
}

func TestRestoreThreadSnapshotAcceptsEmptyObjectMetadata(t *testing.T) {
	snap := ThreadSnapshot{
		Version:         serializedThreadVersion,
		HeadSeq:         1,
		State:           StateIdle,
		Items:           []SnapshotItem{{Seq: 1, Type: "user_text", Text: "a", Metadata: "{}"}},
		IPIndex:         0,
		QueueStartIndex: -1,
		StreamInsIndex:  -1,
	}
	restored, err := RestoreThreadSnapshot(snap)
	if err != nil {
		t.Fatalf("restore empty metadata object: %v", err)
	}
	if restored.items.Head().Metadata != nil {
		t.Fatalf("empty metadata object was not canonicalized to nil: %#v", restored.items.Head().Metadata)
	}
}

func TestTurnIDIsFirstSurvivingTextSeqAndStable(t *testing.T) {
	thread := newThread()
	thread.QueueItem(UserText("hello"))
	if id := thread.CompletedTurns()[0].ID(); id != 1 {
		t.Fatalf("turn id = %d, want 1", id)
	}
	thread.QueueItem(UserText(" world")) // coalesces; left seq retained
	turns := thread.CompletedTurns()
	if turns[0].ID() != 1 || turns[0].Text() != "hello world" {
		t.Fatalf("turn after coalesce = id %d text %q", turns[0].ID(), turns[0].Text())
	}
	// Appending later turns must not change existing IDs.
	thread.QueueItem(SendItem{})
	thread.QueueItem(AssistantText("reply"))
	turns = thread.CompletedTurns()
	if turns[0].ID() != 1 {
		t.Fatalf("first turn id changed to %d after appending turns", turns[0].ID())
	}
	if turns[1].Text() != "reply" {
		t.Fatalf("second turn text = %q", turns[1].Text())
	}
	if turns[1].ID() == turns[0].ID() {
		t.Fatal("turn ids must be distinct")
	}
}

func TestAssistantTurnIDStableAcrossToolExchange(t *testing.T) {
	thread := newThread()
	thread.QueueItem(UserText("u"))
	thread.QueueItem(SendItem{})
	if err := thread.beginStreaming(); err != nil {
		t.Fatal(err)
	}
	if err := thread.appendStreamItem(AssistantText("first")); err != nil {
		t.Fatal(err)
	}
	if err := thread.endStreaming(); err != nil {
		t.Fatal(err)
	}
	id := thread.CompletedTurns()[1].ID()

	thread.QueueItem(ToolCall{CallID: "c1", Name: "lookup", Payload: `{}`})
	thread.QueueItem(ToolCallResult{CallID: "c1", Output: "42"})
	thread.QueueItem(AssistantText(" more"))

	turns := thread.CompletedTurns()
	if turns[1].ID() != id {
		t.Fatalf("assistant turn id changed %d -> %d across tool exchange", id, turns[1].ID())
	}
	if turns[1].Text() != "first more" {
		t.Fatalf("assistant turn text = %q, want \"first more\"", turns[1].Text())
	}
	// The branch checkpoint includes the current complete turn boundary.
	cp, turn, err := thread.checkpointAtTurn(turns[1].ID())
	if err != nil {
		t.Fatalf("checkpoint assistant turn: %v", err)
	}
	if turn.ID() != id {
		t.Fatalf("checkpoint turn id = %d, want %d", turn.ID(), id)
	}
	foundFirst, foundResult := false, false
	for _, raw := range cp.Snapshot.Items {
		if raw.Type == "assistant_text" && raw.Text == "first" {
			foundFirst = true
		}
		if raw.Type == "tool_result" && raw.ID == "c1" {
			foundResult = true
		}
	}
	if !foundFirst || !foundResult {
		t.Fatalf("checkpoint prefix missing turn content: %#v", cp.Snapshot.Items)
	}
}

func TestZeroTargetPatchPersistsExplicitTarget(t *testing.T) {
	thread := newThread()
	thread.QueueItem(UserText("a"))
	thread.QueueItem(PatchItemMetadata{Metadata: map[string]any{"k": "v"}})
	events := thread.WALAfter(0)
	last := events[len(events)-1]
	if last.Op != walOpPatchItemMetadata {
		t.Fatalf("last wal op = %q, want patch_item_metadata", last.Op)
	}
	if last.Target != 1 {
		t.Fatalf("patch target = %d, want 1 (resolved from zero)", last.Target)
	}
}

func TestEmptyPatchIsNoOp(t *testing.T) {
	thread := newThread()
	thread.QueueItem(UserText("a"))
	seqBefore := thread.Seq()
	thread.QueueItem(PatchItemMetadata{})
	if thread.Seq() != seqBefore {
		t.Fatalf("empty patch advanced seq: %d -> %d", seqBefore, thread.Seq())
	}
	if len(thread.WALAfter(0)) != 1 {
		t.Fatalf("empty patch appended WAL: %#v", thread.WALAfter(0))
	}
	if len(thread.items.Slice()) != 1 {
		t.Fatalf("empty patch created nodes: %#v", thread.items.Slice())
	}
}

func TestMultiplePatchesMergeLatestWins(t *testing.T) {
	thread := newThread()
	thread.QueueItem(UserText("a"))
	thread.QueueItem(PatchItemMetadata{Metadata: map[string]any{"k": "v1", "x": "1"}})
	thread.QueueItem(PatchItemMetadata{Metadata: map[string]any{"k": "v2"}})
	head := thread.items.Head()
	if got := head.Metadata["k"]; got != "v2" {
		t.Fatalf("key k = %v, want v2", got)
	}
	if got := head.Metadata["x"]; got != "1" {
		t.Fatalf("key x = %v, want 1", got)
	}
}

func TestUserTurnBranchSyntheticSendUniqueSeqAndChildContinues(t *testing.T) {
	thread := newThread()
	thread.QueueItem(UserText("u"))
	turn := thread.CompletedTurns()[0]
	sourceHead := thread.Seq()
	if sourceHead != 1 {
		t.Fatalf("source head = %d, want 1", sourceHead)
	}
	cp, _, err := thread.checkpointAtTurn(turn.ID())
	if err != nil {
		t.Fatalf("checkpoint user turn: %v", err)
	}
	if cp.Seq != sourceHead+1 {
		t.Fatalf("checkpoint seq = %d, want %d", cp.Seq, sourceHead+1)
	}
	if cp.Snapshot.HeadSeq != sourceHead+1 {
		t.Fatalf("snapshot head seq = %d, want %d", cp.Snapshot.HeadSeq, sourceHead+1)
	}
	if len(cp.Snapshot.Items) != 2 || cp.Snapshot.Items[1].Type != "send" {
		t.Fatalf("user-turn checkpoint items = %#v, want [u send]", cp.Snapshot.Items)
	}
	if cp.Snapshot.Items[1].Seq != ItemSeq(sourceHead+1) {
		t.Fatalf("synthetic send seq = %d, want %d", cp.Snapshot.Items[1].Seq, sourceHead+1)
	}

	child, err := RestoreCheckpoint(cp, RestoreOptions{AllowUnsafe: true})
	if err != nil {
		t.Fatalf("restore child: %v", err)
	}
	child.QueueItem(UserText("child"))
	seen := map[ItemSeq]bool{}
	for n := child.items.Head(); n != nil; n = n.Next {
		if n.Seq == 0 || seen[n.Seq] {
			t.Fatalf("child has invalid seq %d", n.Seq)
		}
		seen[n.Seq] = true
	}
	if got := child.items.Tail().Seq; got != ItemSeq(sourceHead+2) {
		t.Fatalf("child new item seq = %d, want %d", got, sourceHead+2)
	}
}

func TestChildBranchPreservesParentItemSeqsAndSourceFields(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryBranchStore()
	stored, err := store.CreateBranch(ctx, BranchCreateOptions{ID: "root"})
	if err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	branch, err := stored.Load(RestoreOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	branch.SetDurableStore(stored.Durable)
	branch.QueueItem(UserText("hello"))
	branch.QueueItem(SendItem{})
	branch.QueueItem(AssistantText("hi"))
	if err := branch.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	assistantID := ItemSeq(3)
	manager := NewDefaultBranchManager(store, "test")
	child, err := manager.Open(ctx, "/branch/root/seq/3", OpenAsEphemeralCopy("copy"), OpenWithoutEventLoop())
	if err != nil {
		t.Fatalf("Open turn: %v", err)
	}
	defer child.Close()
	rec := child.Record()
	if rec.SourceTurnSeq != assistantID {
		t.Fatalf("SourceTurnSeq = %d, want %d", rec.SourceTurnSeq, assistantID)
	}
	if rec.SourceTurnRole != TurnAssistant {
		t.Fatalf("SourceTurnRole = %q, want assistant", rec.SourceTurnRole)
	}
	if rec.SourceHeadSeq != 3 {
		t.Fatalf("SourceHeadSeq = %d, want 3", rec.SourceHeadSeq)
	}

	var seqs []ItemSeq
	for n := child.thread.items.Head(); n != nil; n = n.Next {
		seqs = append(seqs, n.Seq)
	}
	if got, want := seqs, []ItemSeq{1, 2, 3}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("child preserved seqs = %v, want %v", got, want)
	}

	child.QueueItem(UserText("another"))
	if got := child.thread.items.Tail().Seq; got != 4 {
		t.Fatalf("child mutation seq = %d, want 4 (continues above parent head)", got)
	}
}

func TestUserTurnBranchSourceFieldsDistinguishTurnAndHead(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryBranchStore()
	stored, err := store.CreateBranch(ctx, BranchCreateOptions{ID: "root"})
	if err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	branch, err := stored.Load(RestoreOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	branch.SetDurableStore(stored.Durable)
	branch.QueueItem(UserText("hello"))
	branch.QueueItem(SendItem{})
	branch.QueueItem(AssistantText("hi"))
	if err := branch.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	manager := NewDefaultBranchManager(store, "test")
	child, err := manager.Open(ctx, "/branch/root/seq/1", OpenAsEphemeralCopy("u-copy"), OpenWithoutEventLoop())
	if err != nil {
		t.Fatalf("Open user turn: %v", err)
	}
	defer child.Close()
	rec := child.Record()
	if rec.SourceTurnSeq != 1 {
		t.Fatalf("SourceTurnSeq = %d, want 1", rec.SourceTurnSeq)
	}
	if rec.SourceTurnRole != TurnUser {
		t.Fatalf("SourceTurnRole = %q, want user", rec.SourceTurnRole)
	}
	if rec.SourceHeadSeq != 3 {
		t.Fatalf("SourceHeadSeq = %d, want 3 (parent head at branch time)", rec.SourceHeadSeq)
	}
	// Synthetic send occupies seq 4 at child head; future child mutations must
	// continue above it without colliding.
	seen := map[ItemSeq]bool{}
	var maxSeq ItemSeq
	for n := child.thread.items.Head(); n != nil; n = n.Next {
		if n.Seq == 0 || seen[n.Seq] {
			t.Fatalf("child invalid seq %d", n.Seq)
		}
		seen[n.Seq] = true
		if n.Seq > maxSeq {
			maxSeq = n.Seq
		}
	}
	child.QueueItem(UserText("more"))
	if got := child.thread.items.Tail().Seq; got <= maxSeq || seen[got] {
		t.Fatalf("child mutation seq %d collides with existing max %d", got, maxSeq)
	}
}

func TestBranchPrefixIncludesMetadataPatchedLaterAtParentHead(t *testing.T) {
	thread := newThread()
	thread.QueueItem(UserText("first"))
	thread.QueueItem(SendItem{})
	thread.QueueItem(AssistantText("answer"))
	assistantID := thread.CompletedTurns()[1].ID()
	// Patch metadata onto the assistant node later, after the turn completed.
	thread.QueueItem(PatchItemMetadata{Target: assistantID, Metadata: map[string]any{"cache/id": "x"}})
	cp, _, err := thread.checkpointAtTurn(assistantID)
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	found := false
	for _, raw := range cp.Snapshot.Items {
		if raw.Type == "assistant_text" && raw.Seq == assistantID {
			found = true
			if raw.Metadata == "" {
				t.Fatal("branch prefix assistant item lost metadata in snapshot")
			}
		}
	}
	if !found {
		t.Fatalf("assistant item with seq %d missing from prefix: %#v", assistantID, cp.Snapshot.Items)
	}
	// Restore the child and confirm the metadata is attached to the preserved node.
	child, err := RestoreCheckpoint(cp, RestoreOptions{})
	if err != nil {
		t.Fatalf("restore child: %v", err)
	}
	node := child.findItemBySeq(assistantID)
	if node == nil {
		t.Fatalf("restored child missing seq %d", assistantID)
	}
	if got := node.Metadata["cache/id"]; got != "x" {
		t.Fatalf("restored child metadata = %#v, want cache/id=x", node.Metadata)
	}
}

func TestTurnManagerOpenRejectsZeroAndAbsentTurnSeq(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryBranchStore()
	stored, err := store.CreateBranch(ctx, BranchCreateOptions{ID: "root"})
	if err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	branch, err := stored.Load(RestoreOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	branch.SetDurableStore(stored.Durable)
	branch.QueueItem(UserText("hello"))
	branch.QueueItem(SendItem{})
	if err := branch.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	manager := NewDefaultBranchManager(store, "test")
	if _, err := manager.Open(ctx, "/branch/root/seq/999", OpenAsEphemeralCopy("bad"), OpenWithoutEventLoop()); !errors.Is(err, ErrInvalidTurn) {
		t.Fatalf("absent turn err = %v, want ErrInvalidTurn", err)
	}
	if _, err := manager.Open(ctx, "/branch/root/seq/0", OpenAsEphemeralCopy("bad"), OpenWithoutEventLoop()); err == nil {
		t.Fatal("zero seq ref should be rejected")
	}
	if _, err := manager.Open(ctx, "/branch/root/turn/1", OpenAsEphemeralCopy("bad"), OpenWithoutEventLoop()); err == nil {
		t.Fatal("old /turn/<index> ref should be rejected")
	}
}

func TestRestoreCheckpointRejectsSeqHeadMismatch(t *testing.T) {
	thread := newThread()
	thread.QueueItem(UserText("a"))
	snap, err := thread.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	cp := Checkpoint{Seq: snap.HeadSeq, Snapshot: snap}
	cp.Seq = snap.HeadSeq + 5
	if _, err := RestoreCheckpoint(cp, RestoreOptions{}); err == nil {
		t.Fatal("expected checkpoint seq/snapshot head mismatch error")
	}
}

func TestStreamedChunkRetainsSeqWhenMetadataPresent(t *testing.T) {
	thread := newThread()
	thread.QueueItem(UserText("u"))
	thread.QueueItem(SendItem{})
	if err := thread.beginStreaming(); err != nil {
		t.Fatal(err)
	}
	if err := thread.appendStreamItem(PatchItemMetadata{Metadata: map[string]any{"k": "v"}}); err != nil {
		t.Fatal(err)
	}
	if err := thread.endStreaming(); err != nil {
		t.Fatal(err)
	}
	// Streamed zero-target patch targeted streamInsertionPoint (the send node).
	n := thread.items.Head().Next // send
	if n == nil || n.Metadata["k"] != "v" {
		t.Fatalf("stream insertion point metadata = %#v, want k=v on send node", n.Metadata)
	}
}

func TestMultipleInitialPatchesMergeLatestWins(t *testing.T) {
	thread := newThread()
	thread.QueueItem(UserText("a"),
		PatchItemMetadata{Metadata: map[string]any{"k": "v1", "x": "1"}},
		PatchItemMetadata{Metadata: map[string]any{"k": "v2"}},
	)
	head := thread.items.Head()
	if got := head.Metadata["k"]; got != "v2" {
		t.Fatalf("key k = %v, want v2", got)
	}
	if got := head.Metadata["x"]; got != "1" {
		t.Fatalf("key x = %v, want 1", got)
	}
	// Initial item plus metadata is one WAL event.
	events := thread.WALAfter(0)
	if len(events) != 1 || events[0].Op != walOpQueueItem {
		t.Fatalf("wal events = %#v, want one queue_item", events)
	}
}

func TestInitialPatchWithNonzeroTargetPanics(t *testing.T) {
	thread := newThread()
	assertPanics(t, func() {
		thread.QueueItem(UserText("a"), PatchItemMetadata{Target: 7, Metadata: map[string]any{"k": "v"}})
	}, "initial patch with nonzero target")
	if len(thread.items.Slice()) != 0 {
		t.Fatalf("failed initial patch created nodes: %#v", thread.items.Slice())
	}
}

func TestStandalonePatchOnlyFirstMayTarget(t *testing.T) {
	thread := newThread()
	thread.QueueItem(UserText("a"))
	thread.QueueItem(UserText("b"))
	seqBefore := thread.Seq()
	// Only the first patch may carry an explicit target; a later nonzero target
	// violates the patch contract and must panic before any mutation.
	assertPanics(t, func() {
		thread.QueueItem(
			PatchItemMetadata{Metadata: map[string]any{"k": "v"}},
			PatchItemMetadata{Target: 1, Metadata: map[string]any{"x": "y"}},
		)
	}, "only the first patch may specify an explicit target")
	if thread.Seq() != seqBefore {
		t.Fatalf("seq advanced on contract-violating patch: %d -> %d", seqBefore, thread.Seq())
	}
	if n := thread.items.Head().Next; n != nil && n.Metadata["x"] == "y" {
		t.Fatalf("contract-violating patch mutated node metadata: %#v", n.Metadata)
	}
}

func TestDeleteValuePatchPersistsAndReplays(t *testing.T) {
	thread := newThread()
	base, err := thread.Checkpoint(CheckpointOptions{Policy: InflightSkip})
	if err != nil {
		t.Fatalf("base checkpoint: %v", err)
	}
	thread.QueueItem(UserText("a"), PatchItemMetadata{Metadata: map[string]any{
		"keep":   "yes",
		"remove": "old",
	}})
	thread.QueueItem(PatchItemMetadata{Metadata: map[string]any{"remove": DeleteValue}})

	head := thread.items.Head()
	if got := head.Metadata["keep"]; got != "yes" {
		t.Fatalf("kept metadata = %#v, want yes", got)
	}
	if _, ok := head.Metadata["remove"]; ok {
		t.Fatalf("deleted metadata remains: %#v", head.Metadata)
	}

	events := thread.WALAfter(base.Seq)
	last := events[len(events)-1]
	if last.Op != walOpPatchItemMetadata || !reflect.DeepEqual(last.DeleteKeys, []string{"remove"}) || last.Metadata != "" {
		t.Fatalf("delete WAL event = %#v", last)
	}
	replayed, err := RestoreFromCheckpointAndWAL(base, events, RestoreOptions{})
	if err != nil {
		t.Fatalf("replay delete patch: %v", err)
	}
	if !reflect.DeepEqual(replayed.items.Head().Metadata, head.Metadata) {
		t.Fatalf("replayed metadata = %#v, want %#v", replayed.items.Head().Metadata, head.Metadata)
	}
}

func TestDeleteValueLatestOperationWins(t *testing.T) {
	thread := newThread()
	thread.QueueItem(UserText("a"), PatchItemMetadata{Metadata: map[string]any{"k": "original"}})

	thread.QueueItem(
		PatchItemMetadata{Metadata: map[string]any{"k": DeleteValue}},
		PatchItemMetadata{Metadata: map[string]any{"k": "restored"}},
	)
	if got := thread.items.Head().Metadata["k"]; got != "restored" {
		t.Fatalf("delete then set = %#v, want restored", got)
	}

	thread.QueueItem(
		PatchItemMetadata{Metadata: map[string]any{"k": "again"}},
		PatchItemMetadata{Metadata: map[string]any{"k": DeleteValue}},
	)
	if thread.items.Head().Metadata != nil {
		t.Fatalf("set then delete metadata = %#v, want nil", thread.items.Head().Metadata)
	}
}

func TestDeleteValueRejectedForInitialMetadata(t *testing.T) {
	thread := newThread()
	assertPanics(t, func() {
		thread.QueueItem(UserText("a"), PatchItemMetadata{Metadata: map[string]any{"k": DeleteValue}})
	}, "DeleteValue in initial metadata")
	if thread.Seq() != 0 || thread.items.Head() != nil || len(thread.wal) != 0 {
		t.Fatalf("rejected initial delete mutated thread: seq=%d items=%#v wal=%#v", thread.Seq(), thread.items.Slice(), thread.wal)
	}
}

func TestStreamPatchDeleteValueTargetsInsertionPoint(t *testing.T) {
	thread := newThread()
	thread.QueueItem(UserText("u"))
	thread.QueueItem(SendItem{}, PatchItemMetadata{Metadata: map[string]any{"temporary": true}})
	if err := thread.beginStreaming(); err != nil {
		t.Fatal(err)
	}
	if err := thread.appendStreamItem(PatchItemMetadata{Metadata: map[string]any{"temporary": DeleteValue}}); err != nil {
		t.Fatal(err)
	}
	if thread.cb.streamInsertionPoint.Metadata != nil {
		t.Fatalf("stream deletion metadata = %#v, want nil", thread.cb.streamInsertionPoint.Metadata)
	}
}

func TestPatchWALReplayMatchesLiveExecution(t *testing.T) {
	thread := newThread()
	base, err := thread.Checkpoint(CheckpointOptions{Policy: InflightSkip})
	if err != nil {
		t.Fatalf("base checkpoint: %v", err)
	}
	thread.QueueItem(UserText("a"))
	thread.QueueItem(UserText("b"))
	thread.QueueItem(PatchItemMetadata{Metadata: map[string]any{"cache/id": "z"}})
	thread.QueueItem(PatchItemMetadata{Target: 1, Metadata: map[string]any{"older": "yes"}})

	// Replay the full WAL from the pre-mutation base.
	replayed, err := RestoreFromCheckpointAndWAL(base, thread.WALAfter(base.Seq), RestoreOptions{})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	before := snapshotThread(thread)
	after := snapshotThread(replayed)
	if len(before.Metadata) != len(after.Metadata) {
		t.Fatalf("metadata count mismatch: %#v vs %#v", before.Metadata, after.Metadata)
	}
	for i := range before.Metadata {
		if !reflect.DeepEqual(before.Seqs[i], after.Seqs[i]) {
			t.Fatalf("seq mismatch at %d: %v vs %v", i, before.Seqs[i], after.Seqs[i])
		}
		if !reflect.DeepEqual(before.Metadata[i], after.Metadata[i]) {
			t.Fatalf("metadata mismatch at %d: %v vs %v", i, before.Metadata[i], after.Metadata[i])
		}
	}
}

func TestPatchItemMetadataForItemAndEmit(t *testing.T) {
	base := PatchItemMetadata{Metadata: map[string]any{"k": "v"}}
	if base.Emit() {
		t.Fatal("PatchItemMetadata.Emit must be false")
	}
	targeted := base.ForItem(5)
	if targeted.Target != 5 || targeted.Metadata["k"] != "v" {
		t.Fatalf("ForItem = %#v", targeted)
	}
	if base.Target != 0 {
		t.Fatal("ForItem mutated the receiver")
	}
}

func TestRequestSnapshotAndReqDeepCloneNestedMetadata(t *testing.T) {
	thread := newThread()
	// Anthropic-style nested metadata.
	thread.QueueItem(UserText("a"), PatchItemMetadata{Metadata: map[string]any{
		"cache/anthropic/cache_control": map[string]any{"type": "ephemeral", "ttl": "1h"},
	}})

	// A request snapshot must be fully decoupled: mutating nested values in the
	// returned Req must not leak back into the thread node metadata.
	req := thread.requestSnapshot()
	if len(req.ItemMeta) != 1 || len(req.Items) != 1 {
		t.Fatalf("request = items %#v meta %#v", req.Items, req.ItemMeta)
	}
	nested, _ := req.ItemMeta[0]["cache/anthropic/cache_control"].(map[string]any)
	nested["ttl"] = "5m"
	head := thread.items.Head()
	got, _ := head.Metadata["cache/anthropic/cache_control"].(map[string]any)
	if got["ttl"] != "1h" {
		t.Fatalf("request mutation leaked into thread node metadata: %#v", head.Metadata)
	}

	// A second snapshot must also be isolated from the first.
	req2 := thread.requestSnapshot()
	nested2, _ := req2.ItemMeta[0]["cache/anthropic/cache_control"].(map[string]any)
	nested2["ttl"] = "2h"
	head2 := thread.items.Head()
	got2, _ := head2.Metadata["cache/anthropic/cache_control"].(map[string]any)
	if got2["ttl"] != "1h" {
		t.Fatalf("second request mutation leaked into thread node metadata: %#v", head2.Metadata)
	}
}

func TestPatchOnEmptyThreadPanicsWithoutMutation(t *testing.T) {
	thread := newThread()
	seqBefore := thread.Seq()
	assertPanics(t, func() {
		thread.QueueItem(PatchItemMetadata{Metadata: map[string]any{"k": "v"}})
	}, "patch with no resolvable target")
	if thread.Seq() != seqBefore {
		t.Fatalf("seq advanced on failed patch: %d -> %d", seqBefore, thread.Seq())
	}
	if len(thread.items.Slice()) != 0 {
		t.Fatalf("failed patch created nodes")
	}
}
