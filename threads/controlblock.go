package threads

import "encoding/json"

type State string

const (
	StateIdle                State = "idle"
	StateConstructLLMRequest State = "construct_llm_request"
	StateReceivingStream     State = "receiving_stream"
	StateStreamComplete      State = "stream_complete"
)

type controlBlock struct {
	ip    *item[Item]
	state State
	cbStateHandler
	observer             cbObserver
	streamInsertionPoint *item[Item]
	streamToolCalls      map[string]*item[Item]
	queueStartItem       *item[Item]
}

type cbTransition struct {
	From    State
	To      State
	Changed bool
}

type pendingToolCall struct {
	call    ToolCall
	load    json.RawMessage
	started bool
	bound   bool
}

type cbStateHandler interface {
	QueueItem(items cbItems, v Item) cbTransition
	CanAdvance(items cbItems) bool
	AdvanceNext(items cbItems) cbTransition
	BeginStreaming() cbTransition
	AppendStreamItem(items cbItems, v Item) cbTransition
	EndStreaming() cbTransition
}

// cbItems is the narrow tape surface the control block reads/writes.
// This is a deliberate design smell: CB mutates storage directly instead of
// returning pure actions for Thread to apply. We keep this tradeoff for now
// because it keeps IP/queue/stream insertion invariants co-located and simple.
type cbItems interface {
	Head() *item[Item]
	Tail() *item[Item]
	Append(v Item) *item[Item]
	InsertAfter(after *item[Item], v Item) *item[Item]
	// RemoveAfter removes the item after `after`; passing nil removes head.
	RemoveAfter(after *item[Item]) *item[Item]
}

type cbState struct {
	controlBlock *controlBlock
}

func (s State) New(cb *controlBlock) cbStateHandler {
	stateRef := cbState{controlBlock: cb}
	switch s {
	case StateIdle:
		return idleState(stateRef)
	case StateConstructLLMRequest:
		return constructLLMRequestState(stateRef)
	case StateReceivingStream:
		return receivingStreamState(stateRef)
	case StateStreamComplete:
		return streamCompleteState(stateRef)
	default:
		return idleState(stateRef)
	}
}

type cbObserver interface {
	OnCBStateChange(from, to State) error
}

type idleState cbState

func (s idleState) QueueItem(items cbItems, v Item) cbTransition {
	return queueItemTransition(s.controlBlock, items, v)
}

func (s idleState) CanAdvance(items cbItems) bool {
	return s.controlBlock.nextItemAfterIP(items) != nil
}

func (s idleState) AdvanceNext(items cbItems) cbTransition {
	cb := s.controlBlock
	from := cb.state
	if cb.tryCoalesceAhead(items) {
		cb.syncQueueStartToIP()
		return cbTransition{From: from, To: cb.state, Changed: false}
	}
	next := cb.nextItemAfterIP(items)
	if next == nil {
		return noChangeTransition(cb)
	}
	cb.ip = next
	cb.streamInsertionPoint = nil
	if _, ok := next.Item.(SendItem); ok {
		cb.setState(StateConstructLLMRequest)
	}
	cb.syncQueueStartToIP()
	return cbTransition{From: from, To: cb.state, Changed: from != cb.state}
}

func (s idleState) BeginStreaming() cbTransition {
	return noChangeTransition(s.controlBlock)
}

func (s idleState) AppendStreamItem(_ cbItems, _ Item) cbTransition {
	return noChangeTransition(s.controlBlock)
}

func (s idleState) EndStreaming() cbTransition {
	return noChangeTransition(s.controlBlock)
}

type constructLLMRequestState cbState

func (s constructLLMRequestState) QueueItem(items cbItems, v Item) cbTransition {
	return queueItemTransition(s.controlBlock, items, v)
}

func (s constructLLMRequestState) CanAdvance(_ cbItems) bool {
	return false
}

func (s constructLLMRequestState) AdvanceNext(_ cbItems) cbTransition {
	return noChangeTransition(s.controlBlock)
}

func (s constructLLMRequestState) BeginStreaming() cbTransition {
	cb := s.controlBlock
	from := cb.state
	cb.streamInsertionPoint = cb.ip
	cb.streamToolCalls = map[string]*item[Item]{}
	cb.setState(StateReceivingStream)
	return cbTransition{From: from, To: cb.state, Changed: from != cb.state}
}

func (s constructLLMRequestState) AppendStreamItem(_ cbItems, _ Item) cbTransition {
	return noChangeTransition(s.controlBlock)
}

func (s constructLLMRequestState) EndStreaming() cbTransition {
	return noChangeTransition(s.controlBlock)
}

type receivingStreamState cbState

func (s receivingStreamState) QueueItem(items cbItems, v Item) cbTransition {
	return queueItemTransition(s.controlBlock, items, v)
}

func (s receivingStreamState) CanAdvance(_ cbItems) bool {
	cb := s.controlBlock
	if cb.streamInsertionPoint == nil || cb.ip == nil {
		return false
	}
	return cb.ip != cb.streamInsertionPoint
}

