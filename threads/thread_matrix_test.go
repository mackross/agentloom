package threads

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

func newMatrixBranch(t *testing.T, withEventLoop bool) *Branch {
	t.Helper()
	store := NewMemoryBranchStore()
	stored, err := store.CreateBranch(context.Background(), BranchCreateOptions{})
	if err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	branch, err := loadStoredBranch(stored, RestoreOptions{}, withEventLoop)
	if err != nil {
		t.Fatalf("loadStoredBranch: %v", err)
	}
	t.Cleanup(func() { _ = branch.Close() })
	return branch
}

func requireNoDeadlock(t *testing.T, ch <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func TestThreadMatrixLocalExternalMutationAndWaitMisuse(t *testing.T) {
	thread := newThread()
	thread.QueueItem(UserText("hello"))
	if got := thread.State(); got != StateIdle {
		t.Fatalf("State = %s, want idle", got)
	}
	assertPanics(t, func() { _ = thread.WaitUntilIdle(context.Background()) }, "local WaitUntilIdle")
}

func TestThreadMatrixBranchExternalMutationAndWait(t *testing.T) {
	branch := newMatrixBranch(t, true)
	var thread Thread = branch
	thread.SetDurableStore(nil)
	thread.SetExecutor(nil)
	thread.QueueItem(UserText("hello"))
	if got := thread.State(); got != StateIdle {
		t.Fatalf("State = %s, want idle", got)
	}
	if turns := thread.CompletedTurns(); len(turns) != 1 || turns[0].Text() != "hello" {
		t.Fatalf("CompletedTurns = %#v, want one hello turn", turns)
	}
	if thread.Seq() == 0 {
		t.Fatal("Seq = 0, want mutation sequence")
	}
	snap, err := thread.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snap.Items) != 1 {
		t.Fatalf("Snapshot items = %d, want 1", len(snap.Items))
	}
	if _, err := thread.Checkpoint(CheckpointOptions{Policy: InflightSkip}); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if _, err := thread.EstimateRequestTokens(context.Background()); err != nil {
		t.Fatalf("EstimateRequestTokens: %v", err)
	}
	if err := thread.AttachExecutorForRecoveryWithOptions(nil, RecoveryOptions{}); err != nil {
		t.Fatalf("AttachExecutorForRecoveryWithOptions on idle thread: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := thread.WaitUntilIdle(ctx); err != nil {
		t.Fatalf("WaitUntilIdle: %v", err)
	}
}

func TestThreadMatrixBranchWithoutEventLoopWaitMisuse(t *testing.T) {
	branch := newMatrixBranch(t, false)
	assertPanics(t, func() { _ = branch.WaitUntilIdle(context.Background()) }, "branch WaitUntilIdle without event loop")
}

func TestThreadMatrixBranchDoesNotExposeLocalThreadEscapeHatches(t *testing.T) {
	branch := newMatrixBranch(t, true)
	branchType := reflect.TypeOf(branch)
	for _, name := range []string{"IP", "ReplayWAL"} {
		if _, ok := branchType.MethodByName(name); ok {
			t.Fatalf("Branch exposes local thread method %s", name)
		}
	}
}

func TestThreadMatrixWaitUntilIdleContextDeadline(t *testing.T) {
	branch := newMatrixBranch(t, true)
	if err := branch.runLocal(context.Background(), func(thread *thread) error {
		thread.cb.setState(StateReceivingStream)
		return nil
	}); err != nil {
		t.Fatalf("RunOnEventLoop: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if err := branch.WaitUntilIdle(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitUntilIdle err = %v, want deadline", err)
	}
}

func TestThreadMatrixEventLoopDelegateGetsDirectThread(t *testing.T) {
	branch := newMatrixBranch(t, true)
	called := make(chan struct{})
	branch.SetDelegate(ThreadDelegateFuncs{OnRequest: func(thread Thread) {
		thread.QueueItem(AssistantInstruction("queued by delegate"))
		close(called)
	}})
	branch.QueueItem(UserText("hello"))
	branch.QueueItem(SendItem{})
	requireNoDeadlock(t, called, "delegate callback mutation")
}

func TestThreadMatrixSetDelegateSerializesThroughEventLoop(t *testing.T) {
	branch := newMatrixBranch(t, true)
	delegateStarted := make(chan struct{})
	releaseDelegate := make(chan struct{})
	branch.SetDelegate(ThreadDelegateFuncs{OnRequest: func(Thread) {
		close(delegateStarted)
		<-releaseDelegate
	}})

	branch.QueueItem(UserText("hello"))
	sendDone := make(chan struct{})
	go func() {
		defer close(sendDone)
		branch.QueueItem(SendItem{})
	}()
	requireNoDeadlock(t, delegateStarted, "delegate callback")

	setDone := make(chan struct{})
	go func() {
		defer close(setDone)
		branch.SetDelegate(ThreadDelegateFunc(func(Thread) {}))
	}()
	select {
	case <-setDone:
		close(releaseDelegate)
		t.Fatal("SetDelegate returned while event-loop callback was still running")
	case <-time.After(10 * time.Millisecond):
	}
	close(releaseDelegate)
	requireNoDeadlock(t, setDone, "serialized SetDelegate")
	requireNoDeadlock(t, sendDone, "send mutation")
}

type matrixResolverFunc func(context.Context, Thread, ToolCall, json.RawMessage) (ToolDispatch, error)

func (f matrixResolverFunc) ResolveTool(ctx context.Context, thread Thread, call ToolCall, load json.RawMessage) (ToolDispatch, error) {
	return f(ctx, thread, call, load)
}

func TestThreadMatrixEventLoopSyncToolGetsDirectThread(t *testing.T) {
	branch := newMatrixBranch(t, true)
	called := make(chan struct{})
	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
		b.Emit(ToolCall{CallID: "c1", Name: "calc", Payload: `{}`})
	})
	if err := branch.runLocal(context.Background(), func(thread *thread) error {
		thread.SetExecutor(NewThreadExecutor(streamer.Streamer()))
		return nil
	}); err != nil {
		t.Fatalf("RunOnEventLoop: %v", err)
	}
	branch.SetToolProvider(staticToolProvider{snap: ToolsSnapshot{Snapshot: testToolsSnapshot("calc", "calculate").Snapshot, Handlers: []ToolHandlerBinding{{Name: "calc"}}}})
	branch.SetToolResolver(matrixResolverFunc(func(_ context.Context, thread Thread, call ToolCall, _ json.RawMessage) (ToolDispatch, error) {
		thread.QueueItem(AssistantInstruction("queued by resolver"))
		close(called)
		return ToolDispatch{Started: true, Items: []Item{ToolCallResult{CallID: call.CallID, Output: "ok"}}}, nil
	}))
	branch.QueueItem(UserText("hello"))
	branch.QueueItem(SendItem{})
	requireNoDeadlock(t, called, "sync tool callback mutation")
}

