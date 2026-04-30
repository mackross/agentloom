package threads

import (
	"reflect"
	"testing"

	gschema "github.com/google/jsonschema-go/jsonschema"
)

func TestNewHeadStartsEmpty(t *testing.T) {
	thread := newTestThread(t)
	if thread == nil {
		t.Fatal("New returned nil")
	}

	if got := thread.IP(); got != nil {
		t.Fatalf("expected nil IP item, got %#v", got)
	}
	if got := thread.State(); got != StateIdle {
		t.Fatalf("expected idle state, got %q", got)
	}

	thread.QueueItem(UserText("afljafljaflafj"))

	if got := thread.IP(); got == nil {
		t.Fatal("expected non-nil IP item after queue")
	} else if got.Item != UserText("afljafljaflafj") {
		t.Fatalf("unexpected IP item after queue: %#v", got.Item)
	}
	if got := thread.State(); got != StateIdle {
		t.Fatalf("expected idle state after queue, got %q", got)
	}

	queued := thread.Queued()
	if queued.Head() != nil {
		t.Fatalf("expected empty queued head after advance, got %#v", queued.Head())
	}
	slice := queued.Slice()
	if len(slice) != 0 {
		t.Fatalf("expected no queued items, got %#v", slice)
	}

	count := 0
	for i, item := range queued.Iter2() {
		if i != 0 {
			t.Fatalf("unexpected iterator index: %d", i)
		}
		if item != UserText("afljafljaflafj") {
			t.Fatalf("unexpected iterator item: %#v", item)
		}
		count++
	}
	if count != 0 {
		t.Fatalf("expected zero iterated items, got %d", count)
	}
}

func TestConsecutiveUserTextCoalescesAsIPAdvances(t *testing.T) {
	thread := newTestThread(t)
	thread.QueueItem(UserText("hello"))
	thread.QueueItem(UserText("world"))

	queued := thread.Queued().Slice()
	if len(queued) != 0 {
		t.Fatalf("expected queued to drain after advance, got %#v", queued)
	}
	ip := thread.IP()
	if ip == nil {
		t.Fatal("expected IP after coalescing")
	}
	if got := ip.Item; got != UserText("helloworld") {
		t.Fatalf("unexpected coalesced IP item: %#v", got)
	}
}

func TestConsecutiveAssistantTextCoalescesAsIPAdvances(t *testing.T) {
	thread := newTestThread(t)
	thread.QueueItem(AssistantText("a"))
	thread.QueueItem(AssistantText("b"))

	queued := thread.Queued().Slice()
	if len(queued) != 0 {
		t.Fatalf("expected queued to drain after advance, got %#v", queued)
	}
	ip := thread.IP()
	if ip == nil {
		t.Fatal("expected IP after coalescing")
	}
	if got := ip.Item; got != AssistantText("ab") {
		t.Fatalf("unexpected coalesced IP item: %#v", got)
	}
}

func TestRequestCoalescesConsecutiveUserTextWhenIPAdvances(t *testing.T) {
	thread := newTestThread(t)
	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
		b.AssertRequest(func(req Req) {
			if req.Instruction != "" {
				t.Fatalf("expected empty instruction, got %#v", req.Instruction)
			}
			if len(req.Items) != 1 {
				t.Fatalf("expected one coalesced request item, got %d", len(req.Items))
			}
			if got := req.Items[0]; got != UserText("helloworld") {
				t.Fatalf("unexpected coalesced request item: %#v", got)
			}
		})
		b.Emit(AssistantText("ok"))
	})
	exec := NewThreadExecutor(streamer.Streamer())
	thread.SetExecutor(exec)

	thread.QueueItem(UserText("hello"))
	thread.QueueItem(UserText("world"))
	thread.QueueItem(SendItem{})

	streamer.AssertCallCount(t)
}

func TestQueuedDuringInFlightRequestDrainsAfterStreamReturnsToIdle(t *testing.T) {
	thread := newTestThread(t)
	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
		b.Do(func() { thread.QueueItem(UserText("later")) })
		b.Emit(AssistantText("world"))
	})
	exec := NewThreadExecutor(streamer.Streamer())
	thread.SetExecutor(exec)

	thread.QueueItem(UserText("hello"))
	thread.QueueItem(SendItem{})

	streamer.AssertCallCount(t)

	queued := thread.Queued().Slice()
	if len(queued) != 0 {
		t.Fatalf("expected queued to drain after idle advance, got %#v", queued)
	}
}

