package threads

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
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

func walThroughOp(events []WALEvent, op string) []WALEvent {
	for i, ev := range events {
		if ev.Op == op {
			return append([]WALEvent(nil), events[:i+1]...)
		}
	}
	return append([]WALEvent(nil), events...)
}

func restoreReceivingStreamCheckpoint(t *testing.T, beforeSend []Item, streamed ...Item) (*Thread, func()) {
	t.Helper()

	thread := New()
	ready := make(chan struct{})
	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
		for _, item := range streamed {
			b.Emit(item)
		}
		b.Do(func() { close(ready) })
		b.Wait("hold")
	})
	thread.SetExecutor(NewThreadExecutor(streamer.Streamer()))

	done := make(chan struct{})
	go func() {
		for _, item := range beforeSend {
			thread.QueueItem(item)
		}
		thread.QueueItem(UserText("hello"))
		thread.QueueItem(SendItem{})
		close(done)
	}()

	<-ready
	if got := thread.State(); got != StateReceivingStream {
		streamer.Resolve("hold")
		<-done
		t.Fatalf("expected source thread receiving_stream, got %q", got)
	}
	cp, err := thread.Checkpoint(CheckpointOptions{Policy: InflightUnsafe})
	if err != nil {
		streamer.Resolve("hold")
		<-done
		t.Fatalf("checkpoint receiving stream: %v", err)
	}
	restored, err := RestoreCheckpoint(cp, RestoreOptions{AllowUnsafe: true})
	if err != nil {
		streamer.Resolve("hold")
		<-done
		t.Fatalf("restore receiving stream checkpoint: %v", err)
	}
	if got := restored.State(); got != StateReceivingStream {
		streamer.Resolve("hold")
		<-done
		t.Fatalf("expected restored receiving_stream, got %q", got)
	}

	cleanup := func() {
		streamer.Resolve("hold")
		<-done
	}
	return restored, cleanup
}

type clearingDurableStore struct {
	cp           Checkpoint
	wal          []WALEvent
	replaceCount int
}

func (s *clearingDurableStore) ReplaceSnapshot(cp Checkpoint) {
	s.cp = Checkpoint{Seq: cp.Seq, Unsafe: cp.Unsafe, Snapshot: cloneSnapshot(cp.Snapshot)}
	s.wal = nil
	s.replaceCount++
}

func (s *clearingDurableStore) AppendWALDiff(diff []WALEvent) {
	s.wal = append(s.wal, diff...)
}

func (s *clearingDurableStore) Load() (Checkpoint, []WALEvent) {
	return Checkpoint{Seq: s.cp.Seq, Unsafe: s.cp.Unsafe, Snapshot: cloneSnapshot(s.cp.Snapshot)}, append([]WALEvent(nil), s.wal...)
}

func restoreReceivingStreamFromDurableStore(t *testing.T, beforeSend []Item, streamed ...Item) (*Thread, *clearingDurableStore, func()) {
	t.Helper()

	thread := New()
	store := &clearingDurableStore{}
	thread.SetDurableStore(store)
	ready := make(chan struct{})
	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
		for _, item := range streamed {
			b.Emit(item)
		}
		b.Do(func() { close(ready) })
		b.Wait("hold")
	})
	thread.SetExecutor(NewThreadExecutor(streamer.Streamer()))

	done := make(chan struct{})
	go func() {
		for _, item := range beforeSend {
			thread.QueueItem(item)
		}
		thread.QueueItem(UserText("hello"))
		thread.QueueItem(SendItem{})
		close(done)
	}()

	<-ready
	cp, wal := store.Load()
	restored, err := RestoreFromCheckpointAndWAL(cp, wal, RestoreOptions{AllowUnsafe: true})
	if err != nil {
		streamer.Resolve("hold")
		<-done
		t.Fatalf("restore receiving stream from durable store: %v", err)
	}
	if got := restored.State(); got != StateReceivingStream {
		streamer.Resolve("hold")
		<-done
		t.Fatalf("expected restored receiving_stream, got %q", got)
	}
	restored.store = store

	cleanup := func() {
		streamer.Resolve("hold")
		<-done
	}
	return restored, store, cleanup
}

