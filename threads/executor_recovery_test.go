package threads

import (
	"context"
	"encoding/json"
	"testing"
)

type toolResolverFunc func(context.Context, ToolCall, json.RawMessage) (ToolDispatch, error)

func (f toolResolverFunc) ResolveTool(ctx context.Context, call ToolCall, load json.RawMessage) (ToolDispatch, error) {
	return f(ctx, call, load)
}

func runPendingToolDispatch(t *testing.T, dispatch ToolDispatch) (*Thread, Checkpoint) {
	t.Helper()

	thread := New()
	thread.SetToolProvider(staticToolProvider{snap: testToolsSnapshot("calc", "calculate")})
	thread.SetToolResolver(toolResolverFunc(func(context.Context, ToolCall, json.RawMessage) (ToolDispatch, error) {
		return dispatch, nil
	}))

	base, err := thread.Checkpoint(CheckpointOptions{Policy: InflightSkip})
	if err != nil {
		t.Fatalf("base checkpoint: %v", err)
	}

	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
		b.Emit(ToolCall{CallID: "c1", Name: "calc", Payload: `{"a":1}`})
	})
	thread.SetExecutor(NewThreadExecutor(streamer.Streamer()))
	thread.QueueItem(UserText("hello"))
	thread.QueueItem(SendItem{})
	streamer.AssertCallCount(t)

	return thread, base
}

func requirePendingToolCall(t *testing.T, thread *Thread) pendingToolCall {
	t.Helper()

	pending := thread.cb.pendingToolCalls(&thread.items)
	if len(pending) != 1 {
		t.Fatalf("expected one pending tool call, got %#v", pending)
	}
	return pending[0]
}

func TestAttachExecutorForRecoveryInstallsExecutorWhileIdle(t *testing.T) {
	thread := newTestThread(t)
	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
		b.Emit(AssistantText("ok"))
	})

	if err := thread.AttachExecutorForRecovery(NewThreadExecutor(streamer.Streamer())); err != nil {
		t.Fatalf("attach executor for recovery: %v", err)
	}

	thread.QueueItem(UserText("hello"))
	thread.QueueItem(SendItem{})

	streamer.AssertCallCount(t)
}

func TestAttachExecutorForRecoveryResendsConstructLLMRequestAfterCheckpointRestore(t *testing.T) {
	thread := New()
	thread.QueueItem(AssistantInstruction("tone"))
	thread.QueueItem(UserText("hello"))
	thread.QueueItem(SendItem{})
	if got := thread.State(); got != StateConstructLLMRequest {
		t.Fatalf("expected construct_llm_request before checkpoint, got %q", got)
	}

	cp, err := thread.Checkpoint(CheckpointOptions{Policy: InflightUnsafe})
	if err != nil {
		t.Fatalf("checkpoint construct_llm_request: %v", err)
	}
	restored, err := RestoreCheckpoint(cp, RestoreOptions{AllowUnsafe: true})
	if err != nil {
		t.Fatalf("restore checkpoint: %v", err)
	}
	if got := restored.State(); got != StateConstructLLMRequest {
		t.Fatalf("expected restored construct_llm_request, got %q", got)
	}

	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
		b.AssertRequest(func(req Req) {
			if req.Instruction != "tone" {
				t.Fatalf("expected instruction tone, got %#v", req.Instruction)
			}
			if len(req.Items) != 1 || req.Items[0] != UserText("hello") {
				t.Fatalf("unexpected recovered request items: %#v", req.Items)
			}
		})
		b.Emit(AssistantText("ok"))
	})
	if got := streamer.CallCount(); got != 0 {
		t.Fatalf("expected no streamer calls before recovery attach, got %d", got)
	}

	if err := restored.AttachExecutorForRecovery(NewThreadExecutor(streamer.Streamer())); err != nil {
		t.Fatalf("attach executor for recovery: %v", err)
	}

	if got := streamer.CallCount(); got != 1 {
		t.Fatalf("expected recovery attach to resend exactly one request, got %d", got)
	}
	streamer.AssertCallCount(t)
	if got := restored.State(); got != StateIdle {
		t.Fatalf("expected idle after recovered request, got %q", got)
	}
}

func TestAttachExecutorForRecoveryCallsOnThreadRequestOnceForRecoveredRequest(t *testing.T) {
	thread := New()
	thread.QueueItem(UserText("hello"))
	thread.QueueItem(SendItem{})

	cp, err := thread.Checkpoint(CheckpointOptions{Policy: InflightUnsafe})
	if err != nil {
		t.Fatalf("checkpoint construct_llm_request: %v", err)
	}
	restored, err := RestoreCheckpoint(cp, RestoreOptions{AllowUnsafe: true})
	if err != nil {
		t.Fatalf("restore checkpoint: %v", err)
	}

	requestCalls := 0
	restored.SetDelegate(ThreadDelegateFuncs{
		OnRequest: func(_ *Thread) {
			requestCalls++
		},
	})

	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
		b.Emit(AssistantText("ok"))
	})

	if err := restored.AttachExecutorForRecovery(NewThreadExecutor(streamer.Streamer())); err != nil {
		t.Fatalf("attach executor for recovery: %v", err)
	}

	if requestCalls != 1 {
		t.Fatalf("expected OnThreadRequest to be called once during recovery attach, got %d", requestCalls)
	}
	streamer.AssertCallCount(t)
}

