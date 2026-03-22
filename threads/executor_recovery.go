package threads

import "errors"

var ErrAttachExecutorForRecoveryRequiresRecoverableState = errors.New("threads attach executor for recovery requires idle or construct_llm_request thread")
var ErrAttachExecutorForRecoveryRequiresCleanExactState = errors.New("threads attach executor for recovery requires no outstanding started tool calls")

func (t *Thread) AttachExecutorForRecovery(e stateObserver) error {
	state := t.State()
	if state != StateIdle && state != StateConstructLLMRequest {
		return ErrAttachExecutorForRecoveryRequiresRecoverableState
	}
	for _, p := range t.cb.pendingToolCalls(&t.items) {
		if p.started {
			return ErrAttachExecutorForRecoveryRequiresCleanExactState
		}
	}
	t.SetExecutor(e)
	if state == StateConstructLLMRequest {
		return t.OnCBStateChange(StateConstructLLMRequest, StateConstructLLMRequest)
	}
	return nil
}