func walOps(events []WALEvent) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		out = append(out, ev.Op)
	}
	return out
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

func TestAttachExecutorForRecoveryResolvesRequestedToolAfterEndStreamRestore(t *testing.T) {
	// Once end_stream is durably recorded, the ToolCall is a stable request from
	// the model. Because no ToolCallStarted exists, attach should resolve it
	// normally rather than treating it as ambiguous stream material to roll back.
	thread, base := runPendingToolDispatch(t, ToolDispatch{})
	restored, err := RestoreFromCheckpointAndWAL(base, walThroughOp(thread.WALAfter(base.Seq), walOpEndStream), RestoreOptions{})
	if err != nil {
		t.Fatalf("restore end-stream checkpoint and wal: %v", err)
	}
	if got := restored.State(); got != StateIdle {
		t.Fatalf("expected restored state idle, got %q", got)
	}

	resolverCalls := 0
	restored.SetToolResolver(toolResolverFunc(func(context.Context, ToolCall, json.RawMessage) (ToolDispatch, error) {
		resolverCalls++
		return ToolDispatch{Items: []Item{ToolCallResult{CallID: "c1", Output: "2"}}}, nil
	}))
	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
		b.AssertRequest(func(req Req) {
			want := []Item{UserText("hello"), ToolCall{CallID: "c1", Name: "calc", Payload: `{"a":1}`}, ToolCallResult{CallID: "c1", Output: "2"}}
			if !reflect.DeepEqual(req.Items, want) {
				t.Fatalf("unexpected recovered tool follow-up request items: %#v", req.Items)
			}
		})
		b.Emit(AssistantText("ok"))
	})

	if err := restored.AttachExecutorForRecovery(NewThreadExecutor(streamer.Streamer())); err != nil {
		t.Fatalf("attach executor for recovery: %v", err)
	}
	if resolverCalls != 1 {
		t.Fatalf("expected resolver to run once, got %d", resolverCalls)
	}
	streamer.AssertCallCount(t)
}

