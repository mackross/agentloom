// Package openai names the OpenAI-specific grammar forms a custom tool payload
// can carry.
//
// OpenAI's custom tools accept a context-free grammar written as either a Lark
// grammar or a regular expression. The named string types here let a library
// user declare which form a grammar definition is in, in code that is otherwise
// provider-neutral. They exist for importers of this module; nothing inside the
// module references them, and that is intentional.
//
// The provider-neutral payload types that streamers consume are
// threads.ToolPayloadLark and threads.ToolPayloadRegexp, re-exported by
// threads/tool as PayloadLark and PayloadRegexp. Convert to those when building
// a ToolSpec.
package openai

// RegexpDefinitionString is a tool grammar written as an OpenAI regular
// expression pattern.
type RegexpDefinitionString string

// LarkDefinitionString is a tool grammar written in OpenAI's Lark dialect.
type LarkDefinitionString string
