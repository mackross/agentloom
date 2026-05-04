package threads

import "sync"

// MemoryDurableStore is an in-memory DurableStore. It is useful for ephemeral
// branches and tests. All data is process-local and disappears when the store is
// dropped.
type MemoryDurableStore struct {
	mu  sync.Mutex
	cp  Checkpoint
	wal []WALEvent
}

// NewMemoryDurableStore returns an in-memory branch-local store initialized with
// cp as its base checkpoint and an empty WAL tail.
func NewMemoryDurableStore(cp Checkpoint) *MemoryDurableStore {
	return &MemoryDurableStore{cp: cloneCheckpoint(cp)}
}

func (s *MemoryDurableStore) ReplaceSnapshot(cp Checkpoint) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cp = cloneCheckpoint(cp)
	s.wal = nil
}

func (s *MemoryDurableStore) AppendWALDiff(diff []WALEvent) {
	if len(diff) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wal = append(s.wal, cloneWALEvents(diff)...)
}

func (s *MemoryDurableStore) Load() (Checkpoint, []WALEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneCheckpoint(s.cp), cloneWALEvents(s.wal)
}

func cloneCheckpoint(cp Checkpoint) Checkpoint {
	cp.Snapshot = cloneSnapshot(cp.Snapshot)
	return cp
}

func cloneWALEvents(events []WALEvent) []WALEvent {
	if len(events) == 0 {
		return nil
	}
	out := make([]WALEvent, len(events))
	copy(out, events)
	for i := range out {
		out[i].Item = cloneSnapshotItem(out[i].Item)
	}
	return out
}

func cloneSnapshotItem(item SnapshotItem) SnapshotItem {
	if item.Tools != nil {
		tools := cloneToolsSnapshot(*item.Tools)
		item.Tools = &tools
	}
	return item
}