func TestAttachExecutorForRecoveryRejectsIdleThreadWithOutstandingStartedToolCall(t *testing.T) {
	thread := newTestThread(t)
	thread.QueueItem(ToolCall{CallID: "c1", Name: "calc", Payload: `{"a":1}`})
	thread.QueueItem(ToolCallStarted{CallID: "c1"})

	err := thread.AttachExecutorForRecovery(NewThreadExecutor(newFakeStreamer().Streamer()))
	if err != ErrAttachExecutorForRecoveryRequiresCleanExactState {
		t.Fatalf("expected started-tool recovery attach error, got %v", err)
	}
	if thread.executor != nil {
		t.Fatalf("expected executor to remain unset on failed recovery attach, got %#v", thread.executor)
	}
}

func TestAttachExecutorForRecoveryAllowsIdleThreadWithRequestedToolCall(t *testing.T) {
	thread := newTestThread(t)
	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
		b.Emit(AssistantText("ok"))
	})
	thread.QueueItem(ToolCall{CallID: "c1", Name: "calc", Payload: `{"a":1}`})

	if err := thread.AttachExecutorForRecovery(NewThreadExecutor(streamer.Streamer())); err != nil {
		t.Fatalf("attach executor for recovery: %v", err)
	}

	thread.QueueItem(UserText("hello"))
	thread.QueueItem(SendItem{})

	streamer.AssertCallCount(t)
}

func TestStartedToolCallPreservesRecoveryMetadataAcrossSnapshotAndRestore(t *testing.T) {
	thread, base := runPendingToolDispatch(t, ToolDispatch{
		Started:  true,
		Recovery: ToolRecoveryUnsafe,
	})

	pending := requirePendingToolCall(t, thread)
	if !pending.started || pending.recovery != ToolRecoveryUnsafe {
		t.Fatalf("unexpected started pending tool call: %#v", pending)
	}

	snap, err := thread.Snapshot()
	if err != nil {
		t.Fatalf("snapshot thread: %v", err)
	}
	roundTripped, err := RestoreThreadSnapshot(snap)
	if err != nil {
		t.Fatalf("restore snapshot: %v", err)
	}
	roundTripPending := requirePendingToolCall(t, roundTripped)
	if !roundTripPending.started || roundTripPending.recovery != ToolRecoveryUnsafe {
		t.Fatalf("unexpected round-tripped pending tool call: %#v", roundTripPending)
	}

	restored, err := RestoreFromCheckpointAndWAL(base, thread.WALAfter(base.Seq), RestoreOptions{})
	if err != nil {
		t.Fatalf("restore from checkpoint + wal: %v", err)
	}
	restoredPending := requirePendingToolCall(t, restored)
	if !restoredPending.started || restoredPending.recovery != ToolRecoveryUnsafe {
		t.Fatalf("unexpected restored pending tool call: %#v", restoredPending)
	}
}

func TestRequestedToolCallHasNoDurableRecoveryClassification(t *testing.T) {
	thread, base := runPendingToolDispatch(t, ToolDispatch{
		Recovery: ToolRecoveryUnsafe,
	})

	pending := requirePendingToolCall(t, thread)
	if pending.started || pending.recovery != "" {
		t.Fatalf("unexpected requested pending tool call: %#v", pending)
	}

	snap, err := thread.Snapshot()
	if err != nil {
		t.Fatalf("snapshot thread: %v", err)
	}
	roundTripped, err := RestoreThreadSnapshot(snap)
	if err != nil {
		t.Fatalf("restore snapshot: %v", err)
	}
	roundTripPending := requirePendingToolCall(t, roundTripped)
	if roundTripPending.started || roundTripPending.recovery != "" {
		t.Fatalf("unexpected round-tripped requested tool call: %#v", roundTripPending)
	}

	restored, err := RestoreFromCheckpointAndWAL(base, thread.WALAfter(base.Seq), RestoreOptions{})
	if err != nil {
		t.Fatalf("restore from checkpoint + wal: %v", err)
	}
	restoredPending := requirePendingToolCall(t, restored)
	if restoredPending.started || restoredPending.recovery != "" {
		t.Fatalf("unexpected restored requested tool call: %#v", restoredPending)
	}
}

func TestThreadExecutorReportsStreamerCapabilities(t *testing.T) {
	streamer := newFakeStreamer()
	streamer.capabilities = StreamerCapabilities{AssistantPrefix: false}

	exec := NewThreadExecutor(streamer.Streamer())

	got := exec.StreamerCapabilities()
	if got.AssistantPrefix {
		t.Fatalf("expected assistant-prefix capability to be false, got %#v", got)
	}
}
