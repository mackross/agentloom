package threads

type (
	// AssistantInstruction sets the instruction for a model (in some models this
	// is known as the system prompt). The latest queued AssistantInstruction will
	// be the one that is used.
	AssistantInstruction string

	// UserText is a queued user message (text). If they are added sequentially
	// they will be concatonated.
	UserText string

	// Assistant text is a queued message (text). If they are added sequentially
	// they will be concatonated.
	AssistantText string

	// PreviousItemMetadata annotates the preceding queued item with request
	// construction metadata. It is not sent to providers as content. Its presence
	// also naturally prevents control-block coalescing between surrounding items.
	PreviousItemMetadata map[string]any
)

// ToolCall is generally added to the thread when an LLM response is requesting
// a ToolCall. It is possible to add synthesized "faked" ToolCalls to a thread
// by adding a ToolCall and a ToolResult with paired CallIDs. Be aware that
// this will still cause the Handler to try and run.
type ToolCall struct {
	CallID string

	// The function name as requested by the model.
	Name string

	// The unparsed ToolCall in string form.
	Payload string
}

// ToolCallChunk is generally not used unless thread delegates wish to do
// partial parsing. When the instruction pointer processes these it will
// coalesce them.
type ToolCallChunk struct {
	CallID       string
	Name         string
	PayloadDelta string
}

// ToolCallResolving is added when runtime resolution for a ToolCall begins.
// If recovery sees this marker without a result or ToolCallStarted, it must
// treat the call as ambiguous rather than definitely not started.
type ToolCallResolving struct {
	CallID string
}

// ToolCallStarted should be added to the thread when a ToolCall handler is
// running but has not returned a result. This will be used to enable better
// recovery from errors/crashes.
type ToolCallStarted struct {
	CallID string
	// Continue defaults to ToolContinueAuto when left unset.
	Continue ToolContinue
	// Recovery is the durable recovery classification copied from ToolDispatch.
	Recovery ToolRecovery
}

// ToolCallResult must have exactly one ToolCall before it in the thread with a
// matching CallID.
type ToolCallResult struct {
	CallID string

	// Output is the string that is returned to the LLM for the ToolCallResult.
	Output string

	// Recovered marks a runtime-generated recovery status result.
	Recovered bool

	// Data is for storing structured data for things like UI and debugging.
	Data map[string]any

	// SafeRollback permits capable request builders to hide this tool call/result
	// and retry from a safe assistant prefix when doing so will not remove
	// unrelated tool calls.
	SafeRollback *ToolCallSafeRollback
}

// ToolCallSafeRollback marks a model-correctable failed tool result where the
// next request may retry from immediately before the failed call. Presence of
// this value grants permission to attempt rollback; the request builder must
// still prove rollback is safe for the current transcript projection.
//
// SteeringHint is inserted as-is as user text at the retry point. Callers
// should make it distinguishable from ordinary user intent, for example with XML
// such as:
//
//	<tool_call_hint tool="search">Retry with valid JSON.</tool_call_hint>
//
// The hint is ephemeral: after a successful retry it disappears from future
// requests, so it should favor clarity and completeness over brevity.
type ToolCallSafeRollback struct {
	SteeringHint string `json:"steering_hint,omitempty"`
}

// ToolsSnapshot is the durable helper boundary for tool routing.
// Snapshot is model-facing state; Handlers is opaque runtime load data keyed by tool name.
type ToolsSnapshot struct {
	Snapshot ToolOfferSnapshot
	Handlers []ToolHandlerBinding
}

// SendItem when queued causes the thread executor to make a request when the
// instruction pointer advances to it.
type SendItem struct{}

func (UserText) Emit() bool                       { return true }
func (AssistantText) Emit() bool                  { return true }
func (PreviousItemMetadata) Emit() bool           { return false }
func (AssistantInstruction) Emit() bool           { return false }
func (ToolCallChunk) Emit() bool                  { return false }
func (ToolCall) Emit() bool                       { return true }
func (ToolCallResolving) Emit() bool              { return false }
func (ToolCallStarted) Emit() bool                { return false }
func (ToolCallResult) Emit() bool                 { return true }
func (ToolsSnapshot) Emit() bool                  { return false }
func (SendItem) Emit() bool                       { return false }
