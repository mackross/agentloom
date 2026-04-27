package threads

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"time"
)

// DurableStore persists thread checkpoints and append-only WAL diffs.
//
// Implementer contract:
//   - Methods are expected to be synchronous and durable before they return.
//   - Methods should panic on failure (I/O, corruption, permission, etc) so Thread
//     can fail closed instead of silently diverging from durable state.
//   - ReplaceSnapshot must atomically set a new base snapshot and clear prior WAL
//     tail bytes for that base.
//   - AppendWALDiff receives one or more WAL events and must append those events
//     without rewriting previously persisted WAL history.
//   - Load must return the current base checkpoint and full WAL event tail.
type DurableStore interface {
	ReplaceSnapshot(cp Checkpoint)
	AppendWALDiff(diff []WALEvent)
	Load() (Checkpoint, []WALEvent)
}

const serializedThreadVersion = 1
const nilItemIndex = -1

var ErrCheckpointWaitTimeout = errors.New("thread checkpoint wait timed out")
var ErrCheckpointNoSafeBoundary = errors.New("thread checkpoint has no safe boundary")
var ErrRestoreUnsafeRequiresExecutor = errors.New("unsafe checkpoint restore requires executor resume setup")
var ErrReplayWALSequence = errors.New("wal sequence is not strictly increasing")

type InflightPolicy string

const (
	InflightWait   InflightPolicy = "wait"
	InflightSkip   InflightPolicy = "skip"
	InflightUnsafe InflightPolicy = "unsafe"
)

type CheckpointOptions struct {
	Policy      InflightPolicy
	WaitTimeout time.Duration
}

type Checkpoint struct {
	Seq      uint32         `json:"seq"`
	Unsafe   bool           `json:"unsafe"`
	Snapshot ThreadSnapshot `json:"snapshot"`
}

type RestoreOptions struct {
	AllowUnsafe bool
}

const (
	walOpQueueItem           = "queue_item"
	walOpQueueItemBeforeSend = "queue_item_before_send"
	walOpBeginStream         = "begin_stream"
	walOpAppendStreamItem    = "append_stream_item"
	walOpEndStream           = "end_stream"
)

type WALEvent struct {
	Seq  uint32       `json:"s"`
	Op   string       `json:"o"`
	Item SnapshotItem `json:"i,omitempty"`
}

type ThreadSnapshot struct {
	Version         int            `json:"ver"`
	State           State          `json:"state"`
	Items           []SnapshotItem `json:"items"`
	IPIndex         int            `json:"ip"`
	QueueStartIndex int            `json:"queue"`
	StreamInsIndex  int            `json:"stream"`
}

type SnapshotItem struct {
	Type     string         `json:"kind"`
	Text     string         `json:"text,omitempty"`
	ID       string         `json:"id,omitempty"`
	Name     string         `json:"name,omitempty"`
	Mode     string         `json:"mode,omitempty"`
	Recovery string         `json:"recovery,omitempty"`
	Args     string         `json:"args,omitempty"`
	Output   string         `json:"output,omitempty"`
	Data     string         `json:"data,omitempty"`
	Tools    *ToolsSnapshot `json:"tools,omitempty"`
}

func (t *Thread) Snapshot() (ThreadSnapshot, error) {
	nodeIndex := map[*item[Item]]int{}
	items := make([]SnapshotItem, 0)
	idx := 0
	for n := t.items.Head(); n != nil; n = n.Next {
		it, err := itemToSnapshotItem(n.Item)
		if err != nil {
			return ThreadSnapshot{}, err
		}
		nodeIndex[n] = idx
		items = append(items, it)
		idx++
	}

	return ThreadSnapshot{
		Version:         serializedThreadVersion,
		State:           t.cb.State(),
		Items:           items,
		IPIndex:         indexOfNode(nodeIndex, t.cb.ip),
		QueueStartIndex: indexOfNode(nodeIndex, t.cb.queueStartItem),
		StreamInsIndex:  indexOfNode(nodeIndex, t.cb.streamInsertionPoint),
	}, nil
}

