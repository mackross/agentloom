package threads

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
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

const serializedThreadVersion = 2
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
	walOpPatchItemMetadata   = "patch_item_metadata"
)

type WALEvent struct {
	Seq        uint32       `json:"s"`
	Op         string       `json:"o"`
	Item       SnapshotItem `json:"i,omitempty"`
	State      State        `json:"state,omitempty"`
	Target     ItemSeq      `json:"target,omitempty"`
	Metadata   string       `json:"meta,omitempty"`
	DeleteKeys []string     `json:"delete,omitempty"`
}

// ThreadSnapshot is the serialized form of a thread. Serialization is schema
// version 2 and is not compatible with the pre-item-sequence v1 schema: v1
// snapshots are rejected by RestoreThreadSnapshot/RestoreCheckpoint rather than
// silently migrated.
type ThreadSnapshot struct {
	Version         int            `json:"ver"`
	HeadSeq         uint32         `json:"seq"`
	State           State          `json:"state"`
	Items           []SnapshotItem `json:"items"`
	IPIndex         int            `json:"ip"`
	QueueStartIndex int            `json:"queue"`
	StreamInsIndex  int            `json:"stream"`
}

type SnapshotItem struct {
	Seq          ItemSeq               `json:"seq"`
	Metadata     string                `json:"meta,omitempty"` // encoded JSON object
	Type         string                `json:"kind"`
	Text         string                `json:"text,omitempty"`
	Provider     string                `json:"provider,omitempty"`
	Visibility   ReasoningVisibility   `json:"visibility,omitempty"`
	Summary      string                `json:"summary,omitempty"`
	Opaque       []byte                `json:"opaque,omitempty"`
	ID           string                `json:"id,omitempty"`
	Name         string                `json:"name,omitempty"`
	Mode         string                `json:"mode,omitempty"`
	Recovery     string                `json:"recovery,omitempty"`
	Recovered    bool                  `json:"recovered,omitempty"`
	Args         string                `json:"args,omitempty"`
	Output       string                `json:"output,omitempty"`
	Data         string                `json:"data,omitempty"`
	Tools        *ToolsSnapshot        `json:"tools,omitempty"`
	SafeRollback *ToolCallSafeRollback `json:"safe_rollback,omitempty"`
}

func (t *thread) Snapshot() (ThreadSnapshot, error) {
	nodeIndex := map[*item[Item]]int{}
	items := make([]SnapshotItem, 0)
	idx := 0
	for n := t.items.Head(); n != nil; n = n.Next {
		it, err := nodeToSnapshotItem(n)
		if err != nil {
			return ThreadSnapshot{}, err
		}
		nodeIndex[n] = idx
		items = append(items, it)
		idx++
	}

	return ThreadSnapshot{
		Version:         serializedThreadVersion,
		HeadSeq:         t.mutationSeq,
		State:           t.cb.State(),
		Items:           items,
		IPIndex:         indexOfNode(nodeIndex, t.cb.ip),
		QueueStartIndex: indexOfNode(nodeIndex, t.cb.queueStartItem),
		StreamInsIndex:  indexOfNode(nodeIndex, t.cb.streamInsertionPoint),
	}, nil
}

