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

func TestAttachExecutorForRecoveryRejectsNonIdleThread(t *testing.T) {
	thread := newTestThread(t)
	thread.QueueItem(UserText("hello"))
	thread.QueueItem(SendItem{})

	err := thread.AttachExecutorForRecovery(NewThreadExecutor(newFakeStreamer().Streamer()))
	if err == nil {
		t.Fatal("expected non-idle recovery attach to fail")
	}
	if thread.executor != nil {
		t.Fatalf("expected executor to remain unset on failed recovery attach, got %#v", thread.executor)
	}
}