func TestRequestInputExcludesItemQueuedDuringStream(t *testing.T) {
	thread := newTestThread(t)
	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
		b.AssertRequest(func(req Req) {
			if len(req.Items) != 1 {
				t.Fatalf("expected one request item, got %d", len(req.Items))
			}
			if got := req.Items[0]; got != UserText("hello") {
				t.Fatalf("unexpected request item: %#v", got)
			}
		})
		b.Do(func() { thread.QueueItem(UserText("later")) })
		b.Emit(AssistantText("world"))
	})
	exec := NewThreadExecutor(streamer.Streamer())
	thread.SetExecutor(exec)

	thread.QueueItem(UserText("hello"))
	thread.QueueItem(SendItem{})

	requests := streamer.Requests()
	if len(requests) != 1 {
		t.Fatalf("expected one request, got %d", len(requests))
	}
}

func TestAssistantInstructionLastWinsInConstructedRequest(t *testing.T) {
	thread := newTestThread(t)
	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
		b.AssertRequest(func(req Req) {
			if req.Instruction != "second" {
				t.Fatalf("expected instruction to be last assistant instruction, got %#v", req.Instruction)
			}
			if len(req.Items) != 1 {
				t.Fatalf("expected one request item, got %d", len(req.Items))
			}
			if got := req.Items[0]; got != UserText("hello") {
				t.Fatalf("unexpected request item: %#v", got)
			}
		})
		b.Emit(AssistantText("ok"))
	})
	exec := NewThreadExecutor(streamer.Streamer())
	thread.SetExecutor(exec)

	thread.QueueItem(AssistantInstruction("first"))
	thread.QueueItem(UserText("hello"))
	thread.QueueItem(AssistantInstruction("second"))
	thread.QueueItem(SendItem{})

	streamer.AssertCallCount(t)
}

func TestUserTextCoalescesAcrossAssistantInstructionInRequest(t *testing.T) {
	thread := newTestThread(t)
	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
		b.AssertRequest(func(req Req) {
			if req.Instruction != "tone" {
				t.Fatalf("expected instruction tone, got %#v", req.Instruction)
			}
			if len(req.Items) != 1 {
				t.Fatalf("expected one coalesced request item, got %d", len(req.Items))
			}
			if got := req.Items[0]; got != UserText("helloworld") {
				t.Fatalf("unexpected coalesced request item: %#v", got)
			}
		})
		b.Emit(AssistantText("ok"))
	})
	exec := NewThreadExecutor(streamer.Streamer())
	thread.SetExecutor(exec)

	thread.QueueItem(UserText("hello"))
	thread.QueueItem(AssistantInstruction("tone"))
	thread.QueueItem(UserText("world"))
	thread.QueueItem(SendItem{})

	streamer.AssertCallCount(t)
}

func TestUserTextDoesNotCoalesceAcrossAssistantContentAfterInstruction(t *testing.T) {
	thread := newTestThread(t)
	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
		b.AssertRequest(func(req Req) {
			if req.Instruction != "tone" {
				t.Fatalf("expected instruction tone, got %#v", req.Instruction)
			}
			if len(req.Items) != 3 {
				t.Fatalf("expected three request items, got %d", len(req.Items))
			}
			if got := req.Items[0]; got != UserText("hello") {
				t.Fatalf("unexpected first request item: %#v", got)
			}
			if got := req.Items[1]; got != AssistantText("mid") {
				t.Fatalf("unexpected second request item: %#v", got)
			}
			if got := req.Items[2]; got != UserText("world") {
				t.Fatalf("unexpected third request item: %#v", got)
			}
		})
		b.Emit(AssistantText("ok"))
	})
	exec := NewThreadExecutor(streamer.Streamer())
	thread.SetExecutor(exec)

	thread.QueueItem(UserText("hello"))
	thread.QueueItem(AssistantInstruction("tone"))
	thread.QueueItem(AssistantText("mid"))
	thread.QueueItem(UserText("world"))
	thread.QueueItem(SendItem{})

	streamer.AssertCallCount(t)
}

