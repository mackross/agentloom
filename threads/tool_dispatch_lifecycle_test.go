package threads

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

func toolLifecycleTypes(thread *Thread, callID string) []string {
	out := []string{}
	for _, item := range thread.items.Slice() {
		switch v := item.(type) {
		case ToolCall:
			if v.CallID == callID {
				out = append(out, "tool_call")
			}
		case ToolCallResolving:
			if v.CallID == callID {
				out = append(out, "tool_call_resolving")
			}
		case ToolCallStarted:
			if v.CallID == callID {
				out = append(out, "tool_call_started")
			}
		case ToolCallResultable:
			if v.ToolCallID() == callID {
				out = append(out, "tool_result")
			}
		}
	}
	return out
}

func TestToolResolutionRecordsResolvingBeforeResolverRuns(t *testing.T) {
	thread := New()
	thread.SetToolProvider(staticToolProvider{snap: testToolsSnapshot("calc", "calculate")})
	thread.SetToolResolver(toolResolverFunc(func(context.Context, ToolCall, json.RawMessage) (ToolDispatch, error) {
		// The resolving marker is written before consumer resolver code runs, so a
		// crash/panic inside this function is distinguishable from a call that never
		// entered resolution.
		want := []string{"tool_call", "tool_call_resolving"}
		if got := toolLifecycleTypes(thread, "c1"); !reflect.DeepEqual(got, want) {
			t.Fatalf("unexpected lifecycle at resolver entry: %#v", got)
		}
		return ToolDispatch{
			Started:  true,
			Recovery: ToolRecoverySafe,
			Items:    []Item{ToolCallResult{CallID: "c1", Output: "2"}},
		}, nil
	}))
	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
		b.Emit(ToolCall{CallID: "c1", Name: "calc", Payload: `{"a":1}`})
	})
	thread.SetExecutor(NewThreadExecutor(streamer.Streamer()))

	thread.QueueItem(UserText("hello"))
	thread.QueueItem(SendItem{})

	want := []string{"tool_call", "tool_call_resolving", "tool_call_started", "tool_result"}
	if got := toolLifecycleTypes(thread, "c1"); !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected final lifecycle: %#v", got)
	}
}

func TestToolCallResolvingPreservesAmbiguousStateAcrossSnapshotAndWAL(t *testing.T) {
	thread := New()
	base, err := thread.Checkpoint(CheckpointOptions{Policy: InflightSkip})
	if err != nil {
		t.Fatalf("base checkpoint: %v", err)
	}
	thread.QueueItem(ToolCall{CallID: "c1", Name: "calc", Payload: `{"a":1}`})
	thread.QueueItem(ToolCallResolving{CallID: "c1"})

	pending := requirePendingToolCall(t, thread)
	if !pending.resolving || pending.started {
		t.Fatalf("unexpected pending resolving state: %#v", pending)
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
	if !roundTripPending.resolving || roundTripPending.started {
		t.Fatalf("unexpected round-tripped resolving state: %#v", roundTripPending)
	}

	restored, err := RestoreFromCheckpointAndWAL(base, thread.WALAfter(base.Seq), RestoreOptions{AllowUnsafe: true})
	if err != nil {
		t.Fatalf("restore from checkpoint + wal: %v", err)
	}
	restoredPending := requirePendingToolCall(t, restored)
	if !restoredPending.resolving || restoredPending.started {
		t.Fatalf("unexpected wal-restored resolving state: %#v", restoredPending)
	}
}

func TestAttachExecutorForRecoveryRejectsIdleThreadWithResolvingToolCall(t *testing.T) {
	thread := New()
	thread.QueueItem(ToolCall{CallID: "c1", Name: "calc", Payload: `{"a":1}`})
	thread.QueueItem(ToolCallResolving{CallID: "c1"})

	err := thread.AttachExecutorForRecovery(NewThreadExecutor(newFakeStreamer().Streamer()))
	if err != ErrAttachExecutorForRecoveryRequiresCleanExactState {
		t.Fatalf("expected resolving-tool recovery attach error, got %v", err)
	}
	if thread.executor != nil {
		t.Fatalf("expected executor to remain unset on failed recovery attach, got %#v", thread.executor)
	}
}
