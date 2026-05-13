package threads

import (
	"context"
	"time"
)

// BranchID identifies one branch stream in a BranchStore.
type BranchID string

// BranchKind describes whether a branch is persisted or process-local.
type BranchKind string

const (
	// BranchKindDurable branches use a persistent DurableStore and should be opened
	// under a writer lease.
	BranchKindDurable BranchKind = "durable"
	// BranchKindEphemeral branches use a process-local in-memory DurableStore and
	// do not need a writer lease.
	BranchKindEphemeral BranchKind = "ephemeral"
)

// BranchRef is one entry in a branch's logical ancestry path.
type BranchRef struct {
	ID BranchID
	// Kind is the referenced branch kind at the time the ref was recorded.
	Kind BranchKind
	// Label is an optional display snapshot of the referenced branch label.
	Label string
}

// BranchRecord is storage metadata for a branch.
//
// It is intentionally only metadata: restore must use the branch-local Durable
// store returned by OpenBranch/CreateBranch/BranchFromCheckpoint, not Ancestors
// or any other lineage field. Ancestors are logical provenance and may include
// ephemeral branch IDs that do not exist in durable storage.
type BranchRecord struct {
	// ID is the branch stream identifier.
	ID BranchID
	// Kind says whether this branch is durable or ephemeral.
	Kind BranchKind
	// Ancestors is the canonical logical path from root to immediate parent,
	// excluding this branch. Root, parent, depth, and nearest durable ancestor are
	// derived from this slice.
	Ancestors []BranchRef

	// SourceTurnIndex is the CompletedTurns index selected when this branch was
	// created. It is caller/UI metadata and is not used to restore the branch.
	SourceTurnIndex int
	// SourceTurnRole is the role of the selected source turn. It is caller/UI
	// metadata and is not used to restore the branch.
	SourceTurnRole TurnRole
	// SourceSeq is the checkpoint sequence materialized from the selected turn.
	SourceSeq uint32
	// SourceHeadSeq is the parent branch head sequence at branch creation time.
	// It lets callers tell whether the parent later moved on.
	SourceHeadSeq uint32

	// Label is caller-owned display text for the branch.
	Label string
	// Status is store/application-owned lifecycle metadata, such as "active" or
	// "closed". The threads package does not interpret it except for filtering.
	Status string

	// CreatedAt and UpdatedAt are store-maintained timestamps for display and
	// ordering.
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Ref returns a logical reference to this branch suitable for appending to a
// child's Ancestors slice.
func (r BranchRecord) Ref() BranchRef {
	return BranchRef{ID: r.ID, Kind: normalizeBranchKind(r.Kind), Label: r.Label}
}

// RootRef returns the logical root branch. For a root record, the root is the
// record itself.
func (r BranchRecord) RootRef() BranchRef {
	if len(r.Ancestors) == 0 {
		return r.Ref()
	}
	return r.Ancestors[0]
}

// RootID returns the logical root branch ID. For a root record, it returns ID.
func (r BranchRecord) RootID() BranchID { return r.RootRef().ID }

// ParentRef returns the logical immediate parent. The boolean is false for root
// records.
func (r BranchRecord) ParentRef() (BranchRef, bool) {
	if len(r.Ancestors) == 0 {
		return BranchRef{}, false
	}
	return r.Ancestors[len(r.Ancestors)-1], true
}

// ParentID returns the logical immediate parent branch ID, or empty for roots.
func (r BranchRecord) ParentID() BranchID {
	parent, ok := r.ParentRef()
	if !ok {
		return ""
	}
	return parent.ID
}

// NearestDurableAncestorID returns the nearest durable branch in Ancestors, or
// empty when there is none. For durable roots with no ancestors, it returns ID.
func (r BranchRecord) NearestDurableAncestorID() BranchID {
	for i := len(r.Ancestors) - 1; i >= 0; i-- {
		if normalizeBranchKind(r.Ancestors[i].Kind) == BranchKindDurable {
			return r.Ancestors[i].ID
		}
	}
	if normalizeBranchKind(r.Kind) == BranchKindDurable && len(r.Ancestors) == 0 {
		return r.ID
	}
	return ""
}

// BranchFilter selects records for ListBranches. Zero fields mean no filter.
type BranchFilter struct {
	// RootID limits results to a branch family, derived from BranchRecord.Ancestors.
	RootID BranchID
	// ParentID limits results to records whose logical immediate parent has this ID.
	ParentID BranchID
	// Kind limits results to durable or ephemeral branches.
	Kind BranchKind
	// Status limits results to a store/application lifecycle status.
	Status string
	// Limit caps returned records. Zero means store default or no explicit limit.
	Limit int
}

// BranchOpenOptions controls opening an existing branch handle.
type BranchOpenOptions struct {
	// Owner identifies the caller for lease implementations. It is not interpreted
	// by threads. Ephemeral branches may ignore it.
	Owner string
}

// BranchCreateOptions controls creating a new root branch.
type BranchCreateOptions struct {
	// ID optionally requests a branch id. Stores may ignore or reject caller ids.
	ID BranchID
	// Kind selects durable or ephemeral storage. Empty means durable.
	Kind BranchKind
	// Owner identifies the caller for the returned durable branch lease. Ephemeral
	// branches do not require a lease and may ignore it.
	Owner string
	// Label is caller-owned display text for the new root branch.
	Label string
}

// BranchFromCheckpointOptions controls creating a child branch whose base is a
// materialized checkpoint and whose WAL starts empty.
type BranchFromCheckpointOptions struct {
	// ID optionally requests a branch id. Stores may ignore or reject caller ids.
	ID BranchID
	// Kind selects durable or ephemeral storage. Empty means durable.
	Kind BranchKind
	// Owner identifies the caller for the returned durable child branch lease.
	// Ephemeral branches do not require a lease and may ignore it.
	Owner string
	// Label is caller-owned display text for the child branch.
	Label string

	// Checkpoint is the complete branch-local base for the child. Restoring the
	// child must not require reading the parent branch.
	Checkpoint Checkpoint

	// Source* fields are caller/UI lineage metadata copied into BranchRecord. The
	// store records them but must not use them for restoring the child branch.
	SourceTurnIndex int
	SourceTurnRole  TurnRole
	SourceSeq       uint32
	SourceHeadSeq   uint32
}

// BranchLease is the writer lease for a durable branch. Implementations should
// allow many different durable branch leases in one process but reject
// concurrent writer leases for the same durable branch. Ephemeral branches do
// not need leases and should return a nil Lease.
type BranchLease interface {
	BranchID() BranchID
	Close() error
}

// StoredBranch is an opened branch storage handle: metadata, optional writer
// lease, and the branch-local durable store. It does not own a live Thread or
// EventLoop.
type StoredBranch struct {
	// Record is the branch metadata as known when the branch was opened/created.
	Record BranchRecord
	// Lease must be closed when the caller is done writing a durable branch. It is
	// nil for ephemeral branches.
	Lease BranchLease
	// Durable stores exactly one branch's checkpoint and WAL. For ephemeral
	// branches this is typically an in-memory implementation.
	Durable DurableStore
}

func (b *StoredBranch) Close() error {
	if b == nil || b.Lease == nil {
		return nil
	}
	return b.Lease.Close()
}

// Load restores this stored branch's current durable head into a live Branch.
func (b *StoredBranch) Load(opts RestoreOptions) (*Branch, error) {
	if b == nil || b.Durable == nil {
		return nil, ErrBranchNotFound
	}
	cp, wal := b.Durable.Load()
	thread, err := RestoreFromCheckpointAndWAL(cp, wal, opts)
	if err != nil {
		return nil, err
	}
	branch := &Branch{stored: b, idleCh: make(chan struct{}), Thread: thread}
	thread.SetDelegate(branch)
	return branch, nil
}

// Branch is a loaded branch: an opened stored branch plus its restored Thread.
// Thread is embedded so callers can use Thread methods directly on Branch.
type Branch struct {
	stored        *StoredBranch
	eventLoop     *EventLoop
	eventLoopDone chan error
	delegate      ThreadDelegate
	idleCh        chan struct{}
	*Thread
}

func (b *Branch) Close() error {
	if b == nil {
		return nil
	}
	if b.eventLoop != nil {
		_ = b.eventLoop.Close()
	}
	if b.eventLoopDone != nil {
		<-b.eventLoopDone
	}
	if b.stored == nil {
		return nil
	}
	return b.stored.Close()
}

func (b *Branch) ID() BranchID {
	if b == nil || b.stored == nil {
		return ""
	}
	return b.stored.Record.ID
}

func (b *Branch) Record() BranchRecord {
	if b == nil || b.stored == nil {
		return BranchRecord{}
	}
	return cloneBranchRecord(b.stored.Record)
}

func (b *Branch) SetDelegate(d ThreadDelegate) {
	if b == nil {
		return
	}
	b.delegate = d
	if b.Thread != nil {
		b.Thread.SetDelegate(b)
	}
}

func (b *Branch) RunOnEventLoop(ctx context.Context, fn func(*Thread) error) error {
	if b == nil || b.eventLoop == nil {
		return ErrEventLoopClosed
	}
	return b.eventLoop.Do(ctx, fn)
}

func (b *Branch) WaitUntilIdle(ctx context.Context) error {
	if b == nil || b.eventLoop == nil {
		return ErrEventLoopClosed
	}
	idle := false
	if err := b.RunOnEventLoop(ctx, func(t *Thread) error {
		idle = t.State() == StateIdle
		return nil
	}); err != nil {
		return err
	}
	if idle {
		return nil
	}
	ch := b.idleCh
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *Branch) OnThreadIdle(t *Thread) {
	if b != nil {
		if b.delegate != nil {
			b.delegate.OnThreadIdle(t)
		}
		close(b.idleCh)
		b.idleCh = make(chan struct{})
	}
}

func (b *Branch) OnThreadRequest(t *Thread) {
	if b != nil && b.delegate != nil {
		b.delegate.OnThreadRequest(t)
	}
}

// BranchStore creates, opens, lists, and deletes branch streams. It is
// storage-only: it must not own live threads, event loops, executors, tools,
// delegates, or goroutines.
type BranchStore interface {
	// CreateBranch creates a new root branch and returns it opened. Durable roots
	// should include a writer lease; ephemeral roots should not.
	CreateBranch(ctx context.Context, opts BranchCreateOptions) (*StoredBranch, error)
	// OpenBranch opens an existing branch and returns its branch-local store.
	// Durable branches should acquire a writer lease; ephemeral branches need no
	// lease.
	OpenBranch(ctx context.Context, id BranchID, opts BranchOpenOptions) (*StoredBranch, error)
	// BranchFromCheckpoint creates a child branch from opts.Checkpoint. The child
	// starts with that checkpoint as its base and an empty WAL. Its Ancestors are
	// derived from parent.Record.Ancestors plus parent.Record.Ref().
	BranchFromCheckpoint(ctx context.Context, parent *StoredBranch, opts BranchFromCheckpointOptions) (*StoredBranch, error)
	GetBranch(ctx context.Context, id BranchID) (BranchRecord, error)
	ListBranches(ctx context.Context, filter BranchFilter) ([]BranchRecord, error)
	DeleteBranch(ctx context.Context, id BranchID) error
}

func normalizeBranchKind(kind BranchKind) BranchKind {
	if kind == "" {
		return BranchKindDurable
	}
	return kind
}