func TestAttachExecutorForRecoveryReceivingStreamScenarios(t *testing.T) {
	tests := []struct {
		name             string
		beforeSend       []Item
		streamed         []Item
		capabilities     StreamerCapabilities
		options          RecoveryOptions
		wantErr          bool
		wantStreamCalls  int
		wantRequestItems []Item
		response         []Item
		wantFinalState   State
		wantItems        []Item
		durable          bool
		wantReplaceDelta int
		wantReloadState  State
		wantReloadItems  []Item
		wantReloadWALOps []string
	}{
		{
			// If the stream crashed before any model output was retained, the retry
			// request has no assistant prefix and does not need provider prefix support.
			// The rollback snapshot still matters durably: it clears the original
			// begin_stream WAL before the retry stream writes new WAL.
			name:             "empty stream restarts without assistant prefix",
			capabilities:     StreamerCapabilities{AssistantPrefix: false},
			wantStreamCalls:  1,
			wantRequestItems: []Item{UserText("hello")},
			response:         []Item{AssistantText("done")},
			wantFinalState:   StateIdle,
			wantItems:        []Item{UserText("hello"), SendItem{}, AssistantText("done")},
			durable:          true,
			wantReplaceDelta: 1,
			wantReloadState:  StateIdle,
			wantReloadItems:  []Item{UserText("hello"), SendItem{}, AssistantText("done")},
			wantReloadWALOps: []string{walOpBeginStream, walOpAppendStreamItem, walOpEndStream},
		},
		{
			// A plain partial assistant response has no side effects or tool ambiguity;
			// when the streamer accepts assistant prefixes, exact resume should keep it.
			// No rollback snapshot is needed because the original stream WAL remains part
			// of the retained transcript.
			name:             "assistant prefix resumes plain assistant text",
			streamed:         []Item{AssistantText("partial ")},
			capabilities:     StreamerCapabilities{AssistantPrefix: true},
			wantStreamCalls:  1,
			wantRequestItems: []Item{UserText("hello"), AssistantText("partial ")},
			response:         []Item{AssistantText("done")},
			wantFinalState:   StateIdle,
			wantItems:        []Item{UserText("hello"), SendItem{}, AssistantText("partial done")},
			durable:          true,
			wantReplaceDelta: 0,
			wantReloadState:  StateIdle,
			wantReloadItems:  []Item{UserText("hello"), SendItem{}, AssistantText("partial done")},
			wantReloadWALOps: []string{walOpQueueItem, walOpQueueItem, walOpBeginStream, walOpAppendStreamItem, walOpBeginStream, walOpAppendStreamItem, walOpEndStream},
		},
		{
			// Without assistant-prefix support, retained assistant text cannot be
			// sent as a continuation prompt; because no tool started, recovery should
			// drop the streamed tail and retry the original request instead. Durably,
			// the old assistant chunk must not resurrect after reload.
			name:             "no assistant prefix rolls back retained assistant text",
			streamed:         []Item{AssistantText("partial ")},
			capabilities:     StreamerCapabilities{AssistantPrefix: false},
			wantStreamCalls:  1,
			wantRequestItems: []Item{UserText("hello")},
			response:         []Item{AssistantText("done")},
			wantFinalState:   StateIdle,
			wantItems:        []Item{UserText("hello"), SendItem{}, AssistantText("done")},
			durable:          true,
			wantReplaceDelta: 1,
			wantReloadState:  StateIdle,
			wantReloadItems:  []Item{UserText("hello"), SendItem{}, AssistantText("done")},
			wantReloadWALOps: []string{walOpBeginStream, walOpAppendStreamItem, walOpEndStream},
		},
		{
			// Partial tool-call JSON is not a stable model-visible boundary; replaying
			// from it risks corrupting or duplicating the tool request, so the default
			// policy fails without mutating retained history or rewriting durable WAL.
			name:             "tool chunk default policy fails without mutation",
			streamed:         []Item{ToolCallChunk{CallID: "c1", Name: "calc", PayloadDelta: `{"a"`}},
			capabilities:     StreamerCapabilities{AssistantPrefix: true},
			wantErr:          true,
			wantFinalState:   StateReceivingStream,
			wantItems:        []Item{UserText("hello"), SendItem{}, ToolCallChunk{CallID: "c1", Name: "calc", PayloadDelta: `{"a"`}},
			durable:          true,
			wantReloadState:  StateReceivingStream,
			wantReloadItems:  []Item{UserText("hello"), SendItem{}, ToolCallChunk{CallID: "c1", Name: "calc", PayloadDelta: `{"a"`}},
			wantReloadWALOps: []string{walOpQueueItem, walOpQueueItem, walOpBeginStream, walOpAppendStreamItem},
		},
		{
			// With an explicit rollback policy, unstarted tool-call chunks can be
			// discarded because no durable ToolCallStarted marker means no side effect
			// has begun. The rollback snapshot must clear the chunk WAL before retrying.
			name:             "tool chunk rollback policy retries original request",
			streamed:         []Item{ToolCallChunk{CallID: "c1", Name: "calc", PayloadDelta: `{"a"`}},
			capabilities:     StreamerCapabilities{AssistantPrefix: true},
			options:          RecoveryOptions{ToolChunkPolicy: ToolChunkRecoveryRollbackAndRetry},
			wantStreamCalls:  1,
			wantRequestItems: []Item{UserText("hello")},
			response:         []Item{AssistantText("done")},
			wantFinalState:   StateIdle,
			wantItems:        []Item{UserText("hello"), SendItem{}, AssistantText("done")},
			durable:          true,
			wantReplaceDelta: 1,
			wantReloadState:  StateIdle,
			wantReloadItems:  []Item{UserText("hello"), SendItem{}, AssistantText("done")},
			wantReloadWALOps: []string{walOpBeginStream, walOpAppendStreamItem, walOpEndStream},
		},
		{
			// Some delegates may have already rendered assistant text but ignored tool
			// chunks; with prefix support, this policy keeps that visible assistant
			// prefix while dropping the ambiguous tool-call tail. The durable snapshot
			// must keep the assistant prefix but clear the chunk WAL.
			name:             "tool chunk keep-prefix policy preserves assistant text",
			streamed:         []Item{AssistantText("partial "), ToolCallChunk{CallID: "c1", Name: "calc", PayloadDelta: `{"a"`}},
			capabilities:     StreamerCapabilities{AssistantPrefix: true},
			options:          RecoveryOptions{ToolChunkPolicy: ToolChunkRecoveryKeepAssistantPrefix},
			wantStreamCalls:  1,
			wantRequestItems: []Item{UserText("hello"), AssistantText("partial ")},
			response:         []Item{AssistantText("done")},
			wantFinalState:   StateIdle,
			wantItems:        []Item{UserText("hello"), SendItem{}, AssistantText("partial done")},
			durable:          true,
			wantReplaceDelta: 1,
			wantReloadState:  StateIdle,
			wantReloadItems:  []Item{UserText("hello"), SendItem{}, AssistantText("partial done")},
			wantReloadWALOps: []string{walOpBeginStream, walOpAppendStreamItem, walOpEndStream},
		},
		{
			// A complete ToolCall is still model-emitted tool material, so keep-prefix
			// should treat it the same way as chunks: preserve assistant text and drop
			// the tool request only when prefix continuation is supported.
			name:             "complete tool call keep-prefix policy preserves assistant text",
			streamed:         []Item{AssistantText("partial "), ToolCall{CallID: "c1", Name: "calc", Payload: `{"a":1}`}},
			capabilities:     StreamerCapabilities{AssistantPrefix: true},
			options:          RecoveryOptions{ToolChunkPolicy: ToolChunkRecoveryKeepAssistantPrefix},
			wantStreamCalls:  1,
			wantRequestItems: []Item{UserText("hello"), AssistantText("partial ")},
			response:         []Item{AssistantText("done")},
			wantFinalState:   StateIdle,
			wantItems:        []Item{UserText("hello"), SendItem{}, AssistantText("partial done")},
		},
		{
			// Keep-prefix cannot manufacture an assistant prefix when the stream tail
			// starts with a tool request, so it must fail without mutation.
			name:           "complete tool call keep-prefix policy requires prior assistant text",
			streamed:       []Item{ToolCall{CallID: "c1", Name: "calc", Payload: `{"a":1}`}},
			capabilities:   StreamerCapabilities{AssistantPrefix: true},
			options:        RecoveryOptions{ToolChunkPolicy: ToolChunkRecoveryKeepAssistantPrefix},
			wantErr:        true,
			wantFinalState: StateReceivingStream,
			wantItems:      []Item{UserText("hello"), SendItem{}, ToolCall{CallID: "c1", Name: "calc", Payload: `{"a":1}`}},
		},
		{
			// Keeping assistant text is impossible if the replacement provider cannot
			// accept assistant-prefixed continuation, so the keep-prefix policy fails
			// instead of silently changing to a rollback policy.
			name:           "tool chunk keep-prefix policy requires assistant prefix support",
			streamed:       []Item{AssistantText("partial "), ToolCallChunk{CallID: "c1", Name: "calc", PayloadDelta: `{"a"`}},
			capabilities:   StreamerCapabilities{AssistantPrefix: false},
			options:        RecoveryOptions{ToolChunkPolicy: ToolChunkRecoveryKeepAssistantPrefix},
			wantErr:        true,
			wantFinalState: StateReceivingStream,
			wantItems: []Item{
				UserText("hello"),
				SendItem{},
				AssistantText("partial "),
				ToolCallChunk{CallID: "c1", Name: "calc", PayloadDelta: `{"a"`},
			},
		},
		{
			// If the provider cannot accept prefixes, a partial tool-call chunk from
			// the interrupted stream can still be discarded when no started marker was
			// durably recorded.
			name:             "no assistant prefix rolls back partial tool call chunk",
			streamed:         []Item{ToolCallChunk{CallID: "c1", Name: "calc", PayloadDelta: `{"a"`}},
			capabilities:     StreamerCapabilities{AssistantPrefix: false},
			options:          RecoveryOptions{ToolChunkPolicy: ToolChunkRecoveryRollbackAndRetry},
			wantStreamCalls:  1,
			wantRequestItems: []Item{UserText("hello")},
			response:         []Item{AssistantText("done")},
			wantFinalState:   StateIdle,
			wantItems:        []Item{UserText("hello"), SendItem{}, AssistantText("done")},
		},
		{
			// A complete requested tool call needs tool recovery policy before another
			// model request can be safely issued, so the default policy keeps history
			// unchanged and fails.
			name:           "requested tool call default policy fails without mutation",
			streamed:       []Item{ToolCall{CallID: "c1", Name: "calc", Payload: `{"a":1}`}},
			capabilities:   StreamerCapabilities{AssistantPrefix: true},
			wantErr:        true,
			wantFinalState: StateReceivingStream,
			wantItems:      []Item{UserText("hello"), SendItem{}, ToolCall{CallID: "c1", Name: "calc", Payload: `{"a":1}`}},
		},
		{
			// A complete model-emitted tool request is still safe to discard under the
			// rollback policy if there is no matching ToolCallStarted marker in the WAL;
			// reload must not bring the discarded ToolCall back.
			name:             "requested tool call rollback policy retries original request",
			streamed:         []Item{ToolCall{CallID: "c1", Name: "calc", Payload: `{"a":1}`}},
			capabilities:     StreamerCapabilities{AssistantPrefix: true},
			options:          RecoveryOptions{ToolChunkPolicy: ToolChunkRecoveryRollbackAndRetry},
			wantStreamCalls:  1,
			wantRequestItems: []Item{UserText("hello")},
			response:         []Item{AssistantText("done")},
			wantFinalState:   StateIdle,
			wantItems:        []Item{UserText("hello"), SendItem{}, AssistantText("done")},
			durable:          true,
			wantReplaceDelta: 1,
			wantReloadState:  StateIdle,
			wantReloadItems:  []Item{UserText("hello"), SendItem{}, AssistantText("done")},
			wantReloadWALOps: []string{walOpBeginStream, walOpAppendStreamItem, walOpEndStream},
		},
		{
			// A requested tool call from the interrupted stream is safe to discard when
			// the WAL contains no matching ToolCallStarted marker, because the tool did
			// not start and there is no external side effect to recover.
			name:             "no assistant prefix rolls back requested tool call",
			streamed:         []Item{ToolCall{CallID: "c1", Name: "calc", Payload: `{"a":1}`}},
			capabilities:     StreamerCapabilities{AssistantPrefix: false},
			options:          RecoveryOptions{ToolChunkPolicy: ToolChunkRecoveryRollbackAndRetry},
			wantStreamCalls:  1,
			wantRequestItems: []Item{UserText("hello")},
			response:         []Item{AssistantText("done")},
			wantFinalState:   StateIdle,
			wantItems:        []Item{UserText("hello"), SendItem{}, AssistantText("done")},
		},
		{
			// Safe recovery metadata only says a future tool-execution policy may rerun
			// the call; stream chunk policy must not bypass started-tool recovery.
			name: "preexisting safe started tool blocks stream recovery policy",
			beforeSend: []Item{
				ToolCall{CallID: "c0", Name: "calc", Payload: `{"a":1}`},
				ToolCallStarted{CallID: "c0", Recovery: ToolRecoverySafe},
			},
			streamed:       []Item{ToolCallChunk{CallID: "c1", Name: "calc", PayloadDelta: `{"a"`}},
			capabilities:   StreamerCapabilities{AssistantPrefix: true},
			options:        RecoveryOptions{ToolChunkPolicy: ToolChunkRecoveryRollbackAndRetry},
			wantErr:        true,
			wantFinalState: StateReceivingStream,
			wantItems: []Item{
				ToolCall{CallID: "c0", Name: "calc", Payload: `{"a":1}`},
				ToolCallStarted{CallID: "c0", Recovery: ToolRecoverySafe},
				UserText("hello"),
				SendItem{},
				ToolCallChunk{CallID: "c1", Name: "calc", PayloadDelta: `{"a"`},
			},
		},
		{
			// Unsafe started tools may have performed side effects, so even an explicit
			// rollback stream policy cannot discard history and rerun the model first.
			name: "preexisting unsafe started tool blocks stream recovery policy",
			beforeSend: []Item{
				ToolCall{CallID: "c0", Name: "calc", Payload: `{"a":1}`},
				ToolCallStarted{CallID: "c0", Recovery: ToolRecoveryUnsafe},
			},
			streamed:       []Item{ToolCallChunk{CallID: "c1", Name: "calc", PayloadDelta: `{"a"`}},
			capabilities:   StreamerCapabilities{AssistantPrefix: true},
			options:        RecoveryOptions{ToolChunkPolicy: ToolChunkRecoveryRollbackAndRetry},
			wantErr:        true,
			wantFinalState: StateReceivingStream,
			wantItems: []Item{
				ToolCall{CallID: "c0", Name: "calc", Payload: `{"a":1}`},
				ToolCallStarted{CallID: "c0", Recovery: ToolRecoveryUnsafe},
				UserText("hello"),
				SendItem{},
				ToolCallChunk{CallID: "c1", Name: "calc", PayloadDelta: `{"a"`},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var (
				restored      *Thread
				store         *clearingDurableStore
				cleanup       func()
				replaceBefore int
			)
			if tt.durable {
				restored, store, cleanup = restoreReceivingStreamFromDurableStore(t, tt.beforeSend, tt.streamed...)
				replaceBefore = store.replaceCount
			} else {
				restored, cleanup = restoreReceivingStreamCheckpoint(t, tt.beforeSend, tt.streamed...)
			}
			defer cleanup()

			streamer := newFakeStreamer()
			streamer.capabilities = tt.capabilities
			if tt.wantStreamCalls > 0 {
				streamer.Reply(func(b *streamBuilder) {
					b.AssertRequest(func(req Req) {
						if !reflect.DeepEqual(req.Items, tt.wantRequestItems) {
							t.Fatalf("unexpected recovered request items: %#v", req.Items)
						}
					})
					for _, item := range tt.response {
						b.Emit(item)
					}
				})
			}

			err := restored.AttachExecutorForRecoveryWithOptions(NewThreadExecutor(streamer.Streamer()), tt.options)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected recovery attach error")
				}
			} else if err != nil {
				t.Fatalf("attach executor for recovery: %v", err)
			}
			if got := streamer.CallCount(); got != tt.wantStreamCalls {
				t.Fatalf("expected %d recovery stream calls, got %d", tt.wantStreamCalls, got)
			}
			if tt.wantFinalState != "" {
				if got := restored.State(); got != tt.wantFinalState {
					t.Fatalf("expected final state %q, got %q", tt.wantFinalState, got)
				}
			}
			if tt.wantItems != nil {
				if got := restored.items.Slice(); !reflect.DeepEqual(got, tt.wantItems) {
					t.Fatalf("unexpected final items: %#v", got)
				}
			}
			if tt.durable {
				if got := store.replaceCount - replaceBefore; got != tt.wantReplaceDelta {
					t.Fatalf("expected %d durable snapshot replacements during recovery, got %d", tt.wantReplaceDelta, got)
				}
				cp, wal := store.Load()
				if got := walOps(wal); !reflect.DeepEqual(got, tt.wantReloadWALOps) {
					t.Fatalf("unexpected durable wal ops after recovery: %#v", got)
				}
				reloaded, err := RestoreFromCheckpointAndWAL(cp, wal, RestoreOptions{AllowUnsafe: true})
				if err != nil {
					t.Fatalf("reload recovered durable state: %v", err)
				}
				if got := reloaded.State(); got != tt.wantReloadState {
					t.Fatalf("expected durable reload state %q, got %q", tt.wantReloadState, got)
				}
				if got := reloaded.items.Slice(); !reflect.DeepEqual(got, tt.wantReloadItems) {
					t.Fatalf("unexpected durable reload items: %#v", got)
				}
			}
		})
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

