package threads

import (
	"testing"
)

func TestRecoveryViewExposesOutstandingToolCallMetadata(t *testing.T) {
	thread := newTestThread(t)
	thread.QueueItem(ToolsSnapshot{Handlers: []ToolHandlerBinding{{
		Name:            "calc",
		HandlerLoadData: []byte(`{"snapshot":"bound"}`),
	}}})
	thread.QueueItem(ToolCall{CallID: "c1", Name: "missing", Payload: `{"a":1}`})
	thread.QueueItem(ToolCall{CallID: "c2", Name: "calc", Payload: `{"a":2}`})
	thread.QueueItem(ToolCallStarted{
		CallID:   "c2",
		Continue: ToolContinueManual,
		Recovery: ToolRecoveryUnsafe,
	})

	got := thread.RecoveryView()
	if got.State != StateIdle {
		t.Fatalf("expected idle recovery view, got %q", got.State)
	}
	if got.ExactRecoveryRequires.AssistantPrefix {
		t.Fatalf("expected idle thread not to require assistant-prefix continuation, got %#v", got.ExactRecoveryRequires)
	}
	if len(got.OutstandingToolCalls) != 2 {
		t.Fatalf("expected two outstanding tool calls, got %#v", got.OutstandingToolCalls)
	}

	if got.OutstandingToolCalls[0].Call.CallID != "c1" ||
		got.OutstandingToolCalls[0].Bound ||
		got.OutstandingToolCalls[0].Started ||
		got.OutstandingToolCalls[0].Continue != ToolContinueAuto ||
		got.OutstandingToolCalls[0].Recovery != "" ||
		len(got.OutstandingToolCalls[0].HandlerLoadData) != 0 {
		t.Fatalf("unexpected requested outstanding tool call: %#v", got.OutstandingToolCalls[0])
	}

	if got.OutstandingToolCalls[1].Call.CallID != "c2" ||
		!got.OutstandingToolCalls[1].Bound ||
		!got.OutstandingToolCalls[1].Started ||
		got.OutstandingToolCalls[1].Continue != ToolContinueManual ||
		got.OutstandingToolCalls[1].Recovery != ToolRecoveryUnsafe ||
		string(got.OutstandingToolCalls[1].HandlerLoadData) != `{"snapshot":"bound"}` {
		t.Fatalf("unexpected started outstanding tool call: %#v", got.OutstandingToolCalls[1])
	}

	got.OutstandingToolCalls[1].HandlerLoadData[0] = 'X'
	again := thread.RecoveryView()
	if string(again.OutstandingToolCalls[1].HandlerLoadData) != `{"snapshot":"bound"}` {
		t.Fatalf("expected cloned handler load data, got %#v", again.OutstandingToolCalls[1].HandlerLoadData)
	}
}

func TestRecoveryViewConstructRequestDoesNotRequireAssistantPrefix(t *testing.T) {
	thread := New()
	thread.QueueItem(UserText("hello"))
	thread.QueueItem(SendItem{})

	got := thread.RecoveryView()
	if got.State != StateConstructLLMRequest {
		t.Fatalf("expected construct_llm_request, got %q", got.State)
	}
	if got.ExactRecoveryRequires.AssistantPrefix {
		t.Fatalf("expected construct_llm_request not to require assistant-prefix continuation, got %#v", got.ExactRecoveryRequires)
	}
	if !got.CanRecoverExactlyWith(StreamerCapabilities{}) {
		t.Fatalf("expected construct_llm_request exact recovery without assistant-prefix support, got %#v", got)
	}
}

func TestRecoveryViewReceivingStreamRequiresAssistantPrefix(t *testing.T) {
	thread := New()
	streamStart := make(chan struct{})
	thread.SetDelegate(ThreadDelegateFuncs{
		OnRequest: func(_ *Thread) {
			select {
			case <-streamStart:
			default:
				close(streamStart)
			}
		},
	})

	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
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
	restored, err := RestoreCheckpoint(cp, RestoreOptions{AllowUnsafe: true})
	if err != nil {
		t.Fatalf("restore unsafe checkpoint: %v", err)
	}

	got := restored.RecoveryView()
	if got.State != StateReceivingStream {
		t.Fatalf("expected receiving_stream recovery view, got %q", got.State)
	}
	if !got.ExactRecoveryRequires.AssistantPrefix {
		t.Fatalf("expected receiving_stream to require assistant-prefix continuation, got %#v", got.ExactRecoveryRequires)
	}
	if got.CanRecoverExactlyWith(StreamerCapabilities{}) {
		t.Fatalf("expected exact recovery without assistant-prefix support to be rejected, got %#v", got)
	}
	if !got.CanRecoverExactlyWith(StreamerCapabilities{AssistantPrefix: true}) {
		t.Fatalf("expected exact recovery with assistant-prefix support, got %#v", got)
	}

	streamer.Resolve("hold")
	<-done
}
