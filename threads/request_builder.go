package threads

import (
	"reflect"
	"slices"
)

type RequestBuilder interface {
	Build(items []Item) Req
}

var DefaultRequestBuilder RequestBuilder = defaultRequestBuilder{}

type defaultRequestBuilder struct{}

func (defaultRequestBuilder) Build(items []Item) Req {
	req := Req{Items: make([]Item, 0, len(items))}
	var pending Item
	hasPending := false
	flushPending := func() {
		if !hasPending {
			return
		}
		req.Items = append(req.Items, pending)
		hasPending = false
	}

	for _, it := range items {
		if v, ok := it.(AssistantInstruction); ok {
			req.Instruction = string(v)
			continue
		}
		if v, ok := it.(ToolsSnapshot); ok {
			req.Tools = cloneToolOfferSnapshot(v.Snapshot)
			continue
		}
		if v, ok := it.(ToolCall); ok {
			flushPending()
			req.Items = append(req.Items, v)
			continue
		}
		if v, ok := it.(ToolCallResultable); ok {
			flushPending()
			req.Items = append(req.Items, ToolCallResult{
				CallID:    v.ToolCallID(),
				Output:    v.ToolOutput(),
				Recovered: v.ToolRecovered(),
				Data:      cloneData(v.ToolData()),
			})
			continue
		}

		if !it.Emit() {
			continue
		}

		if hasPending {
			if sharesMergeKey(pending.MergesWith(), it.MergesWith()) {
				if merged, ok := coalesceRequestItems(pending, it); ok {
					pending = merged
					continue
				}
			}
			flushPending()
		}

		pending = it
		hasPending = true
	}
	flushPending()
	return req
}

func sharesMergeKey(left, right []any) bool {
	return slices.ContainsFunc(left, func(l any) bool {
		return slices.ContainsFunc(right, func(r any) bool {
			return reflect.DeepEqual(l, r)
		})
	})
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
