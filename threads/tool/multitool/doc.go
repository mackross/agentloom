// Package multitool exposes one stable model-facing tool that routes parsed
// command calls to configured subtools.
//
// The model sees a single ToolSpec. Application code can configure many
// subtools behind that one spec; they are "hidden" only in the sense that they
// are not advertised as separate provider tools. Changing the configured
// subtools therefore does not change the model-facing tool schema, which helps
// preserve provider prompt/KV-cache stability.
//
// A multitool is a good fit when a tool surface should stay installed for a
// whole session and lie dormant until needed, or when a required tool should
// push the agent through a workflow such as choosing one command from a small
// set. It is less appropriate when each capability should be advertised as a
// separate first-class provider tool.
//
// Calls use either ModeLark:
//
//	command arg...
//
//	input text
//
// or ModeJSON with command and input fields. Both modes normalize to ToolCall
// fields Command, Args, and Input.
//
// Register the Tool.Normalizer only when the executor/streamer you are moving
// to does not support Lark/custom tool history and the thread may already
// contain ModeLark calls. This commonly happens when switching from a GPT-5
// model that accepts Lark/custom tools to a GPT-4-family model or another
// provider where only JSON/function tool calls are allowed in history. In that
// case normalization gives the new provider a chance to rewrite or accept the
// existing history in a valid provider-specific form.
//
// ModeLark requires provider/model support for custom grammar tools; OpenAI
// GPT-4-family models commonly reject those, so use ModeJSON or a supported
// model/API path when needed.
package multitool
