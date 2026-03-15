package tool

import "github.com/mackross/agentloom/threads"

type Payload = threads.ToolPayload
type Spec = threads.ToolSpec
type Snapshot = threads.ToolOfferSnapshot
type Call = threads.ToolCall

type (
	PayloadJSONSchema = threads.ToolPayloadJSONSchema
	PayloadLark       = threads.ToolPayloadLark
	PayloadRegexp     = threads.ToolPayloadRegexp
)

func PayloadText() Payload { return threads.ToolPayloadText() }

func PayloadFor[T any]() Payload { return threads.ToolPayloadFor[T]() }
