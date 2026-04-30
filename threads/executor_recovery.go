package threads

import "errors"

var ErrAttachExecutorForRecoveryRequiresRecoverableState = errors.New("threads attach executor for recovery requires idle, awaiting_tool_results, construct_llm_request, or receiving_stream thread")
var ErrAttachExecutorForRecoveryRequiresCleanExactState = errors.New("threads attach executor for recovery requires no outstanding resolving or started tool calls")

type ToolChunkRecoveryPolicy string

const (
	ToolChunkRecoveryFail                ToolChunkRecoveryPolicy = ""
	ToolChunkRecoveryRollbackAndRetry    ToolChunkRecoveryPolicy = "rollback_and_retry"
	ToolChunkRecoveryKeepAssistantPrefix ToolChunkRecoveryPolicy = "keep_assistant_prefix"
)

type ToolCallRecoveryPolicy string

const (
	// ToolCallRecoveryFail is the zero-value fail-closed policy for outstanding
	// tool calls that cannot be completed exactly.
	ToolCallRecoveryFail ToolCallRecoveryPolicy = ""
	// ToolCallRecoveryRunSafeUnimplemented allows recovery to retry outstanding tool calls
	// that are known to be safe to replay.
	ToolCallRecoveryRunSafeUnimplemented ToolCallRecoveryPolicy = "run_safe"
	// ToolCallRecoveryCancelUnsafeUnimplemented appends recovery status results for ambiguous
	// or unsafe outstanding tool calls instead of rerunning them.
	ToolCallRecoveryCancelUnsafeUnimplemented ToolCallRecoveryPolicy = "cancel_unsafe"
	// ToolCallRecoveryCancelAll appends recovery status results for all
	// outstanding tool calls instead of running or rerunning them.
	ToolCallRecoveryCancelAll ToolCallRecoveryPolicy = "cancel_all"
)

type RecoveryOptions struct {
	ToolChunkPolicy ToolChunkRecoveryPolicy
	ToolCallPolicy  ToolCallRecoveryPolicy
}

func recoveryToolCallStatusResult(p pendingToolCall, policy ToolCallRecoveryPolicy) ToolCallResult {
	return ToolCallResult{
		CallID:    p.call.CallID,
		Output:    recoveryToolCallStatusText(p, policy),
		Recovered: true,
	}
}

func recoveryToolCallStatusText(p pendingToolCall, policy ToolCallRecoveryPolicy) string {
	if !p.resolving && !p.started {
		return "Tool call status: not completed after recovery.\n\n" +
			"The runtime recovered from an interruption before this tool was run, so no action was taken for this tool call. " +
			"If the result is still needed, you may request the tool again; otherwise continue without it."
	}
	if p.resolving && !p.started {
		return "Tool call status: completion unknown after recovery.\n\n" +
			"The runtime was interrupted while handling this tool call and cannot confirm whether the action completed. " +
			"Do not assume it succeeded; if the result matters, verify the relevant state or ask the user before trying again."
	}
	if p.recovery == ToolRecoveryUnsafe {
		return "Tool call status: not retried after recovery.\n\n" +
			"The runtime cannot confirm whether the previous attempt completed, and retrying may duplicate an external action. " +
			"Before requesting this tool again, verify the external state or ask the user for confirmation."
	}
	if p.recovery == ToolRecoverySafe && policy == ToolCallRecoveryCancelAll {
		return "Tool call status: not retried after recovery.\n\n" +
			"The previous attempt did not produce a result before the interruption. " +
			"If the result is still needed, you may request the tool again; otherwise continue without it."
	}
	return "Tool call status: not completed after recovery.\n\n" +
		"The runtime could not determine the outcome of this tool call after an interruption, so it did not run the tool again automatically. " +
		"If this action matters, verify the current state or ask the user before retrying."
}

