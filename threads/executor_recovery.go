package threads

import "errors"

var ErrAttachExecutorForRecoveryRequiresRecoverableState = errors.New("threads attach executor for recovery requires idle, construct_llm_request, or receiving_stream thread")
var ErrAttachExecutorForRecoveryRequiresCleanExactState = errors.New("threads attach executor for recovery requires no outstanding started tool calls")

type ToolChunkRecoveryPolicy string

const (
	ToolChunkRecoveryFail                ToolChunkRecoveryPolicy = ""
	ToolChunkRecoveryRollbackAndRetry    ToolChunkRecoveryPolicy = "rollback_and_retry"
	ToolChunkRecoveryKeepAssistantPrefix ToolChunkRecoveryPolicy = "keep_assistant_prefix"
)

type RecoveryOptions struct {
	ToolChunkPolicy ToolChunkRecoveryPolicy
}

func (t *Thread) AttachExecutorForRecovery(e stateObserver) error {
	return t.AttachExecutorForRecoveryWithOptions(e, RecoveryOptions{})
}

func (t *Thread) AttachExecutorForRecoveryWithOptions(e stateObserver, opts RecoveryOptions) error {
	state := t.State()
	if state != StateIdle && state != StateConstructLLMRequest && state != StateReceivingStream {
		return ErrAttachExecutorForRecoveryRequiresRecoverableState
	}
	for _, p := range t.cb.pendingToolCalls(&t.items) {
		if p.started {
			return ErrAttachExecutorForRecoveryRequiresCleanExactState
		}
	}
	if state == StateReceivingStream {
		return t.resumeReceivingStream(e, opts)
	}
	t.SetExecutor(e)
	if state == StateIdle {
		_, err := t.resolvePendingToolCalls()
		return err
	}
	if state == StateConstructLLMRequest {
		return t.resumeConstructLLMRequest()
	}
	return nil
}

func (t *Thread) resumeReceivingStream(e stateObserver, opts RecoveryOptions) error {
	caps := StreamerCapabilities{}
	if r, ok := e.(interface{ StreamerCapabilities() StreamerCapabilities }); ok {
		caps = r.StreamerCapabilities()
	}
	send, toolStart, toolPrev := (*item[Item])(nil), (*item[Item])(nil), (*item[Item])(nil)
	for prev, n := (*item[Item])(nil), t.items.Head(); n != nil; prev, n = n, n.Next {
		if _, ok := n.Item.(SendItem); ok {
			send = n
		}
		if send != nil && n != send && toolStart == nil {
			if _, ok := n.Item.(ToolCallChunk); ok {
				toolStart, toolPrev = n, prev
			} else if _, ok := n.Item.(ToolCall); ok {
				toolStart, toolPrev = n, prev
			}
		}
		if n == t.cb.streamInsertionPoint {
			break
		}
	}
	if toolStart == nil {
		if !caps.AssistantPrefix {
			t.dropStreamTail(send)
		}
	} else {
		switch opts.ToolChunkPolicy {
		case ToolChunkRecoveryFail:
			return ErrAttachExecutorForRecoveryRequiresCleanExactState
		case ToolChunkRecoveryRollbackAndRetry:
			t.dropStreamTail(send)
		case ToolChunkRecoveryKeepAssistantPrefix:
			if !caps.AssistantPrefix || toolPrev == send {
				return ErrAttachExecutorForRecoveryRequiresCleanExactState
			}
			t.dropStreamTail(toolPrev)
		default:
			return ErrAttachExecutorForRecoveryRequiresCleanExactState
		}
	}
	t.SetExecutor(e)
	t.cb.setState(StateConstructLLMRequest)
	return t.resumeConstructLLMRequest()
}

func (t *Thread) dropStreamTail(keep *item[Item]) {
	if keep == nil || keep == t.cb.streamInsertionPoint {
		return
	}
	keep.Next = t.cb.streamInsertionPoint.Next
	if t.items.tail == t.cb.streamInsertionPoint {
		t.items.tail = keep
	}
	t.cb.ip = keep
}

func (t *Thread) resumeConstructLLMRequest() error {
	if t.State() != StateConstructLLMRequest {
		return nil
	}
	if t.delegate != nil {
		t.delegate.OnThreadRequest(t)
	}
	if t.executor != nil {
		if err := t.executor.OnControlBlockStateChange(t, StateConstructLLMRequest, StateConstructLLMRequest); err != nil {
			return err
		}
	}
	return nil
}