func TestThreadMatrixEventLoopAsyncToolCompletion(t *testing.T) {
	branch := newMatrixBranch(t, true)
	returned := make(chan struct{})
	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
		b.Emit(ToolCall{CallID: "c1", Name: "calc", Payload: `{}`})
	})
	if err := branch.runLocal(context.Background(), func(thread *thread) error {
		thread.SetExecutor(NewThreadExecutor(streamer.Streamer()))
		return nil
	}); err != nil {
		t.Fatalf("RunOnEventLoop: %v", err)
	}
	branch.SetToolProvider(staticToolProvider{snap: ToolsSnapshot{Snapshot: testToolsSnapshot("calc", "calculate").Snapshot, Handlers: []ToolHandlerBinding{{Name: "calc"}}}})
	branch.SetToolResolver(matrixResolverFunc(func(_ context.Context, thread Thread, call ToolCall, _ json.RawMessage) (ToolDispatch, error) {
		go func() {
			for {
				if err := thread.ReturnAsyncToolItem(context.Background(), call.CallID, ToolCallResult{CallID: call.CallID, Output: "ok"}); err == nil {
					close(returned)
					return
				}
				time.Sleep(time.Millisecond)
			}
		}()
		return ToolDispatch{Started: true}, nil
	}))
	branch.QueueItem(UserText("hello"))
	branch.QueueItem(SendItem{})
	requireNoDeadlock(t, returned, "async tool completion")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := branch.WaitUntilIdle(ctx); err != nil {
		t.Fatalf("WaitUntilIdle: %v", err)
	}
}

