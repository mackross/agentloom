package threads

import "encoding/json"

// RecoveryView exposes the recovery-relevant shape of a thread without applying
// any recovery policy.
type RecoveryView struct {
	State                 State
	ExactRecoveryRequires StreamerCapabilities
	OutstandingToolCalls  []OutstandingToolCall
}

// CanRecoverExactlyWith reports whether the provided streamer capabilities can
// continue from the thread's exact retained state.
func (v RecoveryView) CanRecoverExactlyWith(caps StreamerCapabilities) bool {
	if v.ExactRecoveryRequires.AssistantPrefix && !caps.AssistantPrefix {
		return false
	}
	return true
}

// OutstandingToolCall is the recovery-facing view of an unresolved tool call.
type OutstandingToolCall struct {
	Call            ToolCall
	HandlerLoadData json.RawMessage
	Started         bool
	Bound           bool
	Continue        ToolContinue
	Recovery        ToolRecovery
}

// RecoveryView returns the recovery-relevant shape of the current thread.
func (t *Thread) RecoveryView() RecoveryView {
	pending := t.cb.pendingToolCalls(&t.items)
	view := RecoveryView{
		State:                 t.State(),
		ExactRecoveryRequires: exactRecoveryRequirements(t.State()),
		OutstandingToolCalls:  make([]OutstandingToolCall, 0, len(pending)),
	}
	for _, p := range pending {
		view.OutstandingToolCalls = append(view.OutstandingToolCalls, OutstandingToolCall{
			Call:            p.call,
			HandlerLoadData: append(json.RawMessage(nil), p.load...),
			Started:         p.started,
			Bound:           p.bound,
			Continue:        p.continueMode,
			Recovery:        p.recovery,
		})
	}
	return view
}

func exactRecoveryRequirements(state State) StreamerCapabilities {
	if state == StateReceivingStream {
		return StreamerCapabilities{AssistantPrefix: true}
	}
	return StreamerCapabilities{}
}