func RestoreThreadSnapshot(snapshot ThreadSnapshot) (*Thread, error) {
	if snapshot.Version != serializedThreadVersion {
		return nil, fmt.Errorf("unsupported thread serialization version: %d", snapshot.Version)
	}
	if !isKnownState(snapshot.State) {
		return nil, fmt.Errorf("unsupported thread state: %q", snapshot.State)
	}

	t := New()
	nodes := make([]*item[Item], 0, len(snapshot.Items))
	for _, raw := range snapshot.Items {
		v, err := snapshotItemToItem(raw)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, t.items.Append(v))
	}

	var err error
	t.cb.ip, err = nodeAt(nodes, snapshot.IPIndex)
	if err != nil {
		return nil, fmt.Errorf("ip index: %w", err)
	}
	t.cb.queueStartItem, err = nodeAt(nodes, snapshot.QueueStartIndex)
	if err != nil {
		return nil, fmt.Errorf("queue start index: %w", err)
	}
	t.cb.streamInsertionPoint, err = nodeAt(nodes, snapshot.StreamInsIndex)
	if err != nil {
		return nil, fmt.Errorf("stream insertion index: %w", err)
	}
	t.cb.setState(snapshot.State)
	t.captureSafeIfIdle()

	return t, nil
}

func (t *Thread) Checkpoint(opts CheckpointOptions) (Checkpoint, error) {
	policy := opts.Policy
	if policy == "" {
		policy = InflightSkip
	}

	switch policy {
	case InflightUnsafe:
		snap, err := t.Snapshot()
		if err != nil {
			return Checkpoint{}, err
		}
		return Checkpoint{Seq: t.mutationSeq, Unsafe: t.isInflightState(), Snapshot: snap}, nil
	case InflightWait:
		if err := t.waitUntilSafe(opts.WaitTimeout); err != nil {
			return Checkpoint{}, err
		}
		snap, err := t.Snapshot()
		if err != nil {
			return Checkpoint{}, err
		}
		return Checkpoint{Seq: t.mutationSeq, Unsafe: false, Snapshot: snap}, nil
	case InflightSkip:
		if t.isInflightState() {
			if t.lastSafeSnap.Version == 0 {
				return Checkpoint{}, ErrCheckpointNoSafeBoundary
			}
			return Checkpoint{Seq: t.lastSafeSeq, Unsafe: false, Snapshot: cloneSnapshot(t.lastSafeSnap)}, nil
		}
		snap, err := t.Snapshot()
		if err != nil {
			return Checkpoint{}, err
		}
		return Checkpoint{Seq: t.mutationSeq, Unsafe: false, Snapshot: snap}, nil
	default:
		return Checkpoint{}, fmt.Errorf("unsupported inflight policy: %q", opts.Policy)
	}
}

func RestoreCheckpoint(cp Checkpoint, opts RestoreOptions) (*Thread, error) {
	if cp.Unsafe && !opts.AllowUnsafe {
		return nil, ErrRestoreUnsafeRequiresExecutor
	}
	t, err := RestoreThreadSnapshot(cp.Snapshot)
	if err != nil {
		return nil, err
	}
	t.mutationSeq = cp.Seq
	if !cp.Unsafe {
		t.lastSafeSeq = cp.Seq
		t.lastSafeSnap = cloneSnapshot(cp.Snapshot)
	}
	return t, nil
}

func RestoreFromCheckpointAndWAL(cp Checkpoint, wal []WALEvent, opts RestoreOptions) (*Thread, error) {
	t, err := RestoreCheckpoint(cp, opts)
	if err != nil {
		return nil, err
	}
	if err := t.ReplayWAL(wal); err != nil {
		return nil, err
	}
	if opts.AllowUnsafe || !t.isInflightState() {
		return t, nil
	}

	lastSafe := 0
	probe, err := RestoreCheckpoint(cp, opts)
	if err != nil {
		return nil, err
	}
	for i, ev := range wal {
		if err := probe.ReplayWAL([]WALEvent{ev}); err != nil {
			return nil, err
		}
		if !probe.isInflightState() {
			lastSafe = i + 1
		}
	}

	safe, err := RestoreCheckpoint(cp, opts)
	if err != nil {
		return nil, err
	}
	if lastSafe == 0 {
		return safe, nil
	}
	return safe, safe.ReplayWAL(wal[:lastSafe])
}

func (t *Thread) WALAfter(seq uint32) []WALEvent {
	out := make([]WALEvent, 0, len(t.wal))
	for _, ev := range t.wal {
		if ev.Seq <= seq {
			continue
		}
		out = append(out, ev)
	}
	return out
}

