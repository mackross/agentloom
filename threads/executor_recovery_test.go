package threads

import "testing"

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

func TestThreadExecutorReportsStreamerCapabilities(t *testing.T) {
	streamer := newFakeStreamer()
	streamer.capabilities = StreamerCapabilities{AssistantPrefix: false}

	exec := NewThreadExecutor(streamer.Streamer())

	got := exec.StreamerCapabilities()
	if got.AssistantPrefix {
		t.Fatalf("expected assistant-prefix capability to be false, got %#v", got)
	}
}
