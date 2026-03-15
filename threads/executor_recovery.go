package threads

import "errors"

var ErrAttachExecutorForRecoveryRequiresIdle = errors.New("threads attach executor for recovery requires idle thread")

func (t *Thread) AttachExecutorForRecovery(e stateObserver) error {
	if t.State() != StateIdle {
		return ErrAttachExecutorForRecoveryRequiresIdle
	}
	t.SetExecutor(e)
	return nil
}