func (s receivingStreamState) AdvanceNext(items cbItems) cbTransition {
	cb := s.controlBlock
	from := cb.state
	if !s.CanAdvance(items) {
		return noChangeTransition(cb)
	}
	if cb.tryCoalesceAhead(items) {
		cb.syncQueueStartToIP()
		return cbTransition{From: from, To: cb.state, Changed: false}
	}
	next := cb.nextItemAfterIP(items)
	if next == nil {
		return noChangeTransition(cb)
	}
	cb.ip = next
	cb.syncQueueStartToIP()
	return cbTransition{From: from, To: cb.state, Changed: false}
}

func (s receivingStreamState) BeginStreaming() cbTransition {
	return noChangeTransition(s.controlBlock)
}

func (s receivingStreamState) AppendStreamItem(items cbItems, v Item) cbTransition {
	cb := s.controlBlock
	from := cb.state
	if x, ok := v.(ToolCallChunk); ok {
		if x.CallID != "" {
			if n, found := cb.streamToolCalls[x.CallID]; found && n != nil {
				if cur, isChunk := n.Item.(ToolCallChunk); isChunk {
					if cur.Name == "" {
						cur.Name = x.Name
					}
					cur.PayloadDelta += x.PayloadDelta
					n.Item = cur
					return cbTransition{From: from, To: cb.state, Changed: false}
				}
			}
		}
		cb.streamInsertionPoint = items.InsertAfter(cb.streamInsertionPoint, x)
		if x.CallID != "" {
			cb.streamToolCalls[x.CallID] = cb.streamInsertionPoint
		}
		return cbTransition{From: from, To: cb.state, Changed: false}
	}
	if x, ok := v.(ToolCall); ok {
		if x.CallID != "" {
			if n, found := cb.streamToolCalls[x.CallID]; found && n != nil {
				n.Item = x
				delete(cb.streamToolCalls, x.CallID)
				return cbTransition{From: from, To: cb.state, Changed: false}
			}
		}
	}
	cb.streamInsertionPoint = items.InsertAfter(cb.streamInsertionPoint, v)
	return cbTransition{From: from, To: cb.state, Changed: false}
}

func (s receivingStreamState) EndStreaming() cbTransition {
	cb := s.controlBlock
	from := cb.state
	cb.streamInsertionPoint = nil
	cb.streamToolCalls = nil
	cb.setState(StateStreamComplete)
	if cb.canEnterIdle() {
		cb.setState(StateIdle)
	}
	return cbTransition{From: from, To: cb.state, Changed: from != cb.state}
}

type streamCompleteState cbState

func (s streamCompleteState) QueueItem(items cbItems, v Item) cbTransition {
	return queueItemTransition(s.controlBlock, items, v)
}

func (s streamCompleteState) CanAdvance(_ cbItems) bool {
	return false
}

func (s streamCompleteState) AdvanceNext(_ cbItems) cbTransition {
	return noChangeTransition(s.controlBlock)
}

func (s streamCompleteState) BeginStreaming() cbTransition {
	return noChangeTransition(s.controlBlock)
}

func (s streamCompleteState) AppendStreamItem(_ cbItems, _ Item) cbTransition {
	return noChangeTransition(s.controlBlock)
}

func (s streamCompleteState) EndStreaming() cbTransition {
	return noChangeTransition(s.controlBlock)
}

func (cb *controlBlock) setState(state State) {
	cb.state = state
	cb.cbStateHandler = state.New(cb)
}

func (cb *controlBlock) IP() *item[Item] {
	return cb.ip
}

func (cb *controlBlock) State() State {
	return cb.state
}

func (cb *controlBlock) emitStateChange(tr cbTransition) error {
	if !tr.Changed || cb.observer == nil {
		return nil
	}
	return cb.observer.OnCBStateChange(tr.From, tr.To)
}

func (cb *controlBlock) queueItem(items cbItems, v Item) error {
	tr := cb.cbStateHandler.QueueItem(items, v)
	return cb.emitStateChange(tr)
}

func (cb *controlBlock) advanceNext(items cbItems) error {
	tr := cb.cbStateHandler.AdvanceNext(items)
	return cb.emitStateChange(tr)
}

func (cb *controlBlock) beginStreaming() error {
	tr := cb.cbStateHandler.BeginStreaming()
	return cb.emitStateChange(tr)
}

func (cb *controlBlock) appendStreamItem(items cbItems, v Item) error {
	tr := cb.cbStateHandler.AppendStreamItem(items, v)
	return cb.emitStateChange(tr)
}

func (cb *controlBlock) endStreaming() error {
	tr := cb.cbStateHandler.EndStreaming()
	return cb.emitStateChange(tr)
}

func (cb *controlBlock) canEnterIdle() bool {
	return true
}

