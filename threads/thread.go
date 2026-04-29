package threads

import (
	"context"
	"fmt"
)

// Thread is a single-owner conversation state machine. Its mutation methods are
// synchronous and are not goroutine-safe.
//
// If a Thread is owned by an EventLoop, all access that can observe or mutate
// the thread must run through EventLoop.Do. Async tool calling requires
// EventLoop ownership because tool completions can arrive from other goroutines
// and must be serialized back onto the thread.
type Thread struct {
	cb             controlBlock
	items          itemList[Item]
	executor       stateObserver
	delegate       ThreadDelegate
	store          DurableStore
	tools          ToolProvider
	resolver       ToolResolver
	loop           *EventLoop
	policy         ToolResultSendPolicy
	resolvingTools bool

	mutationSeq  uint32
	lastSafeSeq  uint32
	lastSafeSnap ThreadSnapshot
	wal          []WALEvent
	replayingWAL bool
}

func (t *Thread) setEventLoop(loop *EventLoop) {
	if loop != nil && t.loop != nil {
		panic("threads event loop already owns thread")
	}
	t.loop = loop
}

func New() *Thread {
	t := &Thread{}
	t.cb.observer = t
	t.cb.setState(StateIdle)
	t.captureSafeSnapshot()
	return t
}

func (t *Thread) IP() *item[Item] {
	return t.cb.IP()
}

func (t *Thread) State() State {
	return t.cb.State()
}

func (t *Thread) SetExecutor(e stateObserver) {
	t.executor = e
}

func (t *Thread) SetDelegate(d ThreadDelegate) {
	t.delegate = d
}

func (t *Thread) SetDurableStore(store DurableStore) {
	t.store = store
	if store == nil {
		return
	}
	if t.requiresRecovery() {
		return
	}
	cp, err := t.Checkpoint(CheckpointOptions{Policy: InflightSkip})
	if err != nil {
		panic("threads durable store checkpoint failed: " + err.Error())
	}
	store.ReplaceSnapshot(cp)
	t.wal = nil
}

func (t *Thread) SetToolProvider(p ToolProvider) {
	t.tools = p
	snap := ToolsSnapshot{}
	if p != nil {
		snap = cloneToolsSnapshot(p.ToolsSnapshot())
	}
	t.QueueItem(snap)
}

func (t *Thread) SetToolResolver(r ToolResolver) {
	t.resolver = r
}

func (t *Thread) advanceNext() error {
	return t.cb.advanceNext(&t.items)
}

func (t *Thread) advanceWhilePossible() error {
	for {
		if !t.cb.canAdvance(&t.items) {
			return nil
		}
		if err := t.advanceNext(); err != nil {
			return err
		}
	}
}

func (t *Thread) QueueItem(v Item) {
	t.mutationSeq++
	lateAutoSend := false
	if r, ok := v.(ToolCallResultable); ok && !t.resolvingTools && !t.replayingWAL {
		for _, p := range t.cb.pendingToolCalls(&t.items) {
			lateAutoSend = lateAutoSend || p.call.CallID == r.ToolCallID() && p.started && p.continueMode != ToolContinueManual && !t.cb.hasPendingSend(&t.items)
		}
	}
	if err := t.cb.queueItem(&t.items, v); err != nil {
		return
	}
	t.appendWAL(walOpQueueItem, v)
	if lateAutoSend {
		t.QueueItem(SendItem{})
		return
	}
	_ = t.advanceWhilePossible()
	t.captureSafeIfIdle()
}

func (t *Thread) Queued() *itemList[Item] {
	return t.cb.Queued(&t.items)
}

func (t *Thread) beginStreaming() error {
	t.mutationSeq++
	if err := t.cb.beginStreaming(); err != nil {
		return err
	}
	t.appendWAL(walOpBeginStream, nil)
	return nil
}

func (t *Thread) appendStreamItem(v Item) error {
	t.mutationSeq++
	if err := t.cb.appendStreamItem(&t.items, v); err != nil {
		return err
	}
	t.appendWAL(walOpAppendStreamItem, v)
	if t.delegate != nil {
		if d, ok := t.delegate.(ThreadStreamItemAppendedDelegate); ok {
			d.OnThreadStreamItemAppended(t, v)
		}
	}
	return t.advanceWhilePossible()
}

