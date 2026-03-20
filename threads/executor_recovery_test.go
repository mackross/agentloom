package threads

import (
	"sync"
	"testing"
)

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

func TestAttachExecutorForRecoveryResumesConstructLLMRequestState(t *testing.T) {
	thread := newTestThread(t)
	thread.QueueItem(AssistantInstruction("tone"))
	thread.QueueItem(UserText("hello"))
	thread.QueueItem(SendItem{})
	if got := thread.State(); got != StateConstructLLMRequest {
		t.Fatalf("expected construct_llm_request before recovery attach, got %q", got)
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

	if err := thread.AttachExecutorForRecovery(NewThreadExecutor(streamer.Streamer())); err != nil {
		t.Fatalf("attach executor for recovery: %v", err)
	}

	streamer.AssertCallCount(t)
	if got := thread.State(); got != StateIdle {
		t.Fatalf("expected idle after recovered request, got %q", got)
	}
}

func TestAttachExecutorForRecoveryRejectsReceivingStreamThread(t *testing.T) {
	thread := New()
	streamStart := make(chan struct{})
	var once sync.Once
	thread.SetDelegate(ThreadDelegateFuncs{
		OnRequest: func(_ *Thread) { once.Do(func() { close(streamStart) }) },
	})
	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
		b.Wait("hold")
		b.Emit(AssistantText("ok"))
	})
	thread.SetExecutor(NewThreadExecutor(streamer.Streamer()))

	done := make(chan struct{})
	go func() {
		thread.QueueItem(UserText("hello"))
		thread.QueueItem(SendItem{})
		close(done)
	}()

	<-streamStart
	cp, err := thread.Checkpoint(CheckpointOptions{Policy: InflightUnsafe})
	if err != nil {
		t.Fatalf("unsafe checkpoint: %v", err)
	}
	restored, err := RestoreCheckpoint(cp, RestoreOptions{AllowUnsafe: true})
	if err != nil {
		t.Fatalf("restore checkpoint: %v", err)
	}

	err = restored.AttachExecutorForRecovery(NewThreadExecutor(newFakeStreamer().Streamer()))
	if err != ErrAttachExecutorForRecoveryRequiresRecoverableState {
		t.Fatalf("expected receiving-stream recovery attach error, got %v", err)
	}
	if restored.executor != nil {
		t.Fatalf("expected executor to remain unset on failed recovery attach, got %#v", restored.executor)
	}

	streamer.Resolve("hold")
	<-done
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
