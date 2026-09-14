package openai

import (
	"encoding/json"
	"strings"

	"github.com/mackross/openai-go/v3/packages/param"
	"github.com/mackross/openai-go/v3/responses"

	"github.com/mackross/agentloom/llms"
	"github.com/mackross/agentloom/llms/internal/streamerutil"
	"github.com/mackross/agentloom/threads"
)

// Astra support in this provider is the wire model id, the GPT-6 effort
// vocabulary, prompt_cache_options.ttl, configuration_update input items, and
// failure/incomplete reasons. The remaining Astra protocol changes need a typed
// SDK field or a transport change, so they are listed here rather than
// half-implemented:
//
//   - async: true on a function or custom tool definition, plus the matching
//     async flag on returned call items. Delivering a result later against the
//     original call_id already works through ToolCallResult; the missing piece
//     is marking the definition async, which the vendored FunctionToolParam has
//     no field for.
//   - Mid-turn steering (response.steer on the Responses WebSocket). It is
//     Astra-only, connection-scoped, and interleaves a continuation response
//     into a stream the thread executor currently treats as one turn.
//   - Misalignment monitoring, which arrives as response.failed carrying a
//     steer error payload; it surfaces here as a stream failure.
//   - Fast mode with EU data residency, where service_tier fast/priority is
//     rejected. Model.Fast cannot express a per-project restriction.
//
// ReasoningEffortUpdateKey is item metadata that emits a
// configuration_update input item before the marked request item. GPT-6 Astra
// reads the updated reasoning effort from that point onward, which changes
// effort mid-conversation without rewriting the cached prompt prefix.
//
// Keep the request-level reasoning effort unchanged when using this: a
// configuration_update only helps caching when the request-level knob stays
// stable. The value must be one of the efforts the model accepts.
const ReasoningEffortUpdateKey = "openai/reasoning_effort_update"

// ReasoningEffortUpdate marks the next request item with an effort change. It
// is only honoured by GPT-6 Astra in standard single-agent mode.
func ReasoningEffortUpdate(e llms.Effort) threads.PatchItemMetadata {
	return threads.PatchItemMetadata{Metadata: map[string]any{ReasoningEffortUpdateKey: e.String()}}
}

// ClearReasoningEffortUpdate removes a pending effort change.
func ClearReasoningEffortUpdate() threads.PatchItemMetadata {
	return threads.PatchItemMetadata{Metadata: map[string]any{ReasoningEffortUpdateKey: false}}
}

// supportsConfigurationUpdate reports whether a model accepts
// configuration_update input items. Astra only, and only in standard
// single-agent mode.
func supportsConfigurationUpdate(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	return m == ModelAstra || strings.HasPrefix(m, "gpt-6")
}

// effortUpdates emits at most one configuration_update item per distinct
// effort, and never one that repeats the request-level effort. Repeating an
// update adds input tokens without changing behaviour.
type effortUpdates struct {
	enabled bool
	// request is the request-level reasoning effort already in the body.
	request string
	sent    string
}

// before returns the configuration_update item to insert before req.Items[i].
func (u *effortUpdates) before(req threads.Req, i int) (responses.ResponseInputItemUnionParam, bool) {
	if !u.enabled {
		return responses.ResponseInputItemUnionParam{}, false
	}
	raw, ok := streamerutil.ItemMetadata(req, i)[ReasoningEffortUpdateKey]
	if !ok {
		return responses.ResponseInputItemUnionParam{}, false
	}
	if raw == nil || raw == false {
		u.sent = ""
		return responses.ResponseInputItemUnionParam{}, false
	}
	effort, ok := raw.(string)
	if !ok || effort == "" || effort == llms.EffortDefault.String() {
		return responses.ResponseInputItemUnionParam{}, false
	}
	if effort == u.sent || effort == u.request {
		return responses.ResponseInputItemUnionParam{}, false
	}
	item, ok := configurationUpdateParam(effort)
	if !ok {
		return responses.ResponseInputItemUnionParam{}, false
	}
	u.sent = effort
	return item, true
}

// configurationUpdateParam builds the configuration_update input item. The
// vendored SDK has no typed variant for it yet, so the item is carried as raw
// JSON; the API rejects it for models that do not support it.
func configurationUpdateParam(effort string) (responses.ResponseInputItemUnionParam, bool) {
	if effort == "" || effort == llms.EffortDefault.String() {
		return responses.ResponseInputItemUnionParam{}, false
	}
	raw, err := json.Marshal(map[string]any{
		"type":      "configuration_update",
		"reasoning": map[string]any{"effort": effort},
	})
	if err != nil {
		return responses.ResponseInputItemUnionParam{}, false
	}
	return param.Override[responses.ResponseInputItemUnionParam](json.RawMessage(raw)), true
}

// applyPromptCacheTTL merges prompt_cache_options into the request body.
// GPT-5.6 and later replaced prompt_cache_retention with
// prompt_cache_options.ttl, and the vendored SDK has no field for it.
func applyPromptCacheTTL(params *responses.ResponseNewParams, ttl string) {
	if ttl == "" {
		return
	}
	params.SetExtraFields(map[string]any{"prompt_cache_options": map[string]any{"ttl": ttl}})
}
