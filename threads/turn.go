package threads

import (
	"errors"
)

// ErrInvalidTurn means a Turn is zero, stale, or no longer matches its source
// thread.
var ErrInvalidTurn = errors.New("invalid turn")

// TurnRole is the speaker role for a completed conversation turn.
type TurnRole string

const (
	// TurnUser is a completed user turn.
	TurnUser TurnRole = "user"
	// TurnAssistant is a completed assistant turn.
	TurnAssistant TurnRole = "assistant"
)

// Turn is an opaque completed single-role conversation turn returned by
// Thread.CompletedTurns.
//
// Use the accessor methods for display metadata and Checkpoint to materialize a
// branch point. A Turn is tied to its source thread and becomes invalid after
// that thread mutates.
type Turn struct {
	thread *thread
	index  int
	role   TurnRole
	text   string
	seq    uint32
	end    int
}

// Seq returns the thread mutation sequence. The sequence identifies the current
// durable position of the conversation: checkpoints capture it, WAL events
// advance it, and external stores can use it to compare a live thread with the
// persisted branch/session head.
func (t *thread) Seq() uint32 { return t.mutationSeq }

// CompletedTurns returns the branchable completed single-role turns currently
// visible in the thread. It excludes streaming tails and malformed or unresolved
// tool-call prefixes.
//
// If an EventLoop owns this Thread, call CompletedTurns only from EventLoop.Do.
// The returned Turn values are tied to the current mutation sequence and may be
// invalidated by the next thread mutation.
func (t *thread) CompletedTurns() []Turn {
	items := t.items.Slice()
	limit := completedItemLimit(t, items)
	var turns []Turn
	for i := 0; i < limit; i++ {
		role, text, ok := turnItem(items[i])
		if !ok || text == "" {
			continue
		}
		if n := len(turns); n > 0 && turns[n-1].role == role {
			turns[n-1].text += text
			turns[n-1].end = i
			continue
		}
		turns = append(turns, Turn{thread: t, index: len(turns), role: role, text: text, seq: t.mutationSeq, end: i})
	}
	return turns
}

// Index is the turn's zero-based index in CompletedTurns at the time it was
// read.
func (turn Turn) Index() int { return turn.index }

// Role is the turn speaker.
func (turn Turn) Role() TurnRole { return turn.role }

// Text is display text coalesced from adjacent items with the same role.
func (turn Turn) Text() string { return turn.text }

// Seq is the source thread mutation sequence at the time the turn was read.
func (turn Turn) Seq() uint32 { return turn.seq }

// Checkpoint materializes a branch checkpoint immediately after this turn.
// User turns restore request-ready and unsafe; assistant turns restore idle.
func (turn Turn) Checkpoint() (Checkpoint, error) {
	if turn.thread == nil || turn.seq != turn.thread.mutationSeq {
		return Checkpoint{}, ErrInvalidTurn
	}
	turns := turn.thread.CompletedTurns()
	if turn.index < 0 || turn.index >= len(turns) {
		return Checkpoint{}, ErrInvalidTurn
	}
	fresh := turns[turn.index]
	if fresh.role != turn.role || fresh.text != turn.text || fresh.end != turn.end || fresh.seq != turn.seq {
		return Checkpoint{}, ErrInvalidTurn
	}

	items, err := snapshotPrefix(turn.thread.items.Slice()[:turn.end+1])
	if err != nil {
		return Checkpoint{}, err
	}
	if turn.role == TurnUser {
		items = append(items, SnapshotItem{Type: "send"})
		return Checkpoint{Seq: turn.seq, Unsafe: true, Snapshot: ThreadSnapshot{Version: serializedThreadVersion, State: StateConstructLLMRequest, Items: items, IPIndex: len(items) - 1, QueueStartIndex: -1, StreamInsIndex: -1}}, nil
	}
	return Checkpoint{Seq: turn.seq, Snapshot: ThreadSnapshot{Version: serializedThreadVersion, State: StateIdle, Items: items, IPIndex: len(items) - 1, QueueStartIndex: -1, StreamInsIndex: -1}}, nil
}

func turnItem(v Item) (TurnRole, string, bool) {
	switch x := v.(type) {
	case UserText:
		return TurnUser, string(x), true
	case AssistantText:
		return TurnAssistant, string(x), true
	default:
		return "", "", false
	}
}

func completedItemLimit(t *thread, items []Item) int {
	limit := len(items)
	if t.cb.State() == StateReceivingStream && t.cb.streamInsertionPoint != nil {
		limit = sendIndexBefore(t, t.cb.streamInsertionPoint)
	}
	role := TurnRole("")
	turnStart := 0
	for i := 0; i < limit; i++ {
		if nextRole, text, ok := turnItem(items[i]); ok && text != "" {
			if nextRole != role {
				role = nextRole
				turnStart = i
			}
			continue
		}
		switch item := items[i].(type) {
		case ToolCallChunk, ToolCallResolving, ToolCallStarted:
			return unbranchableLimit(i, role, turnStart)
		case ToolCall:
			if !hasToolResult(items[i+1:limit], item.CallID) {
				return unbranchableLimit(i, role, turnStart)
			}
		case ToolCallResult:
			if !hasToolCall(items[:i], item.CallID) {
				return unbranchableLimit(i, role, turnStart)
			}
		}
	}
	return limit
}

func unbranchableLimit(i int, role TurnRole, turnStart int) int {
	if role == TurnAssistant {
		return turnStart
	}
	return i
}

func hasToolResult(items []Item, callID string) bool {
	for _, item := range items {
		if _, ok := item.(UserText); ok {
			return false
		}
		if result, ok := item.(ToolCallResult); ok && result.CallID == callID {
			return true
		}
	}
	return false
}

func hasToolCall(items []Item, callID string) bool {
	for _, item := range items {
		if call, ok := item.(ToolCall); ok && call.CallID == callID {
			return true
		}
	}
	return false
}

func sendIndexBefore(t *thread, end *item[Item]) int {
	last := 0
	for i, n := 0, t.items.Head(); n != nil && n != end.Next; i, n = i+1, n.Next {
		if _, ok := n.Item.(SendItem); ok {
			last = i
		}
	}
	return last
}

func snapshotPrefix(items []Item) ([]SnapshotItem, error) {
	out := make([]SnapshotItem, 0, len(items))
	for _, item := range items {
		raw, err := itemToSnapshotItem(item)
		if err != nil {
			return nil, err
		}
		out = append(out, raw)
	}
	return out, nil
}