func (t *Thread) endStreaming() error {
	t.mutationSeq++
	if !t.replayingWAL {
		t.cb.awaitToolResults = t.policy == ToolResultSendRequiresComplete && len(t.cb.pendingToolCalls(&t.items)) > 0
	}
	// Persist end_stream before the state-change callback can queue follow-on
	// items. If we crash in that callback, replaying this WAL prefix cleanly
	// restores the requested-tool boundary; later tool-resolution items only
	// appear if their own WAL entries were durably appended.
	t.appendWAL(walOpEndStream, nil)
	if err := t.cb.endStreaming(); err != nil {
		return err
	}
	return nil
}

func (t *Thread) OnCBStateChange(from, to State) error {
	if to == StateConstructLLMRequest && t.delegate != nil {
		t.delegate.OnThreadRequest(t)
	}
	if t.executor != nil {
		if err := t.executor.OnControlBlockStateChange(t, from, to); err != nil {
			return err
		}
	}
	if to == StateIdle || to == StateAwaitingToolResults {
		resolved, err := t.resolvePendingToolCalls()
		if err != nil {
			return err
		}
		if to == StateIdle && !resolved && t.delegate != nil {
			t.delegate.OnThreadIdle(t)
		}
	}
	t.captureSafeIfRestorable()
	return nil
}

func (t *Thread) resolvePendingToolCalls() (bool, error) {
	if t.resolver == nil {
		return false, nil
	}
	t.resolvingTools = true
	defer func() { t.resolvingTools = false }()
	resolved := false
	autoSend := false
	hasPendingSend := t.cb.hasPendingSend(&t.items)
	for _, p := range t.cb.pendingToolCalls(&t.items) {
		if p.call.CallID == "" || p.resolving || p.started {
			continue
		}
		if !p.bound {
			return false, fmt.Errorf("threads missing tool handler binding for %q", p.call.Name)
		}
		t.queueToolResolutionItem(hasPendingSend, ToolCallResolving{CallID: p.call.CallID})
		resolved = true
		dispatch, err := t.resolver.ResolveTool(context.Background(), p.call, p.load)
		if err != nil {
			return resolved, err
		}
		if dispatch.Started {
			t.queueToolResolutionItem(hasPendingSend, ToolCallStarted{
				CallID:   p.call.CallID,
				Continue: dispatch.Continue,
				Recovery: dispatch.Recovery,
			})
		}
		for _, out := range dispatch.Items {
			if out == nil {
				return resolved, fmt.Errorf("threads tool resolver returned nil item for %q", p.call.Name)
			}
			if _, ok := out.(ToolCallResultable); ok && dispatch.Continue != ToolContinueManual {
				autoSend = true
			}
			t.queueToolResolutionItem(hasPendingSend, out)
			resolved = true
		}
	}
	if hasPendingSend && resolved {
		_ = t.advanceWhilePossible()
	}
	if autoSend && !hasPendingSend && !t.cb.hasPendingSend(&t.items) {
		t.QueueItem(SendItem{})
	}
	return resolved, nil
}

func (t *Thread) queueToolResolutionItem(beforeSend bool, v Item) {
	if beforeSend && t.queueBeforePendingSend(v) {
		return
	}
	t.QueueItem(v)
}

func (t *Thread) queueBeforePendingSend(v Item) bool {
	if t.cb.queueItemBeforeFirstPendingSend(&t.items, v) {
		t.mutationSeq++
		t.appendWAL(walOpQueueItemBeforeSend, v)
		return true
	}
	return false
}

type stateObserver interface {
	OnControlBlockStateChange(t *Thread, from, to State) error
}

type ThreadDelegate interface {
	OnThreadIdle(t *Thread)
	OnThreadRequest(t *Thread)
}

type ThreadStreamItemAppendedDelegate interface {
	OnThreadStreamItemAppended(t *Thread, item Item)
}

type ThreadDelegateFunc func(t *Thread)

func (f ThreadDelegateFunc) OnThreadIdle(t *Thread) {
	f(t)
}

func (ThreadDelegateFunc) OnThreadRequest(*Thread) {}

type ThreadDelegateFuncs struct {
	OnIdle               func(t *Thread)
	OnRequest            func(t *Thread)
	OnStreamItemAppended func(t *Thread, item Item)
}

func (d ThreadDelegateFuncs) OnThreadIdle(t *Thread) {
	if d.OnIdle != nil {
		d.OnIdle(t)
	}
}

func (d ThreadDelegateFuncs) OnThreadRequest(t *Thread) {
	if d.OnRequest != nil {
		d.OnRequest(t)
	}
}

func (d ThreadDelegateFuncs) OnThreadStreamItemAppended(t *Thread, item Item) {
	if d.OnStreamItemAppended != nil {
		d.OnStreamItemAppended(t, item)
	}
}
