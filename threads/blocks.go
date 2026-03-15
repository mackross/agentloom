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

// ToolCallStarted should be added to the thread when a ToolCall handler is
// running but has not returned a result. This will be used to enable better
// recovery from errors/crashes.
type ToolCallStarted struct {
	CallID string
	// Continue defaults to ToolContinueAuto when left unset.
	Continue ToolContinue
}

// ToolCallResult must have exactly one ToolCall before it in the thread with a
// matching CallID.
type ToolCallResult struct {
	CallID string

	// Output is the string that is returned to the LLM for the ToolCallResult.
	Output string

	// Data is for storing structured data for things like UI and debugging.
	Data map[string]any
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
func (UserText) MergesWith() []any                { return []any{UserText("")} }
func (AssistantText) Emit() bool                  { return true }
func (AssistantText) MergesWith() []any           { return []any{AssistantText("")} }
func (AssistantInstruction) Emit() bool           { return false }
func (AssistantInstruction) MergesWith() []any    { return nil }
func (ToolCallChunk) Emit() bool                  { return false }
func (ToolCallChunk) MergesWith() []any           { return nil } // TODO: investigate why this is nil and UserText isnt? can we just get rid of this method?
func (ToolCall) Emit() bool                       { return false }
func (ToolCall) MergesWith() []any                { return nil }
func (ToolCallStarted) Emit() bool                { return false }
func (ToolCallStarted) MergesWith() []any         { return nil }
func (ToolCallResult) Emit() bool                 { return false }
func (ToolCallResult) MergesWith() []any          { return nil }
func (r ToolCallResult) ToolCallID() string       { return r.CallID }
func (r ToolCallResult) ToolOutput() string       { return r.Output }
func (r ToolCallResult) ToolData() map[string]any { return cloneData(r.Data) }
func (ToolsSnapshot) Emit() bool                  { return false }
func (ToolsSnapshot) MergesWith() []any           { return nil }
func (SendItem) Emit() bool                       { return false }
func (SendItem) MergesWith() []any                { return nil }
