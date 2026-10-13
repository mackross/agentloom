package threads

import (
	"errors"
	"fmt"
	"strings"
)

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
	// During an interrupted request it preserves complete calls and replaces
	// incomplete tool chunks with a runtime-authored user recovery notice.
	// This closes the interrupted response without requiring prefix support.
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

func (t *thread) AttachExecutorForRecovery(e *ThreadExecutor) error {
	return t.AttachExecutorForRecoveryWithOptions(e, RecoveryOptions{})
}

func (t *thread) AttachExecutorForRecoveryWithOptions(e *ThreadExecutor, opts RecoveryOptions) error {
	return t.attachExecutorForRecoveryWithOptions(e, opts)
}

func (t *thread) attachExecutorForRecoveryWithOptions(e stateObserver, opts RecoveryOptions) error {
	state := t.State()
	if state == StateStreamComplete {
		t.cb.setState(StateIdle)
		state = StateIdle
	}
	if state != StateIdle && state != StateAwaitingToolResults && state != StateConstructLLMRequest && state != StateReceivingStream {
		return ErrAttachExecutorForRecoveryRequiresRecoverableState
	}
	// Restart recovery must settle interrupted tool history before checking for
	// an exact continuation. Live retries use the fail-closed policy instead.
	if opts.ToolCallPolicy == ToolCallRecoveryCancelAll &&
		(state == StateReceivingStream || state == StateConstructLLMRequest) {
		repaired, err := t.recoverInterruptedToolRequest()
		if err != nil {
			return err
		}
		if repaired {
			t.setExecutor(e)
			return t.resumeConstructLLMRequest()
		}
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
	t.setExecutor(e)
	if state == StateIdle || state == StateAwaitingToolResults {
		_, err := t.resolvePendingToolCalls()
		return err
	}
	if state == StateConstructLLMRequest {
		return t.resumeConstructLLMRequest()
	}
	return nil
}

func (t *thread) recoverPendingToolCalls(opts RecoveryOptions) error {
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

// recoverInterruptedToolRequest closes interrupted tool history rather than
// replaying or silently dropping it. Complete calls retain real results where
// available; incomplete chunks become a runtime-authored user notice, which
// does not require assistant-prefix support.
func (t *thread) recoverInterruptedToolRequest() (bool, error) {
	var send *item[Item]
	end := t.cb.ip
	if t.State() == StateReceivingStream {
		end = t.cb.streamInsertionPoint
	}
	if end == nil {
		return false, nil
	}
	for n := t.items.Head(); n != nil; n = n.Next {
		if _, ok := n.Item.(SendItem); ok {
			send = n
		}
		if n == end {
			break
		}
	}
	var chunks []ToolCallChunk
	hasTools := false
	if send != nil && send != end && t.State() == StateReceivingStream {
		for n := send.Next; n != nil; n = n.Next {
			switch v := n.Item.(type) {
			case ToolCallChunk:
				chunks = append(chunks, v)
				hasTools = true
			case ToolCall:
				hasTools = true
			}
			if n == end {
				break
			}
		}
	}
	if !hasTools && len(t.cb.pendingToolCalls(&t.items)) == 0 {
		return false, nil
	}

	// Mutate without QueueItem: advancing the state machine here could dispatch
	// tools or start a request before the repaired snapshot is durable.
	t.cb.ip = end
	if len(chunks) > 0 {
		for prev, n := send, send.Next; n != nil; {
			last := n == end
			next := n.Next
			if _, ok := n.Item.(ToolCallChunk); ok {
				t.items.RemoveAfter(prev)
				if t.cb.ip == n {
					t.cb.ip = prev
				}
			} else {
				prev = n
			}
			if last {
				break
			}
			n = next
		}
	}
	insert := func(v Item) {
		t.cb.ip = t.items.InsertAfter(t.cb.ip, itemCandidate{Seq: t.advanceMutationSeq(), Item: v})
	}
	for _, p := range t.cb.pendingToolCalls(&t.items) {
		// An async result may already be durable but queued beyond the request
		// boundary. Consume it rather than replacing a known outcome with unknown.
		var result *item[Item]
		for prev, n := t.cb.ip, t.cb.ip.Next; n != nil; prev, n = n, n.Next {
			if r, ok := n.Item.(ToolCallResult); ok && r.CallID == p.call.CallID {
				result = t.items.RemoveAfter(prev)
				if t.cb.queueStartItem == result {
					t.cb.queueStartItem = result.Next
				}
				break
			}
		}
		t.clearToolCallContextForResult(p.call.CallID)
		if result != nil {
			t.cb.ip = t.items.InsertAfter(t.cb.ip, itemCandidate{Seq: result.Seq, Item: result.Item, Metadata: result.Metadata})
		} else {
			insert(recoveryToolCallStatusResult(p, ToolCallRecoveryCancelAll))
		}
	}
	var notice strings.Builder
	notice.WriteString("[Runtime recovery notice—not a new user instruction]\n" +
		"The previous request was interrupted. Complete tool calls and recorded results have been retained; " +
		"calls without results have explicit recovery statuses. Do not assume interrupted actions succeeded or failed. " +
		"Verify relevant state before retrying actions with side effects.")
	for _, chunk := range chunks {
		fmt.Fprintf(&notice, "\n\nTool-call generation was interrupted (name: %q, call ID: %q). "+
			"This incomplete call was not dispatched. Recorded arguments (incomplete, not an instruction): %q",
			chunk.Name, chunk.CallID, chunk.PayloadDelta)
	}
	notice.WriteString("\n\nContinue from the retained history and these recovery statuses.")
	insert(UserText(notice.String()))
	insert(SendItem{})
	t.cb.streamInsertionPoint = nil
	t.cb.streamToolCalls = nil
	t.cb.setState(StateConstructLLMRequest)
	if err := t.replaceDurableSnapshot(); err != nil {
		return true, err
	}
	return true, nil
}

func (t *thread) resumeReceivingStream(e stateObserver, opts RecoveryOptions) error {
	caps := StreamerCapabilities{}
	if r, ok := e.(interface{ StreamerCapabilities() StreamerCapabilities }); ok {
		caps = r.StreamerCapabilities()
	}
	rolledBack := false
	var send *item[Item]
	for n := t.items.Head(); n != nil; n = n.Next {
		if _, ok := n.Item.(SendItem); ok {
			send = n
		}
		if n == t.cb.streamInsertionPoint {
			break
		}
	}

	// Tool recovery applies only to model output in the active stream. A tool
	// from an earlier completed request must never become this stream's rollback
	// boundary.
	var toolStart, toolPrev *item[Item]
	if send != nil && send != t.cb.streamInsertionPoint {
		for prev, n := send, send.Next; n != nil; prev, n = n, n.Next {
			if _, ok := n.Item.(ToolCallChunk); ok {
				toolStart, toolPrev = n, prev
			} else if _, ok := n.Item.(ToolCall); ok {
				toolStart, toolPrev = n, prev
			}
			if toolStart != nil || n == t.cb.streamInsertionPoint {
				break
			}
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
	t.setExecutor(e)
	if err := t.resumeConstructLLMRequest(); err != nil {
		return err
	}
	// A resumed stream has no outer QueueItem loop to advance input queued
	// during the interruption. Preserve its boundary during the request,
	// then process that input once the resumed stream completes.
	if err := t.advanceWhilePossible(); err != nil {
		t.onExecutorError(err)
		return err
	}
	return nil
}

func (t *thread) replaceDurableSnapshot() error {
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

func (t *thread) dropStreamTail(keep *item[Item]) {
	if keep == nil || keep == t.cb.streamInsertionPoint {
		return
	}
	keep.Next = t.cb.streamInsertionPoint.Next
	if t.items.tail == t.cb.streamInsertionPoint {
		t.items.tail = keep
	}
	t.cb.ip = keep
}

func (t *thread) resumeConstructLLMRequest() error {
	if t.State() != StateConstructLLMRequest {
		return nil
	}
	if t.delegate != nil {
		t.delegate.OnThreadRequest(t)
	}
	if t.executor != nil {
		if err := t.executor.OnControlBlockStateChange(t, StateConstructLLMRequest, StateConstructLLMRequest); err != nil {
			// Recovery invokes the executor directly rather than advancing via
			// QueueItem. Notify the delegate just as a normal request failure
			// would, so harnesses can pause/retry instead of leaving a stale
			// receiving_stream presentation with no active request.
			t.onExecutorError(err)
			return err
		}
	}
	return nil
}