func TestToolSnapshotLastWinsInConstructedRequest(t *testing.T) {
	thread := newTestThread(t)
	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
		b.AssertRequest(func(req Req) {
			if len(req.Items) != 1 {
				t.Fatalf("expected one request item, got %d", len(req.Items))
			}
			if got := req.Items[0]; got != UserText("hello") {
				t.Fatalf("unexpected request item: %#v", got)
			}
			want := testToolsSnapshot("new", "new desc").Snapshot
			if !reflect.DeepEqual(req.Tools, want) {
				t.Fatalf("unexpected tool snapshot: %#v", req.Tools)
			}
		})
		b.Emit(AssistantText("ok"))
	})
	thread.SetExecutor(NewThreadExecutor(streamer.Streamer()))

	thread.QueueItem(testToolsSnapshot("old", "old desc"))
	thread.QueueItem(UserText("hello"))
	thread.QueueItem(testToolsSnapshot("new", "new desc"))
	thread.QueueItem(SendItem{})

	streamer.AssertCallCount(t)
}

func TestToolSnapshotRequestPreservesExplicitEmptyAllowed(t *testing.T) {
	thread := newTestThread(t)
	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
		b.AssertRequest(func(req Req) {
			if req.Tools.Allowed == nil || len(req.Tools.Allowed) != 0 {
				t.Fatalf("expected explicit empty allowed, got %#v", req.Tools.Allowed)
			}
		})
		b.Emit(AssistantText("ok"))
	})
	thread.SetExecutor(NewThreadExecutor(streamer.Streamer()))

	snap := testToolsSnapshot("calc", "calculate").Snapshot
	snap.Allowed = []string{}
	thread.QueueItem(ToolsSnapshot{Snapshot: snap})
	thread.QueueItem(UserText("hello"))
	thread.QueueItem(SendItem{})

	streamer.AssertCallCount(t)
}

type staticToolProvider struct {
	snap ToolsSnapshot
}

func (p staticToolProvider) ToolsSnapshot() ToolsSnapshot { return p.snap }

func TestSetToolProviderQueuesToolOfferSnapshot(t *testing.T) {
	thread := newTestThread(t)
	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
		b.AssertRequest(func(req Req) {
			want := ToolOfferSnapshot{Offered: []ToolSpec{{
				Name:        "calc",
				Description: "calculate",
				Payload:     ToolPayloadJSONSchema(gschema.Schema{Type: "object"}),
			}}}
			if !reflect.DeepEqual(req.Tools, want) {
				t.Fatalf("unexpected tool snapshot: %#v", req.Tools)
			}
		})
		b.Emit(AssistantText("ok"))
	})
	thread.SetExecutor(NewThreadExecutor(streamer.Streamer()))

	thread.SetToolProvider(staticToolProvider{snap: ToolsSnapshot{Snapshot: ToolOfferSnapshot{Offered: []ToolSpec{{
		Name:        "calc",
		Description: "calculate",
		Payload:     ToolPayloadJSONSchema(gschema.Schema{Type: "object"}),
	}}}}})
	thread.QueueItem(UserText("hello"))
	thread.QueueItem(SendItem{})

	streamer.AssertCallCount(t)
}

func TestTwoStreamChunksStayBeforeMidStreamQueuedItem(t *testing.T) {
	thread := newTestThread(t)

	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
		b.Do(func() {
			if got := thread.Queued().Slice(); len(got) != 0 {
				t.Fatalf("expected no queued items, got %#v", got)
			}
			thread.QueueItem(UserText("later"))
			if got := thread.Queued().Slice(); len(got) != 1 {
				t.Fatalf("expected one queued item, got %#v", got)
			}
		})
		b.Emit(AssistantText("w1"))
		b.Do(func() {
			thread.QueueItem(UserText("even later"))
			if got := thread.Queued().Slice(); len(got) != 2 {
				t.Fatalf("expected two queued items, got %#v", got)
			}
		})
		b.Emit(AssistantText("w2"))
	})
	exec := NewThreadExecutor(streamer.Streamer())
	thread.SetExecutor(exec)

	thread.QueueItem(UserText("hello"))
	thread.QueueItem(SendItem{})

	queued := thread.Queued().Slice()
	if len(queued) != 0 {
		t.Fatalf("expected queued to drain after idle advance, got %#v", queued)
	}
}

