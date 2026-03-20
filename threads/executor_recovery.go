package threads

import "errors"

var ErrAttachExecutorForRecoveryRequiresIdle = errors.New("threads attach executor for recovery requires idle thread")
var ErrAttachExecutorForRecoveryRequiresCleanExactState = errors.New("threads attach executor for recovery requires no outstanding started tool calls")

func (t *Thread) AttachExecutorForRecovery(e stateObserver) error {
	if t.State() != StateIdle {
		return ErrAttachExecutorForRecoveryRequiresIdle
	}
	for _, p := range t.cb.pendingToolCalls(&t.items) {
		if p.started {
			return ErrAttachExecutorForRecoveryRequiresCleanExactState
		}
	}
	t.SetExecutor(e)
	return nil
}
