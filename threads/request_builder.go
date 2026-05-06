package threads

import "reflect"

type RequestBuilder interface{ Build(items []Item) Req }

var DefaultRequestBuilder RequestBuilder = defaultRequestBuilder{}

type defaultRequestBuilder struct{}

func (defaultRequestBuilder) Build(items []Item) Req {
	req := Req{Items: make([]Item, 0, len(items)), ItemMeta: make([]map[string]any, 0, len(items))}
	appendReq := func(it Item, meta map[string]any) {
		if n := len(req.Items); n > 0 && reflect.DeepEqual(normalizeMeta(req.ItemMeta[n-1]), normalizeMeta(meta)) {
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
		if v, ok := it.(ToolCallResultable); ok {
			it = ToolCallResult{CallID: v.ToolCallID(), Output: v.ToolOutput(), Recovered: v.ToolRecovered(), Data: cloneData(v.ToolData())}
		}
		if !it.Emit() {
			continue
		}
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