func queueItemTransition(cb *controlBlock, items cbItems, v Item) cbTransition {
	from := cb.state
	n := items.Append(v)
	if cb.queueStartItem == nil {
		cb.queueStartItem = n
	}
	return cbTransition{From: from, To: cb.state, Changed: false}
}

func noChangeTransition(cb *controlBlock) cbTransition {
	return cbTransition{From: cb.state, To: cb.state, Changed: false}
}

func mergeCoalescableItems(left, right Item) (Item, bool) {
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

func (cb *controlBlock) tryCoalesceAhead(items cbItems) bool {
	if cb.ip == nil || cb.ip.Next == nil {
		return false
	}
	merged, ok := mergeCoalescableItems(cb.ip.Item, cb.ip.Next.Item)
	if !ok {
		return false
	}
	removed := items.RemoveAfter(cb.ip)
	if removed == nil {
		return false
	}
	cb.ip.Item = merged
	if cb.streamInsertionPoint == removed {
		cb.streamInsertionPoint = cb.ip
	}
	if cb.queueStartItem == removed {
		cb.queueStartItem = removed.Next
	}
	return true
}

func (cb *controlBlock) nextItemAfterIP(items cbItems) *item[Item] {
	start := items.Head()
	if cb.ip != nil {
		start = cb.ip.Next
	}
	return start
}

func (cb *controlBlock) hasPendingSend(items cbItems) bool {
	for n := cb.nextItemAfterIP(items); n != nil; n = n.Next {
		if _, ok := n.Item.(SendItem); ok {
			return true
		}
	}
	return false
}

func (cb *controlBlock) queueItemBeforeFirstPendingSend(items cbItems, v Item) bool {
	prev := cb.ip
	for n := cb.nextItemAfterIP(items); n != nil; n = n.Next {
		if _, ok := n.Item.(SendItem); ok {
			inserted := items.InsertAfter(prev, v)
			if cb.queueStartItem == n {
				cb.queueStartItem = inserted
			}
			return true
		}
		prev = n
	}
	return false
}

func (cb *controlBlock) canAdvance(items cbItems) bool {
	if cb.cbStateHandler == nil {
		return false
	}
	return cb.cbStateHandler.CanAdvance(items)
}

func (cb *controlBlock) syncQueueStartToIP() {
	if cb.queueStartItem == nil || cb.ip == nil {
		return
	}
	for n := cb.queueStartItem; n != nil; n = n.Next {
		if n == cb.ip {
			cb.queueStartItem = n.Next
			return
		}
	}
}

func (cb *controlBlock) Queued(items cbItems) *itemList[Item] {
	head := cb.queueStartItem
	if head == nil {
		return &itemList[Item]{}
	}
	return &itemList[Item]{head: head, tail: items.Tail()}
}

func (cb *controlBlock) outstandingToolCalls(items cbItems) []OutstandingToolCall {
	if cb.ip == nil {
		return nil
	}
	bindings, pending, pendingByID := map[string]json.RawMessage{}, []OutstandingToolCall{}, map[string]int{}
	for n := items.Head(); n != nil; n = n.Next {
		switch v := n.Item.(type) {
		case ToolsSnapshot:
			bindings = map[string]json.RawMessage{}
			for _, h := range v.Handlers {
				bindings[h.Name] = append(json.RawMessage(nil), h.HandlerLoadData...)
			}
		case ToolCall:
			load, ok := bindings[v.Name]
			call := OutstandingToolCall{
				Call:            v,
				State:           OutstandingToolCallRequested,
				Bound:           ok,
				HandlerLoadData: append(json.RawMessage(nil), load...),
			}
			if i, ok := pendingByID[v.CallID]; ok {
				call.State = pending[i].State
				call.Continue = pending[i].Continue
				pending[i] = call
				break
			}
			pendingByID[v.CallID] = len(pending)
			pending = append(pending, call)
		case ToolCallStarted:
			if i, ok := pendingByID[v.CallID]; ok {
				pending[i].State = OutstandingToolCallStarted
				pending[i].Continue = v.Continue
			}
		case ToolCallResultable:
			if i, ok := pendingByID[v.ToolCallID()]; ok {
				pending[i].Call = ToolCall{}
				delete(pendingByID, v.ToolCallID())
			}
		}
		if n == cb.ip {
			break
		}
	}
	out := pending[:0]
	for _, call := range pending {
		if call.Call.CallID != "" {
			call.HandlerLoadData = append(json.RawMessage(nil), call.HandlerLoadData...)
			out = append(out, call)
		}
	}
	return out
}

func (cb *controlBlock) pendingToolCalls(items cbItems) []pendingToolCall {
	outstanding := cb.outstandingToolCalls(items)
	out := make([]pendingToolCall, 0, len(outstanding))
	for _, call := range outstanding {
		out = append(out, pendingToolCall{
			call:    call.Call,
			load:    append(json.RawMessage(nil), call.HandlerLoadData...),
			started: call.State == OutstandingToolCallStarted,
			bound:   call.Bound,
		})
	}
	return out
}
