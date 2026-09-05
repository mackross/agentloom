package threads

import "errors"

// ErrInvalidTurn means a Turn ID is zero, absent, or not currently branchable.
var ErrInvalidTurn = errors.New("invalid turn")

// TurnRole is the speaker role for a completed conversation turn.
type TurnRole string

const (
	// TurnUser is a completed user turn.
	TurnUser TurnRole = "user"
	// TurnAssistant is a completed assistant turn.
	TurnAssistant TurnRole = "assistant"
)

// Turn is a plain projection value for a completed single-role conversation
// turn. It is not tied to a live thread and never becomes stale. Use the
// accessor methods for display metadata and ID for stable identity. The turn ID
// is the ItemSeq of the first surviving text node in the turn.
type Turn struct {
	index int
	role  TurnRole
	text  string
	id    ItemSeq
}

// completedTurn is the internal turn projection with its current end-node
// boundary. The end pointer never escapes the owning thread.
type completedTurn struct {
	Turn
	end *item[Item]
}

// Seq returns the thread mutation sequence. The sequence identifies the current
// durable position of the conversation: checkpoints capture it, WAL events
// advance it, and external stores can use it to compare a live thread with the
// persisted branch/session head.
func (t *thread) Seq() uint32 { return t.mutationSeq }

// CompletedTurns returns the branchable completed single-role turns currently
// visible in the thread. See the Thread interface documentation for the
// EventLoop ownership rule.
func (t *thread) CompletedTurns() []Turn {
	turns := t.completedTurns()
	out := make([]Turn, len(turns))
	for i := range turns {
		out[i] = turns[i].Turn
	}
	return out
}

func (t *thread) completedTurns() []completedTurn {
	var turns []completedTurn
	receiving := t.cb.State() == StateReceivingStream && t.cb.streamInsertionPoint != nil
	var limit *item[Item]
	if receiving {
		limit = t.lastSendBefore(t.cb.streamInsertionPoint)
		if limit == nil {
			return nil
		}
	}
	var role TurnRole
	var text string
	var id ItemSeq
	var start, end *item[Item]
	finalize := func() {
		if start != nil {
			turns = append(turns, completedTurn{
				Turn: Turn{index: len(turns), role: role, text: text, id: id},
				end:  end,
			})
			start = nil
		}
	}
	for n := t.items.Head(); n != nil && n != limit; n = n.Next {
		if nrole, ntext, ok := turnItem(n); ok && ntext != "" {
			if start == nil {
				start, end, role, text, id = n, n, nrole, ntext, n.Seq
				continue
			}
			if nrole == role {
				// Same-role text coalesces: keep the first surviving sequence and
				// extend the end boundary.
				end = n
				text += ntext
				continue
			}
			finalize()
			start, end, role, text, id = n, n, nrole, ntext, n.Seq
			continue
		}
		// Non-text node. Unresolved or malformed tool state cuts the turn scan:
		// retain a preceding user turn, but drop an in-progress assistant turn
		// because it belongs to the unbranchable prefix.
		unbranchable := false
		switch item := n.Item.(type) {
		case ToolCallChunk, ToolCallResolving, ToolCallStarted:
			unbranchable = true
		case ToolCall:
			unbranchable = !t.hasToolResult(n.Next, item.CallID)
		case ToolCallResult:
			unbranchable = !t.hasToolCallBefore(n, item.CallID)
		}
		if unbranchable {
			if role == TurnUser {
				finalize()
			}
			return turns
		}
		// Settled control nodes (send, instruction, tools snapshot) continue.
	}
	finalize()
	return turns
}

// lastSendBefore returns the last SendItem at or before end, or nil when there
// is none. It defines the streaming completed-turn boundary.
func (t *thread) lastSendBefore(end *item[Item]) *item[Item] {
	var last *item[Item]
	for n := t.items.Head(); n != nil; n = n.Next {
		if _, ok := n.Item.(SendItem); ok {
			last = n
		}
		if n == end {
			break
		}
	}
	return last
}