func TestStateIsReceivingDuringStreamAndCompleteAfter(t *testing.T) {
	thread := newTestThread(t)
	stateDuring := State("unknown")
	ipDuring := (*item[Item])(nil)
	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
		b.Do(func() {
			stateDuring = thread.State()
			ipDuring = thread.IP()
		})
		b.Emit(AssistantText("world"))
	})
	exec := NewThreadExecutor(streamer.Streamer())
	thread.SetExecutor(exec)

	thread.QueueItem(UserText("hello"))
	thread.QueueItem(SendItem{})

	if stateDuring != State("receiving_stream") {
		t.Fatalf("expected receiving_stream during stream, got %q", stateDuring)
	}
	if ipDuring == nil {
		t.Fatal("expected IP unchanged during stream")
	}
	if got := thread.State(); got != StateIdle {
		t.Fatalf("expected idle after stream, got %q", got)
	}
}

func TestDelegateNotifiedWhenThreadTransitionsToIdle(t *testing.T) {
	thread := newTestThread(t)
	idleCalls := 0
	thread.SetDelegate(ThreadDelegateFunc(func(_ *Thread) {
		idleCalls++
	}))

	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
		b.Emit(AssistantText("world"))
	})
	thread.SetExecutor(NewThreadExecutor(streamer.Streamer()))

	thread.QueueItem(UserText("hello"))
	thread.QueueItem(SendItem{})

	if idleCalls != 1 {
		t.Fatalf("expected one idle delegate call, got %d", idleCalls)
	}
}

func TestDelegateNotifiedWhenThreadTransitionsToRequestThenIdle(t *testing.T) {
	thread := newTestThread(t)
	events := make([]string, 0, 2)
	thread.SetDelegate(ThreadDelegateFuncs{
		OnRequest: func(_ *Thread) { events = append(events, "request") },
		OnIdle:    func(_ *Thread) { events = append(events, "idle") },
	})

	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
		b.Emit(AssistantText("world"))
	})
	thread.SetExecutor(NewThreadExecutor(streamer.Streamer()))

	thread.QueueItem(UserText("hello"))
	thread.QueueItem(SendItem{})

	if len(events) != 2 {
		t.Fatalf("expected two delegate events, got %#v", events)
	}
	if events[0] != "request" || events[1] != "idle" {
		t.Fatalf("unexpected delegate event order: %#v", events)
	}
}

func TestDelegateReceivesEachStreamItemAppended(t *testing.T) {
	thread := newTestThread(t)
	appended := make([]Item, 0, 2)
	thread.SetDelegate(ThreadDelegateFuncs{
		OnStreamItemAppended: func(_ *Thread, item Item) {
			appended = append(appended, item)
		},
	})

	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
		b.Emit(AssistantText("w1"))
		b.Emit(AssistantText("w2"))
	})
	thread.SetExecutor(NewThreadExecutor(streamer.Streamer()))

	thread.QueueItem(UserText("hello"))
	thread.QueueItem(SendItem{})

	if len(appended) != 2 {
		t.Fatalf("expected two appended stream items, got %#v", appended)
	}
	if appended[0] != AssistantText("w1") || appended[1] != AssistantText("w2") {
		t.Fatalf("unexpected appended stream items: %#v", appended)
	}
}

func TestSendItemQueuedInIdleAutoConstructsAndStreams(t *testing.T) {
	thread := newTestThread(t)
	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
		b.Emit(AssistantText("world"))
	})
	exec := NewThreadExecutor(streamer.Streamer())
	thread.SetExecutor(exec)

	thread.QueueItem(UserText("hello"))
	thread.QueueItem(SendItem{})

	streamer.AssertCallCount(t)
	queued := thread.Queued().Slice()
	if len(queued) != 0 {
		t.Fatalf("expected no queued items after send, got %#v", queued)
	}
}

func TestSendItemExcludedFromConstructedRequest(t *testing.T) {
	thread := newTestThread(t)

	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
		b.AssertRequest(func(req Req) {
			if len(req.Items) != 1 {
				t.Fatalf("expected one request item, got %d", len(req.Items))
			}
			if got := req.Items[0]; got != UserText("hello") {
				t.Fatalf("unexpected request item: %#v", got)
			}
		})
		b.Emit(AssistantText("world"))
	})
	exec := NewThreadExecutor(streamer.Streamer())
	thread.SetExecutor(exec)

	thread.QueueItem(UserText("hello"))
	thread.QueueItem(SendItem{})

	streamer.AssertCallCount(t)
	requests := streamer.Requests()
	if len(requests) != 1 {
		t.Fatalf("expected one request, got %d", len(requests))
	}
}

