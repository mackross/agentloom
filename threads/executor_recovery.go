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
		return t.resumeConstructLLMRequest()
	}
	return nil
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
