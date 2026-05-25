package threads

import (
	"testing"
	"time"
)

func TestCheckpointSkipUsesSafeBoundaryDuringInflight(t *testing.T) {
	thread := newThread()
	streamStart := make(chan struct{})

	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
		b.Do(func() { close(streamStart) })
		b.Wait("hold")
		b.Emit(AssistantText("world"))
	})
	thread.SetExecutor(NewThreadExecutor(streamer.Streamer()))

	done := make(chan struct{})
	go func() {
		thread.QueueItem(UserText("hello"))
		thread.QueueItem(SendItem{})
		close(done)
	}()

	<-streamStart
	cp, err := thread.Checkpoint(CheckpointOptions{Policy: InflightSkip})
	if err != nil {
		t.Fatalf("skip checkpoint: %v", err)
	}
	if cp.Unsafe {
		t.Fatal("skip checkpoint must be safe")
	}
	restored, err := RestoreCheckpoint(cp, RestoreOptions{})
	if err != nil {
		t.Fatalf("restore checkpoint: %v", err)
	}
	if got := restored.State(); got != StateIdle {
		t.Fatalf("expected restored safe state idle, got %q", got)
	}
	got := restored.items.Slice()
	if len(got) != 1 || got[0] != UserText("hello") {
		t.Fatalf("unexpected restored safe items: %#v", got)
	}

	streamer.Resolve("hold")
	<-done
}

func TestCheckpointWaitReturnsAfterInflightCompletion(t *testing.T) {
	thread := newThread()
	streamStart := make(chan struct{})

	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
		b.Do(func() { close(streamStart) })
		b.Wait("hold")
		b.Emit(AssistantText("world"))
	})
	thread.SetExecutor(NewThreadExecutor(streamer.Streamer()))

	done := make(chan struct{})
	go func() {
		thread.QueueItem(UserText("hello"))
		thread.QueueItem(SendItem{})
		close(done)
	}()

	<-streamStart
	type checkpointResult struct {
		cp  Checkpoint
		err error
	}
	resultCh := make(chan checkpointResult, 1)
	go func() {
		cp, err := thread.Checkpoint(CheckpointOptions{Policy: InflightWait, WaitTimeout: 2 * time.Second})
		resultCh <- checkpointResult{cp: cp, err: err}
	}()

	time.Sleep(15 * time.Millisecond)
	streamer.Resolve("hold")
	<-done

	res := <-resultCh
	if res.err != nil {
		t.Fatalf("wait checkpoint: %v", res.err)
	}
	if res.cp.Unsafe {
		t.Fatal("wait checkpoint must be safe")
	}
	restored, err := RestoreCheckpoint(res.cp, RestoreOptions{})
	if err != nil {
		t.Fatalf("restore checkpoint: %v", err)
	}
	if got := restored.State(); got != StateIdle {
		t.Fatalf("expected restored state idle, got %q", got)
	}
	all := restored.items.Slice()
	if len(all) < 3 {
		t.Fatalf("expected restored items to include streamed response, got %#v", all)
	}
}

func TestCheckpointUnsafeRestoreRequiresExplicitOptIn(t *testing.T) {
	thread := newThread()
	streamStart := make(chan struct{})

	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
		b.Do(func() { close(streamStart) })
		b.Wait("hold")
		b.Emit(AssistantText("world"))
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
	if !cp.Unsafe {
		t.Fatal("expected unsafe checkpoint during inflight")
	}
	if _, err := RestoreCheckpoint(cp, RestoreOptions{}); err == nil {
		t.Fatal("expected restore rejection for unsafe checkpoint without opt-in")
	}
	restored, err := RestoreCheckpoint(cp, RestoreOptions{AllowUnsafe: true})
	if err != nil {
		t.Fatalf("restore unsafe checkpoint with opt-in: %v", err)
	}
	if got := restored.State(); got != StateReceivingStream {
		t.Fatalf("expected unsafe restore state receiving_stream, got %q", got)
	}

	streamer.Resolve("hold")
	<-done
}

func TestCheckpointSkipPreservesAwaitingToolResults(t *testing.T) {
	thread := newThread()
	streamer := newFakeStreamer()
	streamer.capabilities.ToolResultSendPolicy = ToolResultSendRequiresComplete
	streamer.Reply(func(b *streamBuilder) {
		b.Emit(ToolCall{CallID: "c1", Name: "calc", Payload: `{"a":1}`})
	})
	thread.SetExecutor(NewThreadExecutor(streamer.Streamer()))
	thread.QueueItem(UserText("hello"))
	thread.QueueItem(SendItem{})

	cp, err := thread.Checkpoint(CheckpointOptions{Policy: InflightSkip})
	if err != nil {
		t.Fatalf("checkpoint awaiting: %v", err)
	}
	restored, err := RestoreCheckpoint(cp, RestoreOptions{})
	if err != nil {
		t.Fatalf("restore awaiting checkpoint: %v", err)
	}
	if got := restored.State(); got != StateAwaitingToolResults {
		t.Fatalf("restored state = %q, want %q", got, StateAwaitingToolResults)
	}
}