func TestAttachExecutorForRecoveryAcceptsAwaitingToolResults(t *testing.T) {
	thread := New()
	thread.SetToolProvider(staticToolProvider{snap: testToolsSnapshot("calc", "calculate")})
	streamer := newFakeStreamer()
	streamer.capabilities.ToolResultSendPolicy = ToolResultSendRequiresComplete
	streamer.Reply(func(b *streamBuilder) {
		b.Emit(ToolCall{CallID: "c1", Name: "calc", Payload: `{"a":1}`})
	})
	thread.SetExecutor(NewThreadExecutor(streamer.Streamer()))
	thread.QueueItem(UserText("hello"))
	thread.QueueItem(SendItem{})
	snap, err := thread.Snapshot()
	if err != nil {
		t.Fatalf("snapshot awaiting: %v", err)
	}
	restored, err := RestoreThreadSnapshot(snap)
	if err != nil {
		t.Fatalf("restore awaiting: %v", err)
	}
	restored.SetToolResolver(toolResolverFunc(func(context.Context, ToolCall, json.RawMessage) (ToolDispatch, error) {
		return ToolDispatch{Items: []Item{ToolCallResult{CallID: "c1", Output: "1"}}}, nil
	}))
	followup := newFakeStreamer().Reply(func(b *streamBuilder) {
		b.AssertRequest(func(req Req) {
			want := []Item{UserText("hello"), ToolCall{CallID: "c1", Name: "calc", Payload: `{"a":1}`}, ToolCallResult{CallID: "c1", Output: "1"}}
			if !reflect.DeepEqual(req.Items, want) {
				t.Fatalf("unexpected recovered request items: %#v", req.Items)
			}
		})
	})
	followup.capabilities.ToolResultSendPolicy = ToolResultSendRequiresComplete
	if err := restored.AttachExecutorForRecovery(NewThreadExecutor(followup.Streamer())); err != nil {
		t.Fatalf("attach executor for recovery: %v", err)
	}
	followup.AssertCallCount(t)
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

	restored, err := RestoreFromCheckpointAndWAL(base, thread.WALAfter(base.Seq), RestoreOptions{AllowUnsafe: true})
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

func TestRestoreAfterEndStreamCrashKeepsToolCallRequestedUntilStartedItemPersists(t *testing.T) {
	thread, base := runPendingToolDispatch(t, ToolDispatch{
		Started:  true,
		Recovery: ToolRecoveryUnsafe,
	})

	wal := walThroughOp(thread.WALAfter(base.Seq), walOpEndStream)
	restored, err := RestoreFromCheckpointAndWAL(base, wal, RestoreOptions{})
	if err != nil {
		t.Fatalf("restore from checkpoint + wal: %v", err)
	}
	if got := restored.State(); got != StateIdle {
		t.Fatalf("expected idle after end_stream prefix restore, got %q", got)
	}

	pending := requirePendingToolCall(t, restored)
	if pending.started || pending.recovery != "" {
		t.Fatalf("unexpected pending tool call after end_stream-only restore: %#v", pending)
	}
}

func TestToolRecoveryStatusTextForCanceledCalls(t *testing.T) {
	tests := []struct {
		name       string
		pending    pendingToolCall
		policy     ToolCallRecoveryPolicy
		wantOutput string
	}{
		{
			name: "requested call not run",
			pending: pendingToolCall{call: ToolCall{
				CallID: "c1",
				Name:   "calc",
			}},
			policy: ToolCallRecoveryCancelAll,
			wantOutput: "Tool call status: not completed after recovery.\n\n" +
				"The runtime recovered from an interruption before this tool was run, so no action was taken for this tool call. " +
				"If the result is still needed, you may request the tool again; otherwise continue without it.",
		},
		{
			name: "resolving call outcome unknown",
			pending: pendingToolCall{call: ToolCall{
				CallID: "c1",
				Name:   "calc",
			}, resolving: true},
			policy: ToolCallRecoveryCancelUnsafe,
			wantOutput: "Tool call status: completion unknown after recovery.\n\n" +
				"The runtime was interrupted while handling this tool call and cannot confirm whether the action completed. " +
				"Do not assume it succeeded; if the result matters, verify the relevant state or ask the user before trying again.",
		},
		{
			name: "unsafe started call not retried",
			pending: pendingToolCall{call: ToolCall{
				CallID: "c1",
				Name:   "write_file",
			}, started: true, recovery: ToolRecoveryUnsafe},
			policy: ToolCallRecoveryCancelUnsafe,
			wantOutput: "Tool call status: not retried after recovery.\n\n" +
				"The runtime cannot confirm whether the previous attempt completed, and retrying may duplicate an external action. " +
				"Before requesting this tool again, verify the external state or ask the user for confirmation.",
		},
		{
			name: "safe started call canceled by policy",
			pending: pendingToolCall{call: ToolCall{
				CallID: "c1",
				Name:   "calc",
			}, started: true, recovery: ToolRecoverySafe},
			policy: ToolCallRecoveryCancelAll,
			wantOutput: "Tool call status: not retried after recovery.\n\n" +
				"The previous attempt did not produce a result before the interruption. " +
				"If the result is still needed, you may request the tool again; otherwise continue without it.",
		},
		{
			name: "started call with unknown replay safety",
			pending: pendingToolCall{call: ToolCall{
				CallID: "c1",
				Name:   "send_email",
			}, started: true},
			policy: ToolCallRecoveryCancelUnsafe,
			wantOutput: "Tool call status: not completed after recovery.\n\n" +
				"The runtime could not determine the outcome of this tool call after an interruption, so it did not run the tool again automatically. " +
				"If this action matters, verify the current state or ask the user before retrying.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := recoveryToolCallStatusResult(tt.pending, tt.policy)
			if got.CallID != tt.pending.call.CallID {
				t.Fatalf("expected result call id %q, got %q", tt.pending.call.CallID, got.CallID)
			}
			if got.Output != tt.wantOutput {
				t.Fatalf("unexpected recovery status output:\n%s", got.Output)
			}
			if got.Data == nil {
				t.Fatalf("expected recovery status metadata")
			}
			if got.Data["recovery"] != true {
				t.Fatalf("expected recovery metadata flag, got %#v", got.Data)
			}
			if got.Data["tool_call_status"] != got.Output {
				t.Fatalf("expected metadata to include model-facing status text, got %#v", got.Data)
			}
		})
	}
}

