package threads

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

var (
	ErrBranchNotFound        = errors.New("threads branch not found")
	ErrBranchAlreadyExists   = errors.New("threads branch already exists")
	ErrInvalidBranchKind     = errors.New("threads invalid branch kind")
	ErrBranchAlreadyOpen     = errors.New("threads branch already open")
	ErrBranchOpen            = errors.New("threads branch is open")
	ErrBranchParentRequired  = errors.New("threads branch parent required")
	ErrBranchCheckpointEmpty = errors.New("threads branch checkpoint is empty")
)

// MemoryBranchStore is a process-local BranchStore implementation. It is useful
// for tests, examples, and ephemeral branches. Durable-kind branches created in
// this store still use in-memory data; BranchKindDurable only affects branch
// metadata and lease behavior.
type MemoryBranchStore struct {
	mu      sync.Mutex
	nextID  uint64
	records map[BranchID]BranchRecord
	stores  map[BranchID]*MemoryDurableStore
	open    map[BranchID]bool
}

// NewMemoryBranchStore returns an empty in-memory BranchStore.
func NewMemoryBranchStore() *MemoryBranchStore {
	return &MemoryBranchStore{
		records: make(map[BranchID]BranchRecord),
		stores:  make(map[BranchID]*MemoryDurableStore),
		open:    make(map[BranchID]bool),
	}
}

func (s *MemoryBranchStore) CreateBranch(ctx context.Context, opts BranchCreateOptions) (*StoredBranch, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	kind, err := validateBranchKind(opts.Kind)
	if err != nil {
		return nil, err
	}
	cp, err := newThread().Checkpoint(CheckpointOptions{Policy: InflightSkip})
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	id, err := s.allocateIDLocked(opts.ID)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	rec := BranchRecord{
		ID:        id,
		Kind:      kind,
		Label:     opts.Label,
		CreatedAt: now,
		UpdatedAt: now,
	}
	s.records[id] = cloneBranchRecord(rec)
	s.stores[id] = NewMemoryDurableStore(cp)
	return s.branchLocked(rec, opts.Owner, false)
}

func (s *MemoryBranchStore) OpenBranch(ctx context.Context, id BranchID, opts BranchOpenOptions) (*StoredBranch, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[id]
	if !ok {
		return nil, ErrBranchNotFound
	}
	return s.branchLocked(rec, opts.Owner, opts.ReadOnly)
}

func (s *MemoryBranchStore) BranchFromCheckpoint(ctx context.Context, parent *StoredBranch, opts BranchFromCheckpointOptions) (*StoredBranch, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if parent == nil {
		return nil, ErrBranchParentRequired
	}
	kind, err := validateBranchKind(opts.Kind)
	if err != nil {
		return nil, err
	}
	if opts.Checkpoint.Snapshot.Version == 0 {
		return nil, ErrBranchCheckpointEmpty
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	id, err := s.allocateIDLocked(opts.ID)
	if err != nil {
		return nil, err
	}
	ancestors := append(cloneBranchRefs(parent.Record.Ancestors), parent.Record.Ref())
	now := time.Now()
	rec := BranchRecord{
		ID:              id,
		Kind:            kind,
		Ancestors:       ancestors,
		SourceTurnIndex: opts.SourceTurnIndex,
		SourceTurnRole:  opts.SourceTurnRole,
		SourceSeq:       opts.SourceSeq,
		SourceHeadSeq:   opts.SourceHeadSeq,
		Label:           opts.Label,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	s.records[id] = cloneBranchRecord(rec)
	s.stores[id] = NewMemoryDurableStore(opts.Checkpoint)
	return s.branchLocked(rec, opts.Owner, false)
}

func (s *MemoryBranchStore) GetBranch(ctx context.Context, id BranchID) (BranchRecord, error) {
	if err := ctx.Err(); err != nil {
		return BranchRecord{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[id]
	if !ok {
		return BranchRecord{}, ErrBranchNotFound
	}
	return cloneBranchRecord(rec), nil
}

func (s *MemoryBranchStore) ListBranches(ctx context.Context, filter BranchFilter) ([]BranchRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	kind := normalizeBranchKind(filter.Kind)
	filterKind := filter.Kind != ""
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]BranchRecord, 0, len(s.records))
	for _, rec := range s.records {
		if filter.RootID != "" && rec.RootID() != filter.RootID {
			continue
		}
		if filter.ParentID != "" && rec.ParentID() != filter.ParentID {
			continue
		}
		if filterKind && normalizeBranchKind(rec.Kind) != kind {
			continue
		}
		if filter.Status != "" && rec.Status != filter.Status {
			continue
		}
		out = append(out, cloneBranchRecord(rec))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out, nil
}

func (s *MemoryBranchStore) DeleteBranch(ctx context.Context, id BranchID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.records[id]; !ok {
		return ErrBranchNotFound
	}
	if s.open[id] {
		return ErrBranchOpen
	}
	delete(s.records, id)
	delete(s.stores, id)
	return nil
}

func (s *MemoryBranchStore) branchLocked(rec BranchRecord, _ string, readOnly bool) (*StoredBranch, error) {
	store := s.stores[rec.ID]
	if store == nil {
		return nil, ErrBranchNotFound
	}
	var lease BranchLease
	if normalizeBranchKind(rec.Kind) == BranchKindDurable && !readOnly {
		if s.open[rec.ID] {
			return nil, ErrBranchAlreadyOpen
		}
		s.open[rec.ID] = true
		lease = &memoryBranchLease{store: s, id: rec.ID}
	}
	return &StoredBranch{Record: cloneBranchRecord(rec), Lease: lease, Durable: store}, nil
}

func (s *MemoryBranchStore) allocateIDLocked(requested BranchID) (BranchID, error) {
	if requested != "" {
		if _, exists := s.records[requested]; exists {
			return "", ErrBranchAlreadyExists
		}
		return requested, nil
	}
	for {
		s.nextID++
		id := BranchID(fmt.Sprintf("branch-%d", s.nextID))
		if _, exists := s.records[id]; !exists {
			return id, nil
		}
	}
}

type memoryBranchLease struct {
	mu     sync.Mutex
	store  *MemoryBranchStore
	id     BranchID
	closed bool
}

func (l *memoryBranchLease) BranchID() BranchID { return l.id }

func (l *memoryBranchLease) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	l.store.mu.Lock()
	defer l.store.mu.Unlock()
	delete(l.store.open, l.id)
	return nil
}

func validateBranchKind(kind BranchKind) (BranchKind, error) {
	kind = normalizeBranchKind(kind)
	switch kind {
	case BranchKindDurable, BranchKindEphemeral:
		return kind, nil
	default:
		return "", ErrInvalidBranchKind
	}
}

func cloneBranchRecord(rec BranchRecord) BranchRecord {
	rec.Kind = normalizeBranchKind(rec.Kind)
	rec.Ancestors = cloneBranchRefs(rec.Ancestors)
	return rec
}

func cloneBranchRefs(refs []BranchRef) []BranchRef {
	if len(refs) == 0 {
		return nil
	}
	out := make([]BranchRef, len(refs))
	copy(out, refs)
	for i := range out {
		out[i].Kind = normalizeBranchKind(out[i].Kind)
	}
	return out
}
