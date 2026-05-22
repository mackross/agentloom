package threads

import "reflect"

type RequestBuilder interface {
	Build(items []Item, caps StreamerCapabilities) Req
}

var DefaultRequestBuilder RequestBuilder = defaultRequestBuilder{}

type defaultRequestBuilder struct{}

func (defaultRequestBuilder) Build(items []Item, caps StreamerCapabilities) Req {
	items = projectRollbackableToolFailures(items, caps)
	req := Req{Items: make([]Item, 0, len(items)), ItemMeta: make([]map[string]any, 0, len(items))}
	appendReq := func(it Item, meta map[string]any) {
		n := len(req.Items)
		hasPreviousItem := n > 0
		previousMetaMatches := hasPreviousItem && reflect.DeepEqual(normalizeMeta(req.ItemMeta[n-1]), normalizeMeta(meta))
		if previousMetaMatches {
			if merged, ok := coalesceRequestItems(req.Items[n-1], it); ok {
				req.Items[n-1] = merged
				return
			}
		}
		req.Items, req.ItemMeta = append(req.Items, it), append(req.ItemMeta, meta)
	}
	for i := 0; i < len(items); i++ {
		it := items[i]
		if v, ok := it.(AssistantInstruction); ok {
			req.Instruction = string(v)
			continue
		}
		if v, ok := it.(ToolsSnapshot); ok {
			req.Tools = cloneToolOfferSnapshot(v.Snapshot)
			continue
		}
		if _, ok := it.(PreviousItemMetadata); ok {
			continue
		}
		if !it.Emit() {
			continue
		}
		// Metadata items annotate the immediately preceding emitted request item.
		// They are consumed here rather than emitted as content.
		meta := map[string]any(nil)
		for i+1 < len(items) {
			m, ok := items[i+1].(PreviousItemMetadata)
			if !ok {
				break
			}
			meta = mergeMeta(meta, m)
			i++
		}
		appendReq(it, meta)
	}
	return req
}

// projectRollbackableToolFailures lowers the durable thread IR into the request
// shape preferred by streamers that can continue from an assistant prefix.
//
// A rollbackable tool result is still a normal tool result in the durable log:
// conservative streamers see the failed ToolCall plus its corrective
// ToolCallResult. When assistant-prefix continuation is available and the
// request contains exactly one tool call, we can instead safely hide that failed
// call/result from the next model request and inject the caller-provided steering
// hint as UserText at the removed call's position. Repeated failed tool calls
// are not rolled back until a new user message or a successful tool result
// establishes a fresh boundary. If there are multiple tool calls since that
// boundary, this function leaves the request unchanged to avoid projecting away
// unrelated parallel work.
func projectRollbackableToolFailures(items []Item, caps StreamerCapabilities) []Item {
	if !caps.AssistantPrefix {
		return items
	}

	type rollbackCandidate struct {
		callID string
		hint   string
	}

	currentSegmentToolCalls := 0
	var candidate rollbackCandidate
	resetCurrentSegment := func() {
		currentSegmentToolCalls = 0
		candidate = rollbackCandidate{}
	}
	for _, it := range items {
		if _, ok := it.(UserText); ok {
			resetCurrentSegment()
			continue
		}
		if result, ok := it.(ToolCallResult); ok {
			if result.SafeRollback != nil {
				candidate = rollbackCandidate{callID: result.CallID, hint: result.SafeRollback.SteeringHint}
				continue
			}
			resetCurrentSegment()
			continue
		}
		if _, ok := it.(ToolCall); ok {
			currentSegmentToolCalls++
		}
	}
	if currentSegmentToolCalls != 1 || candidate.callID == "" {
		return items
	}

	out := make([]Item, 0, len(items))
	for _, it := range items {
		switch v := it.(type) {
		case ToolCall:
			if v.CallID != candidate.callID {
				out = append(out, it)
				continue
			}
			if candidate.hint != "" {
				out = append(out, UserText(candidate.hint))
			}
		case ToolCallResolving:
			if v.CallID != candidate.callID {
				out = append(out, it)
			}
		case ToolCallStarted:
			if v.CallID != candidate.callID {
				out = append(out, it)
			}
		case ToolCallResult:
			if v.CallID != candidate.callID {
				out = append(out, it)
			}
		default:
			out = append(out, it)
		}
	}
	return out
}

func mergeMeta(a map[string]any, b PreviousItemMetadata) map[string]any {
	out := cloneData(a)
	if out == nil {
		out = map[string]any{}
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}
func normalizeMeta(m map[string]any) map[string]any {
	if len(m) == 0 {
		return nil
	}
	return m
}
func coalesceRequestItems(left, right Item) (Item, bool) {
	if l, ok := left.(UserText); ok {
		if r, ok := right.(UserText); ok {
			return UserText(string(l) + string(r)), true
		}
	}
	if l, ok := left.(AssistantText); ok {
		if r, ok := right.(AssistantText); ok {
			return AssistantText(string(l) + string(r)), true
		}
	}
	return nil, false
}