// checkpointAtTurn materializes a branch checkpoint immediately after the turn
// identified by id. User turns restore request-ready and unsafe; assistant
// turns restore idle. ErrInvalidTurn is returned if the ID is zero, absent, or
// not currently branchable.
func (t *thread) checkpointAtTurn(id ItemSeq) (Checkpoint, Turn, error) {
	if id == 0 {
		return Checkpoint{}, Turn{}, ErrInvalidTurn
	}
	for _, ct := range t.completedTurns() {
		if ct.id != id {
			continue
		}
		items, err := t.snapshotPrefixNodes(ct.end)
		if err != nil {
			return Checkpoint{}, Turn{}, err
		}
		if ct.role == TurnUser {
			seq, err := t.candidateMutationSeq()
			if err != nil {
				return Checkpoint{}, Turn{}, err
			}
			items = append(items, SnapshotItem{Type: "send", Seq: seq})
			cp := Checkpoint{
				Seq:    uint32(seq),
				Unsafe: true,
				Snapshot: ThreadSnapshot{
					Version:         serializedThreadVersion,
					HeadSeq:         uint32(seq),
					State:           StateConstructLLMRequest,
					Items:           items,
					IPIndex:         len(items) - 1,
					QueueStartIndex: -1,
					StreamInsIndex:  -1,
				},
			}
			return cp, ct.Turn, nil
		}
		cp := Checkpoint{
			Seq: t.mutationSeq,
			Snapshot: ThreadSnapshot{
				Version:         serializedThreadVersion,
				HeadSeq:         t.mutationSeq,
				State:           StateIdle,
				Items:           items,
				IPIndex:         len(items) - 1,
				QueueStartIndex: -1,
				StreamInsIndex:  -1,
			},
		}
		return cp, ct.Turn, nil
	}
	return Checkpoint{}, Turn{}, ErrInvalidTurn
}

func turnItem(n *item[Item]) (TurnRole, string, bool) {
	switch x := n.Item.(type) {
	case UserText:
		return TurnUser, string(x), true
	case AssistantText:
		return TurnAssistant, string(x), true
	default:
		return "", "", false
	}
}

// snapshotPrefixNodes serializes the node chain from the head through end,
// preserving each node's Seq and Metadata.
func (t *thread) snapshotPrefixNodes(end *item[Item]) ([]SnapshotItem, error) {
	var out []SnapshotItem
	for n := t.items.Head(); n != nil; n = n.Next {
		raw, err := nodeToSnapshotItem(n)
		if err != nil {
			return nil, err
		}
		out = append(out, raw)
		if n == end {
			break
		}
	}
	return out, nil
}

// Index is the turn's zero-based index in CompletedTurns at the time it was
// read. It is display-only metadata and does not identify a branch.
func (turn Turn) Index() int { return turn.index }

// Role is the turn speaker.
func (turn Turn) Role() TurnRole { return turn.role }

// Text is display text coalesced from adjacent items with the same role.
func (turn Turn) Text() string { return turn.text }

// ID is the stable ItemSeq of the first surviving text node in the turn.
func (turn Turn) ID() ItemSeq { return turn.id }

// hasToolResult reports whether callID resolves to a ToolCallResult before any
// intervening UserText or SendItem.
func (t *thread) hasToolResult(from *item[Item], callID string) bool {
	for n := from; n != nil; n = n.Next {
		switch v := n.Item.(type) {
		case UserText:
			return false
		case ToolCallResult:
			if v.CallID == callID {
				return true
			}
		case SendItem:
			return false
		}
	}
	return false
}

// hasToolCallBefore reports whether a ToolCall with callID appears before node.
func (t *thread) hasToolCallBefore(node *item[Item], callID string) bool {
	for n := t.items.Head(); n != nil; n = n.Next {
		if call, ok := n.Item.(ToolCall); ok && call.CallID == callID {
			return true
		}
		if n == node {
			break
		}
	}
	return false
}