func TestStreamItemsAreBeforeMidStreamQueuedItems(t *testing.T) {
	thread := newTestThread(t)
	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
		b.Do(func() { thread.QueueItem(UserText("later")) })
		b.Emit(AssistantText("w1"))
		b.Emit(AssistantText("w2"))
	})
	exec := NewThreadExecutor(streamer.Streamer())
	thread.SetExecutor(exec)

	thread.QueueItem(UserText("hello"))
	thread.QueueItem(SendItem{})

	all := thread.items.Slice()
	if len(all) != 4 {
		t.Fatalf("expected four items, got %d", len(all))
	}
	if got := all[0]; got != UserText("hello") {
		t.Fatalf("unexpected first item: %#v", got)
	}
	if _, ok := all[1].(SendItem); !ok {
		t.Fatalf("unexpected second item: %#v", all[1])
	}
	if got := all[2]; got != AssistantText("w1w2") {
		t.Fatalf("unexpected third item: %#v", got)
	}
	if got := all[3]; got != UserText("later") {
		t.Fatalf("unexpected fourth item: %#v", got)
	}
}

func TestPendingSendItemRunsWhenTransitioningToIdle(t *testing.T) {
	thread := newTestThread(t)
	streamer := newFakeStreamer().
		Reply(func(b *streamBuilder) {
			b.Do(func() {
				thread.QueueItem(UserText("u2"))
				thread.QueueItem(SendItem{})
			})
			b.Emit(AssistantText("a1"))
		}).
		Reply(func(b *streamBuilder) {
			b.Emit(AssistantText("a2"))
		})
	exec := NewThreadExecutor(streamer.Streamer())
	thread.SetExecutor(exec)

	thread.QueueItem(UserText("u1"))
	thread.QueueItem(SendItem{})

	streamer.AssertCallCount(t)
}

func TestQueuedExcludesStreamItemsWhileReceivingStream(t *testing.T) {
	thread := newTestThread(t)
	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
		b.Do(func() { thread.QueueItem(UserText("later")) })
		b.Emit(AssistantText("world"))
		b.Do(func() {
			if got := thread.State(); got != StateReceivingStream {
				t.Fatalf("expected receiving_stream during callback, got %q", got)
			}
			queued := thread.Queued().Slice()
			if len(queued) != 1 {
				t.Fatalf("expected one queued item, got %#v", queued)
			}
			if got := queued[0]; got != UserText("later") {
				t.Fatalf("expected queued to contain only later user item, got %#v", got)
			}
		})
	})
	exec := NewThreadExecutor(streamer.Streamer())
	thread.SetExecutor(exec)

	thread.QueueItem(UserText("hello"))
	thread.QueueItem(SendItem{})
}

func TestIPTracksLatestStreamChunkWithoutEnteringQueue(t *testing.T) {
	thread := newTestThread(t)
	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
		b.Do(func() {
			thread.QueueItem(UserText("later"))
		})
		b.Emit(AssistantText("w1"))
		b.Do(func() {
			ip := thread.IP()
			if ip == nil || ip.Item != AssistantText("w1") {
				t.Fatalf("expected IP at first stream chunk, got %#v", ip)
			}
			queued := thread.Queued().Slice()
			if len(queued) != 1 || queued[0] != UserText("later") {
				t.Fatalf("expected queued to remain [later], got %#v", queued)
			}
		})
		b.Emit(AssistantText("w2"))
		b.Do(func() {
			ip := thread.IP()
			if ip == nil || ip.Item != AssistantText("w1w2") {
				t.Fatalf("expected IP at second stream chunk, got %#v", ip)
			}
			queued := thread.Queued().Slice()
			if len(queued) != 1 || queued[0] != UserText("later") {
				t.Fatalf("expected queued to remain [later], got %#v", queued)
			}
		})
	})
	exec := NewThreadExecutor(streamer.Streamer())
	thread.SetExecutor(exec)

	thread.QueueItem(UserText("hello"))
	thread.QueueItem(SendItem{})
}