type cancelMatrixStreamer struct {
	started  chan struct{}
	canceled chan struct{}
	emitOnce bool
}

func (s *cancelMatrixStreamer) Capabilities() StreamerCapabilities            { return StreamerCapabilities{} }
func (s *cancelMatrixStreamer) RegisterToolNormalizer(string, ToolNormalizer) {}
func (s *cancelMatrixStreamer) UnregisterToolNormalizer(string)               {}
func (s *cancelMatrixStreamer) StreamReq(Req, func(Item) error) error         { panic("use context streamer") }
func (s *cancelMatrixStreamer) StreamReqContext(ctx context.Context, _ Req, _ func(Item) error) error {
	close(s.started)
	<-ctx.Done()
	close(s.canceled)
	return ctx.Err()
}

type callbackCancelMatrixStreamer struct {
	canceled chan struct{}
}

func (s *callbackCancelMatrixStreamer) Capabilities() StreamerCapabilities {
	return StreamerCapabilities{}
}
func (s *callbackCancelMatrixStreamer) RegisterToolNormalizer(string, ToolNormalizer) {}
func (s *callbackCancelMatrixStreamer) UnregisterToolNormalizer(string)               {}
func (s *callbackCancelMatrixStreamer) StreamReq(Req, func(Item) error) error {
	panic("use context streamer")
}
func (s *callbackCancelMatrixStreamer) StreamReqContext(ctx context.Context, _ Req, emit func(Item) error) error {
	if err := emit(AssistantText("chunk")); err != nil {
		return err
	}
	<-ctx.Done()
	close(s.canceled)
	return ctx.Err()
}

func TestThreadMatrixEventLoopExternalCancelCurrentTurn(t *testing.T) {
	branch := newMatrixBranch(t, true)
	streamer := &cancelMatrixStreamer{started: make(chan struct{}), canceled: make(chan struct{})}
	if err := branch.runLocal(context.Background(), func(thread *thread) error {
		thread.SetExecutor(NewThreadExecutor(streamer))
		return nil
	}); err != nil {
		t.Fatalf("RunOnEventLoop: %v", err)
	}
	branch.QueueItem(UserText("hello"))
	sendDone := make(chan struct{})
	go func() {
		defer close(sendDone)
		branch.QueueItem(SendItem{})
	}()
	requireNoDeadlock(t, streamer.started, "stream start")
	if !branch.CancelCurrentTurn() {
		t.Fatal("CancelCurrentTurn = false, want true")
	}
	requireNoDeadlock(t, streamer.canceled, "stream cancellation")
	requireNoDeadlock(t, sendDone, "send cancellation return")
}

func TestThreadMatrixEventLoopCallbackDirectThreadCancelCurrentTurn(t *testing.T) {
	branch := newMatrixBranch(t, true)
	streamer := &callbackCancelMatrixStreamer{canceled: make(chan struct{})}
	called := make(chan struct{})
	cancelOK := make(chan bool, 1)
	if err := branch.runLocal(context.Background(), func(thread *thread) error {
		thread.SetExecutor(NewThreadExecutor(streamer))
		return nil
	}); err != nil {
		t.Fatalf("RunOnEventLoop: %v", err)
	}
	branch.SetDelegate(ThreadDelegateFuncs{OnStreamItemAppended: func(thread Thread, _ Item) {
		cancelOK <- thread.CancelCurrentTurn()
		close(called)
	}})
	branch.QueueItem(UserText("hello"))
	branch.QueueItem(SendItem{})
	requireNoDeadlock(t, called, "callback cancellation")
	if !<-cancelOK {
		t.Fatal("CancelCurrentTurn from callback thread = false, want true")
	}
	requireNoDeadlock(t, streamer.canceled, "callback-triggered stream cancellation")
}