func TestToolRecoveryStatusTextStartsWithStablePrefixAndAvoidsInternalDetails(t *testing.T) {
	cases := []pendingToolCall{
		{call: ToolCall{CallID: "c1", Name: "calc"}},
		{call: ToolCall{CallID: "c2", Name: "calc"}, resolving: true},
		{call: ToolCall{CallID: "c3", Name: "write_file"}, started: true, recovery: ToolRecoveryUnsafe},
		{call: ToolCall{CallID: "c4", Name: "calc"}, started: true, recovery: ToolRecoverySafe},
		{call: ToolCall{CallID: "c5", Name: "send_email"}, started: true},
	}
	for _, p := range cases {
		out := recoveryToolCallStatusResult(p, ToolCallRecoveryCancelAll).Output
		if !strings.HasPrefix(out, "Tool call status:") {
			t.Fatalf("expected stable prefix, got %q", out)
		}
		for _, internal := range []string{"durable", "resolver", "dispatch", "WAL", "marker", "classification"} {
			if strings.Contains(strings.ToLower(out), strings.ToLower(internal)) {
				t.Fatalf("recovery status text leaked internal detail %q: %s", internal, out)
			}
		}
		if !strings.Contains(out, "If") && !strings.Contains(out, "Before") && !strings.Contains(out, "Do not") {
			t.Fatalf("expected next-step guidance in status text: %s", out)
		}
	}
}

func TestThreadExecutorReportsStreamerCapabilities(t *testing.T) {
	streamer := newFakeStreamer()
	streamer.capabilities = StreamerCapabilities{AssistantPrefix: false}

	exec := NewThreadExecutor(streamer.Streamer())

	got := exec.StreamerCapabilities()
	if got.AssistantPrefix || got.ToolResultSendPolicy != "" {
		t.Fatalf("expected assistant-prefix capability to be false, got %#v", got)
	}
}