func TestToolCallChunksCoalesceAndFinalizeInThreadItems(t *testing.T) {
	thread := newTestThread(t)
	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
		b.Emit(ToolCallChunk{CallID: "c1", Name: "calc", PayloadDelta: `{"a":`})
		b.Emit(ToolCallChunk{CallID: "c1", PayloadDelta: `1}`})
		b.Emit(ToolCall{CallID: "c1", Name: "calc", Payload: `{"a":1}`})
	})
	thread.SetExecutor(NewThreadExecutor(streamer.Streamer()))

	thread.QueueItem(UserText("hello"))
	thread.QueueItem(SendItem{})

	all := thread.items.Slice()
	if len(all) != 3 {
		t.Fatalf("expected three items, got %#v", all)
	}
	if got := all[2]; got != (ToolCall{CallID: "c1", Name: "calc", Payload: `{"a":1}`}) {
		t.Fatalf("unexpected finalized tool call: %#v", got)
	}
}

func TestPendingToolCallsTreatResultWithoutStartAsCompleted(t *testing.T) {
	thread := newTestThread(t)
	thread.QueueItem(ToolsSnapshot{Handlers: []ToolHandlerBinding{{Name: "calc", HandlerLoadData: []byte(`{"answer":"3"}`)}}})
	thread.QueueItem(ToolCall{CallID: "c1", Name: "calc", Payload: `{"a":1}`})
	thread.QueueItem(ToolCall{CallID: "c2", Name: "calc", Payload: `{"a":2}`})
	thread.QueueItem(ToolCallStarted{CallID: "c2"})
	thread.QueueItem(ToolCall{CallID: "c3", Name: "calc", Payload: `{"a":3}`})
	thread.QueueItem(ToolCallResult{CallID: "c3", Output: "3"})

	got := thread.cb.pendingToolCalls(&thread.items)
	if len(got) != 2 {
		t.Fatalf("expected two pending tool calls, got %#v", got)
	}
	if got[0].call.CallID != "c1" || got[0].started {
		t.Fatalf("unexpected requested tool call state: %#v", got[0])
	}
	if got[1].call.CallID != "c2" || !got[1].started {
		t.Fatalf("unexpected started tool call state: %#v", got[1])
	}
}

func TestPendingToolCallsUseNearestSnapshotAndCloneLoadData(t *testing.T) {
	thread := newTestThread(t)
	thread.QueueItem(ToolsSnapshot{Handlers: []ToolHandlerBinding{
		{Name: "calc", HandlerLoadData: []byte(`{"snapshot":"old"}`)},
	}})
	thread.QueueItem(ToolCall{CallID: "c1", Name: "missing", Payload: `{"a":1}`})
	thread.QueueItem(ToolCall{CallID: "c2", Name: "calc", Payload: `{"a":2}`})
	thread.QueueItem(ToolsSnapshot{Handlers: []ToolHandlerBinding{
		{Name: "calc", HandlerLoadData: []byte(`{"snapshot":"new"}`)},
	}})
	thread.QueueItem(ToolCall{CallID: "c3", Name: "calc", Payload: `{"a":3}`})

	got := thread.cb.pendingToolCalls(&thread.items)
	if len(got) != 3 {
		t.Fatalf("expected three pending tool calls, got %#v", got)
	}
	if got[0].call.CallID != "c1" || got[0].bound || got[0].started || len(got[0].load) != 0 {
		t.Fatalf("unexpected unbound pending tool call: %#v", got[0])
	}
	if got[1].call.CallID != "c2" || !got[1].bound || string(got[1].load) != `{"snapshot":"old"}` {
		t.Fatalf("unexpected old snapshot pending tool call: %#v", got[1])
	}
	if got[2].call.CallID != "c3" || !got[2].bound || string(got[2].load) != `{"snapshot":"new"}` {
		t.Fatalf("unexpected new snapshot pending tool call: %#v", got[2])
	}

	got[1].load[0] = 'X'
	again := thread.cb.pendingToolCalls(&thread.items)
	if string(again[1].load) != `{"snapshot":"old"}` {
		t.Fatalf("expected cloned load data, got %#v", again[1].load)
	}
}