func (t *Thread) AttachExecutorForRecovery(e stateObserver) error {
	return t.AttachExecutorForRecoveryWithOptions(e, RecoveryOptions{})
}

func (t *Thread) AttachExecutorForRecoveryWithOptions(e stateObserver, opts RecoveryOptions) error {
	state := t.State()
	if state == StateStreamComplete {
		t.cb.setState(StateIdle)
		state = StateIdle
	}
	if state != StateIdle && state != StateAwaitingToolResults && state != StateConstructLLMRequest && state != StateReceivingStream {
		return ErrAttachExecutorForRecoveryRequiresRecoverableState
	}
	if state == StateIdle || state == StateAwaitingToolResults {
		if err := t.recoverPendingToolCalls(opts); err != nil {
			return err
		}
		state = t.State()
	}
	for _, p := range t.cb.pendingToolCalls(&t.items) {
		if p.resolving || p.started {
			return ErrAttachExecutorForRecoveryRequiresCleanExactState
		}
	}
	if state == StateReceivingStream {
		return t.resumeReceivingStream(e, opts)
	}
	t.SetExecutor(e)
	if state == StateIdle || state == StateAwaitingToolResults {
		_, err := t.resolvePendingToolCalls()
		return err
	}
	if state == StateConstructLLMRequest {
		return t.resumeConstructLLMRequest()
	}
	return nil
}

func (t *Thread) recoverPendingToolCalls(opts RecoveryOptions) error {
	policy := opts.ToolCallPolicy
	switch policy {
	case "":
		return nil
	case ToolCallRecoveryRunSafeUnimplemented:
		panic("threads ToolCallRecoveryRunSafeUnimplemented is not implemented")
	case ToolCallRecoveryCancelUnsafeUnimplemented:
		panic("threads ToolCallRecoveryCancelUnsafeUnimplemented is not implemented")
	}
	for _, p := range t.cb.pendingToolCalls(&t.items) {
		if !pendingToolCallNeedsRecovery(p) {
			continue
		}
		switch policy {
		case ToolCallRecoveryCancelAll:
			t.QueueItem(recoveryToolCallStatusResult(p, policy))
		default:
			return ErrAttachExecutorForRecoveryRequiresCleanExactState
		}
	}
	return nil
}

func pendingToolCallNeedsRecovery(p pendingToolCall) bool {
	return p.resolving || p.started
}

func (t *Thread) resumeReceivingStream(e stateObserver, opts RecoveryOptions) error {
	caps := StreamerCapabilities{}
	if r, ok := e.(interface{ StreamerCapabilities() StreamerCapabilities }); ok {
		caps = r.StreamerCapabilities()
	}
	rolledBack := false
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
			rolledBack = true
		}
	} else {
		switch opts.ToolChunkPolicy {
		case ToolChunkRecoveryFail:
			return ErrAttachExecutorForRecoveryRequiresCleanExactState
		case ToolChunkRecoveryRollbackAndRetry:
			t.dropStreamTail(send)
			rolledBack = true
		case ToolChunkRecoveryKeepAssistantPrefix:
			if !caps.AssistantPrefix || toolPrev == send {
				return ErrAttachExecutorForRecoveryRequiresCleanExactState
			}
			t.dropStreamTail(toolPrev)
			rolledBack = true
		default:
			return ErrAttachExecutorForRecoveryRequiresCleanExactState
		}
	}
	t.cb.setState(StateConstructLLMRequest)
	if rolledBack {
		if err := t.replaceDurableSnapshot(); err != nil {
			return err
		}
	}
	t.SetExecutor(e)
	return t.resumeConstructLLMRequest()
}

func (t *Thread) replaceDurableSnapshot() error {
	if t.store == nil {
		return nil
	}
	cp, err := t.Checkpoint(CheckpointOptions{Policy: InflightUnsafe})
	if err != nil {
		return err
	}
	t.store.ReplaceSnapshot(cp)
	t.wal = nil
	return nil
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
