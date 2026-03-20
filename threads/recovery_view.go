package threads

import "encoding/json"

// OutstandingToolCallState describes whether an unresolved tool call has only
// been requested or has already started execution.
type OutstandingToolCallState string

const (
	OutstandingToolCallRequested OutstandingToolCallState = "requested"
	OutstandingToolCallStarted   OutstandingToolCallState = "started"
)

// OutstandingToolCall is the recovery-facing view of an unresolved tool call.
// Continue is only meaningful when State is OutstandingToolCallStarted.
type OutstandingToolCall struct {
	Call            ToolCall
	State           OutstandingToolCallState
	Bound           bool
	HandlerLoadData json.RawMessage
	Continue        ToolContinue
}

// OutstandingToolCalls returns the unresolved tool calls visible on the tape in
// a recovery-facing shape.
func (t *Thread) OutstandingToolCalls() []OutstandingToolCall {
	return t.cb.outstandingToolCalls(&t.items)
}