func TestInterleavedToolCallChunksCoalesceByCallID(t *testing.T) {
	thread := newTestThread(t)
	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
		b.Emit(ToolCallChunk{CallID: "c1", Name: "first", PayloadDelta: `{"x":`})
		b.Emit(ToolCallChunk{CallID: "c2", Name: "second", PayloadDelta: `{"y":`})
		b.Emit(ToolCallChunk{CallID: "c1", PayloadDelta: `1}`})
		b.Emit(ToolCallChunk{CallID: "c2", PayloadDelta: `2}`})
		b.Emit(ToolCall{CallID: "c2", Name: "second", Payload: `{"y":2}`})
		b.Emit(ToolCall{CallID: "c1", Name: "first", Payload: `{"x":1}`})
	})
	thread.SetExecutor(NewThreadExecutor(streamer.Streamer()))

	thread.QueueItem(UserText("hello"))
	thread.QueueItem(SendItem{})

	all := thread.items.Slice()
	if len(all) != 4 {
		t.Fatalf("expected four items, got %#v", all)
	}
	if got := all[2]; got != (ToolCall{CallID: "c1", Name: "first", Payload: `{"x":1}`}) {
		t.Fatalf("unexpected first tool call: %#v", got)
	}
	if got := all[3]; got != (ToolCall{CallID: "c2", Name: "second", Payload: `{"y":2}`}) {
		t.Fatalf("unexpected second tool call: %#v", got)
	}
}