// RestoreThreadSnapshot restores a thread directly from a snapshot. The restored
// mutation sequence is taken from HeadSeq so a thread restored directly from a
// snapshot cannot reuse old item IDs.
func RestoreThreadSnapshot(snapshot ThreadSnapshot) (*thread, error) {
	if snapshot.Version != serializedThreadVersion {
		return nil, fmt.Errorf("unsupported thread serialization version: %d", snapshot.Version)
	}
	if !isKnownState(snapshot.State) {
		return nil, fmt.Errorf("unsupported thread state: %q", snapshot.State)
	}

	t := newThread()
	nodes := make([]*item[Item], 0, len(snapshot.Items))
	seen := make(map[ItemSeq]bool, len(snapshot.Items))
	for _, raw := range snapshot.Items {
		post, err := snapshotToCandidate(raw)
		if err != nil {
			return nil, err
		}
		if post.Seq == 0 {
			return nil, fmt.Errorf("thread snapshot item has zero sequence")
		}
		if post.Seq > ItemSeq(snapshot.HeadSeq) {
			return nil, fmt.Errorf("thread snapshot item sequence %d exceeds head seq %d", post.Seq, snapshot.HeadSeq)
		}
		if seen[post.Seq] {
			return nil, fmt.Errorf("thread snapshot duplicate item sequence %d", post.Seq)
		}
		seen[post.Seq] = true
		nodes = append(nodes, t.items.Append(post))
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
	t.mutationSeq = snapshot.HeadSeq
	t.cb.setState(snapshot.State)
	if snapshot.State == StateIdle && len(t.cb.pendingToolCalls(&t.items)) > 0 {
		t.cb.setState(StateAwaitingToolResults)
	}
	t.captureSafeIfRestorable()

	return t, nil
}

func (t *thread) Checkpoint(opts CheckpointOptions) (Checkpoint, error) {
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

func RestoreCheckpoint(cp Checkpoint, opts RestoreOptions) (*thread, error) {
	if cp.Unsafe && !opts.AllowUnsafe {
		return nil, ErrRestoreUnsafeRequiresExecutor
	}
	if cp.Seq != cp.Snapshot.HeadSeq {
		return nil, fmt.Errorf("checkpoint seq %d does not match snapshot head seq %d", cp.Seq, cp.Snapshot.HeadSeq)
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

// RestoreFromCheckpointAndWAL replays the WAL and, unless AllowUnsafe is set,
// trims content requiring recovery. Trimming retains the full WAL sequence
// high-water mark so discarded item identities cannot be reused. Persist the
// recovered checkpoint before appending new WAL; SetDurableStore does this
// when attaching the store to the recovered thread.
func RestoreFromCheckpointAndWAL(cp Checkpoint, wal []WALEvent, opts RestoreOptions) (*thread, error) {
	t, err := RestoreCheckpoint(cp, opts)
	if err != nil {
		return nil, err
	}
	if err := t.ReplayWAL(wal); err != nil {
		return nil, err
	}
	if opts.AllowUnsafe || !t.requiresRecovery() {
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
		if !probe.requiresRecovery() {
			lastSafe = i + 1
		}
	}

	safe, err := RestoreCheckpoint(cp, opts)
	if err != nil {
		return nil, err
	}
	if err := safe.ReplayWAL(wal[:lastSafe]); err != nil {
		return nil, err
	}
	// Only content rolls back. The next item must be allocated above every
	// validated event, including those omitted from the recovered transcript.
	safe.mutationSeq = t.mutationSeq
	safe.captureSafeIfIdle()
	return safe, nil
}

func (t *thread) WALAfter(seq uint32) []WALEvent {
	out := make([]WALEvent, 0, len(t.wal))
	for _, ev := range t.wal {
		if ev.Seq <= seq {
			continue
		}
		copy := ev
		copy.DeleteKeys = append([]string(nil), ev.DeleteKeys...)
		out = append(out, copy)
	}
	return out
}

func (t *thread) ReplayWAL(events []WALEvent) error {
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
		// Reject a sequence at the exhausted end of the range before the
		// arithmetic below can wraparound: prev+1 at MaxUint32 is zero, which
		// would let a zero-sequence event bypass this check and then panic in
		// the mutation helpers instead of failing closed as a replay error.
		if prev == math.MaxUint32 {
			return fmt.Errorf("%w: thread sequence exhausted at %d", ErrReplayWALSequence, prev)
		}
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

// itemToSnapshotItem converts an item payload to snapshot fields. It never
// includes Seq or Metadata; those live on the node and are added by
// nodeToSnapshotItem. PatchItemMetadata is not legal as a materialized item.
func itemToSnapshotItem(v Item) (SnapshotItem, error) {
	switch x := v.(type) {
	case UserText:
		return SnapshotItem{Type: "user_text", Text: string(x)}, nil
	case AssistantText:
		return SnapshotItem{Type: "assistant_text", Text: string(x)}, nil
	case ReasoningItem:
		return SnapshotItem{Type: "reasoning", Provider: x.Provider, ID: x.ID, Visibility: x.Visibility, Text: x.Text, Summary: x.Summary, Opaque: append([]byte(nil), x.Opaque...)}, nil
	case PatchItemMetadata:
		return SnapshotItem{}, fmt.Errorf("patch metadata is not a materialized snapshot item")
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
	case ToolCallResult:
		data, err := encodeToolData(x.Data)
		if err != nil {
			return SnapshotItem{}, err
		}
		out := SnapshotItem{
			Type:      "tool_result",
			ID:        x.CallID,
			Output:    x.Output,
			Data:      data,
			Recovered: x.Recovered,
		}
		if x.SafeRollback != nil {
			rb := *x.SafeRollback
			out.SafeRollback = &rb
		}
		return out, nil
	case ToolsSnapshot:
		snap := cloneToolsSnapshot(x)
		return SnapshotItem{Type: "tool_snapshot", Tools: &snap}, nil
	case SendItem:
		return SnapshotItem{Type: "send"}, nil
	default:
		return SnapshotItem{}, fmt.Errorf("unsupported item type for snapshot: %T", v)
	}
}

// nodeToSnapshotItem converts a materialized node to snapshot fields, adding
// the node's Seq and encoded Metadata.
func nodeToSnapshotItem(n *item[Item]) (SnapshotItem, error) {
	raw, err := itemToSnapshotItem(n.Item)
	if err != nil {
		return SnapshotItem{}, err
	}
	raw.Seq = n.Seq
	meta, err := encodeMetadata(n.Metadata)
	if err != nil {
		return SnapshotItem{}, err
	}
	raw.Metadata = meta
	return raw, nil
}

// snapshotToCandidate converts a snapshot item back to an internal posted value with
// candidate sequence and decoded metadata.
func snapshotToCandidate(raw SnapshotItem) (itemCandidate, error) {
	v, err := snapshotItemToItem(raw)
	if err != nil {
		return itemCandidate{}, err
	}
	meta, err := decodeMetadata(raw.Metadata)
	if err != nil {
		return itemCandidate{}, err
	}
	return itemCandidate{Seq: raw.Seq, Item: v, Metadata: meta}, nil
}

func snapshotItemToItem(raw SnapshotItem) (Item, error) {
	switch raw.Type {
	case "user_text":
		return UserText(raw.Text), nil
	case "assistant_text":
		return AssistantText(raw.Text), nil
	case "reasoning":
		return ReasoningItem{Provider: raw.Provider, ID: raw.ID, Visibility: raw.Visibility, Text: raw.Text, Summary: raw.Summary, Opaque: append([]byte(nil), raw.Opaque...)}, nil
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
		var safeRollback *ToolCallSafeRollback
		if raw.SafeRollback != nil {
			rb := *raw.SafeRollback
			safeRollback = &rb
		}
		return ToolCallResult{CallID: raw.ID, Output: raw.Output, Data: data, Recovered: raw.Recovered, SafeRollback: safeRollback}, nil
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
	case StateIdle, StateAwaitingToolResults, StateConstructLLMRequest, StateReceivingStream, StateStreamComplete:
		return true
	default:
		return false
	}
}

// encodeToolData serializes tool-result Data (the structured payload on
// ToolCallResult). It is separate from item metadata, which uses
// encodeMetadata; see metadata.go.
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

// decodeToolData parses persisted tool-result Data. Item metadata is parsed by
// decodeMetadata; this path stays separate so a metadata payload can never be
// mistaken for tool result data or vice versa.
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
		items = append(items, cloneSnapshotItem(item))
	}
	return ThreadSnapshot{
		Version:         s.Version,
		HeadSeq:         s.HeadSeq,
		State:           s.State,
		Items:           items,
		IPIndex:         s.IPIndex,
		QueueStartIndex: s.QueueStartIndex,
		StreamInsIndex:  s.StreamInsIndex,
	}
}

func (t *thread) appendWAL(op string, post itemCandidate) {
	if t.replayingWAL {
		return
	}
	ev := WALEvent{Seq: t.mutationSeq, Op: op}
	if isItemBearingWALOp(op) {
		raw, err := itemToSnapshotItem(post.Item)
		if err != nil {
			panic("threads append wal serialize failed: " + err.Error())
		}
		raw.Seq = post.Seq
		meta, err := encodeMetadata(post.Metadata)
		if err != nil {
			panic("threads append wal serialize metadata failed: " + err.Error())
		}
		raw.Metadata = meta
		ev.Item = raw
	}
	t.storeWAL(ev)
}

// appendWALPatch records an explicit-target metadata patch. Zero targets must be
// resolved before calling; zero is never replayed from the WAL.
func (t *thread) appendWALPatch(target ItemSeq, patch metadataPatch) {
	if t.replayingWAL {
		return
	}
	ev := WALEvent{
		Seq:        t.mutationSeq,
		Op:         walOpPatchItemMetadata,
		Target:     target,
		DeleteKeys: append([]string(nil), patch.DeleteKeys...),
	}
	encoded, err := encodeMetadata(patch.Set)
	if err != nil {
		panic("threads append wal serialize metadata failed: " + err.Error())
	}
	ev.Metadata = encoded
	t.storeWAL(ev)
}

func isItemBearingWALOp(op string) bool {
	switch op {
	case walOpQueueItem, walOpQueueItemBeforeSend, walOpAppendStreamItem:
		return true
	}
	return false
}

func (t *thread) storeWAL(ev WALEvent) {
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

func (t *thread) applyWALEvent(ev WALEvent) error {
	switch ev.Op {
	case walOpQueueItem:
		post, err := decodeWALCandidate(ev, "queue_item")
		if err != nil {
			return err
		}
		t.queueItemWithMeta(post.Seq, post.Item, post.Metadata)
		return nil
	case walOpQueueItemBeforeSend:
		post, err := decodeWALCandidate(ev, "queue_item_before_send")
		if err != nil {
			return err
		}
		if !t.queueBeforePendingSendWithMeta(post.Item, post.Metadata) {
			t.queueItemWithMeta(post.Seq, post.Item, post.Metadata)
		}
		return nil
	case walOpBeginStream:
		return t.beginStreaming()
	case walOpAppendStreamItem:
		post, err := decodeWALCandidate(ev, "append_stream_item")
		if err != nil {
			return err
		}
		return t.appendStreamItemReplay(post)
	case walOpEndStream:
		return t.endStreaming()
	case walOpPatchItemMetadata:
		if ev.Target == 0 {
			return fmt.Errorf("wal patch_item_metadata requires an explicit nonzero target")
		}
		patch, err := decodeMetadataPatch(ev.Metadata, ev.DeleteKeys)
		if err != nil {
			return fmt.Errorf("patch metadata: %w", err)
		}
		if t.findItemBySeq(ev.Target) == nil {
			return fmt.Errorf("wal patch_item_metadata target %d not found", ev.Target)
		}
		t.advanceMutationSeq()
		t.patchItem(ev.Target, patch)
		t.captureSafeIfIdle()
		return nil
	default:
		return fmt.Errorf("unsupported wal op: %q", ev.Op)
	}
}

// decodeWALCandidate decodes an item-bearing WAL event and verifies that its
// item candidate sequence exactly matches the event sequence. This is the
// fail-closed guard that prevents a reordered or corrupt WAL tail from
// assigning a different identity than the one durably recorded.
func decodeWALCandidate(ev WALEvent, op string) (itemCandidate, error) {
	post, err := snapshotToCandidate(ev.Item)
	if err != nil {
		return itemCandidate{}, err
	}
	if post.Seq != ItemSeq(ev.Seq) {
		return itemCandidate{}, fmt.Errorf("wal %s sequence %d does not match event sequence %d", op, post.Seq, ev.Seq)
	}
	return post, nil
}

// appendStreamItemReplay mirrors live appendStreamItem with a preset candidate
// sequence and no metadata patch path (those events use walOpPatchItemMetadata).
func (t *thread) appendStreamItemReplay(post itemCandidate) error {
	t.advanceMutationSeq()
	if err := t.cb.appendStreamItem(&t.items, post); err != nil {
		return err
	}
	return t.advanceWhilePossible()
}

func (t *thread) isInflightState() bool {
	s := t.State()
	return s == StateConstructLLMRequest || s == StateReceivingStream || s == StateStreamComplete
}

func (t *thread) requiresRecovery() bool {
	if t.isInflightState() {
		return true
	}
	for _, p := range t.cb.pendingToolCalls(&t.items) {
		if p.resolving || p.started {
			return true
		}
	}
	return false
}

func (t *thread) waitUntilSafe(timeout time.Duration) error {
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

func (t *thread) captureSafeIfIdle() { t.captureSafeIfRestorable() }

func (t *thread) captureSafeIfRestorable() {
	if t.State() != StateIdle && t.State() != StateAwaitingToolResults {
		return
	}
	t.captureSafeSnapshot()
}

func (t *thread) captureSafeSnapshot() {
	snap, err := t.Snapshot()
	if err != nil {
		return
	}
	t.lastSafeSnap = snap
	t.lastSafeSeq = t.mutationSeq
}