func (t *Thread) ReplayWAL(events []WALEvent) error {
	if len(events) == 0 {
		return nil
	}

	oldExec, oldDelegate := t.executor, t.delegate
	oldReplay := t.replayingWAL
	t.executor = nil
	t.delegate = nil
	t.replayingWAL = true
	defer func() {
		t.executor = oldExec
		t.delegate = oldDelegate
		t.replayingWAL = oldReplay
	}()

	prev := t.mutationSeq
	for _, ev := range events {
		if ev.Seq != prev+1 {
			return ErrReplayWALSequence
		}
		if err := t.applyWALEvent(ev); err != nil {
			return err
		}
		if t.mutationSeq != ev.Seq {
			return ErrReplayWALSequence
		}
		prev = ev.Seq
	}
	t.captureSafeIfIdle()
	return nil
}

func itemToSnapshotItem(v Item) (SnapshotItem, error) {
	switch x := v.(type) {
	case UserText:
		return SnapshotItem{Type: "user_text", Text: string(x)}, nil
	case AssistantText:
		return SnapshotItem{Type: "assistant_text", Text: string(x)}, nil
	case AssistantInstruction:
		return SnapshotItem{Type: "assistant_instruction", Text: string(x)}, nil
	case ToolCallChunk:
		return SnapshotItem{Type: "tool_call_chunk", ID: x.CallID, Name: x.Name, Args: x.PayloadDelta}, nil
	case ToolCall:
		return SnapshotItem{Type: "tool_call", ID: x.CallID, Name: x.Name, Args: x.Payload}, nil
	case ToolCallResolving:
		return SnapshotItem{Type: "tool_call_resolving", ID: x.CallID}, nil
	case ToolCallStarted:
		return SnapshotItem{
			Type:     "tool_call_started",
			ID:       x.CallID,
			Mode:     string(x.Continue),
			Recovery: string(x.Recovery),
		}, nil
	case ToolCallResultable:
		data, err := encodeToolData(x.ToolData())
		if err != nil {
			return SnapshotItem{}, err
		}
		return SnapshotItem{
			Type:   "tool_result",
			ID:     x.ToolCallID(),
			Output: x.ToolOutput(),
			Data:   data,
		}, nil
	case ToolsSnapshot:
		snap := cloneToolsSnapshot(x)
		return SnapshotItem{Type: "tool_snapshot", Tools: &snap}, nil
	case SendItem:
		return SnapshotItem{Type: "send"}, nil
	default:
		return SnapshotItem{}, fmt.Errorf("unsupported item type for snapshot: %T", v)
	}
}

func snapshotItemToItem(raw SnapshotItem) (Item, error) {
	switch raw.Type {
	case "user_text":
		return UserText(raw.Text), nil
	case "assistant_text":
		return AssistantText(raw.Text), nil
	case "assistant_instruction":
		return AssistantInstruction(raw.Text), nil
	case "tool_call_chunk":
		return ToolCallChunk{CallID: raw.ID, Name: raw.Name, PayloadDelta: raw.Args}, nil
	case "tool_call":
		return ToolCall{CallID: raw.ID, Name: raw.Name, Payload: raw.Args}, nil
	case "tool_call_resolving":
		return ToolCallResolving{CallID: raw.ID}, nil
	case "tool_call_started":
		return ToolCallStarted{
			CallID:   raw.ID,
			Continue: ToolContinue(raw.Mode),
			Recovery: ToolRecovery(raw.Recovery),
		}, nil
	case "tool_result":
		data, err := decodeToolData(raw.Data)
		if err != nil {
			return nil, fmt.Errorf("tool result data: %w", err)
		}
		return ToolCallResult{CallID: raw.ID, Output: raw.Output, Data: data}, nil
	case "tool_snapshot":
		if raw.Tools == nil {
			return ToolsSnapshot{}, nil
		}
		return cloneToolsSnapshot(*raw.Tools), nil
	case "send":
		return SendItem{}, nil
	default:
		return nil, fmt.Errorf("unsupported snapshot item type: %q", raw.Type)
	}
}

func isKnownState(v State) bool {
	switch v {
	case StateIdle, StateConstructLLMRequest, StateReceivingStream, StateStreamComplete:
		return true
	default:
		return false
	}
}