func TestToolResultSendPolicyGating(t *testing.T) {
	tests := []struct {
		name        string
		policy      ToolResultSendPolicy
		calls       []ToolCall
		results     []Item
		wantSend    bool
		wantState   State
		wantItems   []Item
		resultFirst bool
	}{
		{
			name:      "strict waits for missing single result",
			policy:    ToolResultSendRequiresComplete,
			calls:     []ToolCall{{CallID: "c1", Name: "calc", Payload: `{"a":1}`}},
			wantState: StateAwaitingToolResults,
		},
		{
			name:      "strict sends after single result",
			policy:    ToolResultSendRequiresComplete,
			calls:     []ToolCall{{CallID: "c1", Name: "calc", Payload: `{"a":1}`}},
			results:   []Item{ToolCallResult{CallID: "c1", Output: "1"}},
			wantSend:  true,
			wantState: StateIdle,
			wantItems: []Item{UserText("hello"), ToolCall{CallID: "c1", Name: "calc", Payload: `{"a":1}`}, ToolCallResult{CallID: "c1", Output: "1"}},
		},
		{
			name:   "strict waits for all parallel results",
			policy: ToolResultSendRequiresComplete,
			calls: []ToolCall{
				{CallID: "c1", Name: "alpha", Payload: `{"a":1}`},
				{CallID: "c2", Name: "beta", Payload: `{"b":2}`},
			},
			results:   []Item{ToolCallResult{CallID: "c1", Output: "1"}},
			wantState: StateAwaitingToolResults,
		},
		{
			name: "permissive allows partial send",
			calls: []ToolCall{
				{CallID: "c1", Name: "alpha", Payload: `{"a":1}`},
				{CallID: "c2", Name: "beta", Payload: `{"b":2}`},
			},
			results:     []Item{ToolCallResult{CallID: "c1", Output: "1"}},
			wantSend:    true,
			wantState:   StateIdle,
			resultFirst: true,
			wantItems:   []Item{UserText("hello"), ToolCall{CallID: "c1", Name: "alpha", Payload: `{"a":1}`}, ToolCall{CallID: "c2", Name: "beta", Payload: `{"b":2}`}, ToolCallResult{CallID: "c1", Output: "1"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			thread := newTestThread(t)
			streamer := newFakeStreamer()
			streamer.capabilities.ToolResultSendPolicy = tt.policy
			streamer.Reply(func(b *streamBuilder) {
				for _, call := range tt.calls {
					b.Emit(call)
				}
			})
			if tt.wantSend {
				streamer.Reply(func(b *streamBuilder) {
					b.AssertRequest(func(req Req) {
						if !reflect.DeepEqual(req.Items, tt.wantItems) {
							t.Fatalf("unexpected follow-up request items: %#v", req.Items)
						}
					})
				})
			}

			thread.SetExecutor(NewThreadExecutor(streamer.Streamer()))
			thread.QueueItem(UserText("hello"))
			thread.QueueItem(SendItem{})
			if tt.resultFirst {
				for _, result := range tt.results {
					thread.QueueItem(result)
				}
				thread.QueueItem(SendItem{})
			} else {
				thread.QueueItem(SendItem{})
				for _, result := range tt.results {
					thread.QueueItem(result)
				}
			}

			if got := thread.State(); got != tt.wantState {
				t.Fatalf("state = %q, want %q", got, tt.wantState)
			}
			if got, want := streamer.CallCount(), 1; tt.wantSend {
				want = 2
				if got != want {
					t.Fatalf("stream calls = %d, want %d", got, want)
				}
			} else if got != want {
				t.Fatalf("stream calls = %d, want %d", got, want)
			}
		})
	}
}

func TestStrictToolResultPolicyMovesResultsBeforeHeldSendBoundary(t *testing.T) {
	thread := newTestThread(t)
	streamer := newFakeStreamer()
	streamer.capabilities.ToolResultSendPolicy = ToolResultSendRequiresComplete
	streamer.Reply(func(b *streamBuilder) {
		b.Emit(ToolCall{CallID: "c1", Name: "alpha", Payload: `{"a":1}`})
		b.Emit(ToolCall{CallID: "c2", Name: "beta", Payload: `{"b":2}`})
	}).Reply(func(b *streamBuilder) {
		b.AssertRequest(func(req Req) {
			want := []Item{
				UserText("hello"),
				ToolCall{CallID: "c1", Name: "alpha", Payload: `{"a":1}`},
				ToolCall{CallID: "c2", Name: "beta", Payload: `{"b":2}`},
				ToolCallResult{CallID: "c1", Output: "1"},
				ToolCallResult{CallID: "c2", Output: "2"},
			}
			if !reflect.DeepEqual(req.Items, want) {
				t.Fatalf("unexpected follow-up request items: %#v", req.Items)
			}
		})
	})
	thread.SetExecutor(NewThreadExecutor(streamer.Streamer()))

	thread.QueueItem(UserText("hello"))
	thread.QueueItem(SendItem{})
	thread.QueueItem(SendItem{})
	thread.QueueItem(UserText("future"))
	thread.QueueItem(ToolCallResult{CallID: "c1", Output: "1"})
	if got := streamer.CallCount(); got != 1 {
		t.Fatalf("stream calls after partial result = %d, want 1", got)
	}
	thread.QueueItem(ToolCallResult{CallID: "c2", Output: "2"})

	streamer.AssertCallCount(t)
}

func TestAwaitingToolResultsIsNotIdle(t *testing.T) {
	thread := newTestThread(t)
	idleCalls := 0
	thread.SetDelegate(ThreadDelegateFuncs{OnIdle: func(*Thread) { idleCalls++ }})
	streamer := newFakeStreamer()
	streamer.capabilities.ToolResultSendPolicy = ToolResultSendRequiresComplete
	streamer.Reply(func(b *streamBuilder) {
		b.Emit(ToolCall{CallID: "c1", Name: "calc", Payload: `{"a":1}`})
	})
	thread.SetExecutor(NewThreadExecutor(streamer.Streamer()))

	thread.QueueItem(UserText("hello"))
	thread.QueueItem(SendItem{})

	if got := thread.State(); got != StateAwaitingToolResults {
		t.Fatalf("state = %q, want %q", got, StateAwaitingToolResults)
	}
	if idleCalls != 0 {
		t.Fatalf("OnThreadIdle called %d times", idleCalls)
	}
}

func TestDelegateReceivesRawToolCallChunkAndFinalItems(t *testing.T) {
	thread := newTestThread(t)
	seen := []Item{}
	thread.SetDelegate(ThreadDelegateFuncs{OnStreamItemAppended: func(_ *Thread, item Item) {
		seen = append(seen, item)
	}})
	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
		b.Emit(ToolCallChunk{CallID: "c1", Name: "calc", PayloadDelta: `{"a":`})
		b.Emit(ToolCallChunk{CallID: "c1", PayloadDelta: `1}`})
		b.Emit(ToolCall{CallID: "c1", Name: "calc", Payload: `{"a":1}`})
	})
	thread.SetExecutor(NewThreadExecutor(streamer.Streamer()))

	thread.QueueItem(UserText("hello"))
	thread.QueueItem(SendItem{})

	if len(seen) != 3 {
		t.Fatalf("expected three delegate stream callbacks, got %#v", seen)
	}
	if got := seen[0]; got != (ToolCallChunk{CallID: "c1", Name: "calc", PayloadDelta: `{"a":`}) {
		t.Fatalf("unexpected first callback item: %#v", got)
	}
	if got := seen[1]; got != (ToolCallChunk{CallID: "c1", PayloadDelta: `1}`}) {
		t.Fatalf("unexpected second callback item: %#v", got)
	}
	if got := seen[2]; got != (ToolCall{CallID: "c1", Name: "calc", Payload: `{"a":1}`}) {
		t.Fatalf("unexpected third callback item: %#v", got)
	}
}
