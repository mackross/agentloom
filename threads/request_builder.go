package threads

import (
	"reflect"
	"sort"
)

type RequestBuilder interface {
	Build(items []Item, caps StreamerCapabilities) Req
}

var DefaultRequestBuilder RequestBuilder = defaultRequestBuilder{}

type defaultRequestBuilder struct{}

func (defaultRequestBuilder) Build(items []Item, caps StreamerCapabilities) Req {
	items = projectRollbackableToolFailures(items, caps)
	lastUser := -1
	for i, item := range items {
		if _, ok := item.(UserText); ok {
			lastUser = i
		}
	}
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
		if reasoning, ok := it.(ReasoningItem); ok && (caps.Reasoning[0] == "" || reasoning.Provider != caps.Reasoning[0] || caps.Reasoning[1] != "" && i <= lastUser) {
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
// A rollbackable tool result is still a normal tool result in the durable log.
// Once every call has a terminal result, assistant-prefix streamers can hide the
// rollbackable call/result pairs while retaining successful parallel siblings.
// The latest failed round contributes its exact steering hints; a later
// successful round removes those hints. A user message starts a new projection
// segment, so earlier failures remain ordinary history.
func projectRollbackableToolFailures(items []Item, caps StreamerCapabilities) []Item {
	if !caps.AssistantPrefix {
		return items
	}

	type callInfo struct {
		name  string
		batch int
		index int
	}
	type hintInfo struct {
		text  string
		index int
	}
	type toolRound struct {
		hints    []hintInfo
		hasOther bool
	}

	segmentStart := 0
	for i, item := range items {
		if _, ok := item.(UserText); ok {
			segmentStart = i + 1
		}
	}

	calls := make(map[string]callInfo)
	results := make(map[string]ToolCallResult)
	resultIndexes := make(map[string]int)
	batch := -1
	needBatch := true
	for i, item := range items {
		switch value := item.(type) {
		case UserText:
			needBatch = true
		case ToolCall:
			if needBatch {
				batch++
				needBatch = false
			}
			calls[value.CallID] = callInfo{name: value.Name, batch: batch, index: i}
		case ToolCallResult:
			results[value.CallID] = value
			resultIndexes[value.CallID] = i
			needBatch = true
		}
	}
	for callID := range calls {
		if _, ok := results[callID]; !ok {
			return items
		}
	}

	batchRounds := make(map[int]map[string]*toolRound)
	rollbackCalls := make(map[string]struct{})
	for callID, info := range calls {
		result := results[callID]
		if info.index < segmentStart || resultIndexes[callID] < segmentStart {
			continue
		}
		byTool := batchRounds[info.batch]
		if byTool == nil {
			byTool = make(map[string]*toolRound)
			batchRounds[info.batch] = byTool
		}
		round := byTool[info.name]
		if round == nil {
			round = &toolRound{}
			byTool[info.name] = round
		}
		if result.SafeRollback == nil {
			round.hasOther = true
			continue
		}
		rollbackCalls[callID] = struct{}{}
		round.hints = append(round.hints, hintInfo{
			text:  result.SafeRollback.SteeringHint,
			index: resultIndexes[callID],
		})
	}
	if len(rollbackCalls) == 0 {
		return items
	}

	active := make(map[string][]hintInfo)
	batchIDs := make([]int, 0, len(batchRounds))
	for batchID := range batchRounds {
		batchIDs = append(batchIDs, batchID)
	}
	sort.Ints(batchIDs)
	for _, batchID := range batchIDs {
		for name, round := range batchRounds[batchID] {
			if len(round.hints) > 0 {
				active[name] = round.hints
				continue
			}
			if round.hasOther {
				delete(active, name)
			}
		}
	}

	out := make([]Item, 0, len(items))
	skipMetadata := false
	for _, item := range items {
		if _, ok := item.(PreviousItemMetadata); ok {
			if skipMetadata {
				continue
			}
			out = append(out, item)
			continue
		}
		skipMetadata = false
		callID := ""
		switch value := item.(type) {
		case ToolCall:
			callID = value.CallID
		case ToolCallResolving:
			callID = value.CallID
		case ToolCallStarted:
			callID = value.CallID
		case ToolCallResult:
			callID = value.CallID
		}
		if _, remove := rollbackCalls[callID]; remove {
			skipMetadata = item.Emit()
			continue
		}
		out = append(out, item)
	}

	var hints []hintInfo
	for _, toolHints := range active {
		hints = append(hints, toolHints...)
	}
	sort.Slice(hints, func(i, j int) bool { return hints[i].index < hints[j].index })
	for _, hint := range hints {
		if hint.text != "" {
			out = append(out, UserText(hint.text))
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