func encodeToolData(data map[string]any) (string, error) {
	if len(data) == 0 {
		return "", nil
	}
	buf, err := json.Marshal(data)
	if err != nil {
		return "", fmt.Errorf("marshal tool result data: %w", err)
	}
	return string(buf), nil
}

func decodeToolData(raw string) (map[string]any, error) {
	if raw == "" {
		return nil, nil
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return nil, fmt.Errorf("unmarshal tool result data: %w", err)
	}
	return data, nil
}

func cloneData(data map[string]any) map[string]any {
	return maps.Clone(data)
}

func indexOfNode(index map[*item[Item]]int, n *item[Item]) int {
	if n == nil {
		return nilItemIndex
	}
	i, ok := index[n]
	if !ok {
		return nilItemIndex
	}
	return i
}

func nodeAt(nodes []*item[Item], idx int) (*item[Item], error) {
	if idx == nilItemIndex {
		return nil, nil
	}
	if idx < 0 || idx >= len(nodes) {
		return nil, fmt.Errorf("index %d out of range", idx)
	}
	return nodes[idx], nil
}

func cloneSnapshot(s ThreadSnapshot) ThreadSnapshot {
	items := make([]SnapshotItem, 0, len(s.Items))
	for _, item := range s.Items {
		if item.Tools != nil {
			snap := cloneToolsSnapshot(*item.Tools)
			item.Tools = &snap
		}
		items = append(items, item)
	}
	return ThreadSnapshot{
		Version:         s.Version,
		State:           s.State,
		Items:           items,
		IPIndex:         s.IPIndex,
		QueueStartIndex: s.QueueStartIndex,
		StreamInsIndex:  s.StreamInsIndex,
	}
}

func (t *Thread) appendWAL(op string, item Item) {
	if t.replayingWAL {
		return
	}
	ev := WALEvent{Seq: t.mutationSeq, Op: op}
	if op == walOpQueueItem || op == walOpQueueItemBeforeSend || op == walOpAppendStreamItem {
		raw, err := itemToSnapshotItem(item)
		if err != nil {
			panic("threads append wal serialize failed: " + err.Error())
		}
		ev.Item = raw
	}
	t.wal = append(t.wal, ev)

	if t.store == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			panic(fmt.Sprintf("threads append wal durable store append failed: %v", r))
		}
	}()
	t.store.AppendWALDiff([]WALEvent{ev})
}

func (t *Thread) applyWALEvent(ev WALEvent) error {
	switch ev.Op {
	case walOpQueueItem:
		if ev.Item.Type == "" {
			return fmt.Errorf("wal queue_item missing item")
		}
		v, err := snapshotItemToItem(ev.Item)
		if err != nil {
			return err
		}
		t.QueueItem(v)
		return nil
	case walOpQueueItemBeforeSend:
		if ev.Item.Type == "" {
			return fmt.Errorf("wal queue_item_before_send missing item")
		}
		v, err := snapshotItemToItem(ev.Item)
		if err != nil {
			return err
		}
		if !t.queueBeforePendingSend(v) {
			t.QueueItem(v)
		}
		return nil
	case walOpBeginStream:
		return t.beginStreaming()
	case walOpAppendStreamItem:
		if ev.Item.Type == "" {
			return fmt.Errorf("wal append_stream_item missing item")
		}
		v, err := snapshotItemToItem(ev.Item)
		if err != nil {
			return err
		}
		return t.appendStreamItem(v)
	case walOpEndStream:
		return t.endStreaming()
	default:
		return fmt.Errorf("unsupported wal op: %q", ev.Op)
	}
}

func (t *Thread) isInflightState() bool {
	s := t.State()
	return s == StateConstructLLMRequest || s == StateReceivingStream || s == StateStreamComplete
}

func (t *Thread) waitUntilSafe(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	if timeout <= 0 {
		deadline = time.Now().Add(5 * time.Second)
	}
	for t.isInflightState() {
		if time.Now().After(deadline) {
			return ErrCheckpointWaitTimeout
		}
		time.Sleep(2 * time.Millisecond)
	}
	return nil
}

func (t *Thread) captureSafeIfIdle() {
	if t.State() != StateIdle {
		return
	}
	t.captureSafeSnapshot()
}

func (t *Thread) captureSafeSnapshot() {
	snap, err := t.Snapshot()
	if err != nil {
		return
	}
	t.lastSafeSnap = snap
	t.lastSafeSeq = t.mutationSeq
}
