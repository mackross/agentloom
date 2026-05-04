package sqlitebranchstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/mackross/agentloom/threads"
	_ "modernc.org/sqlite"
)

const (
	sqliteBranchSchemaVersion = "1"
	sqliteBranchTimeLayout    = "2006-01-02T15-04-05.000000000Z"
	defaultSQLiteBusyTimeout  = 5 * time.Second
	defaultBranchStatus       = "active"
)

// SQLiteBranchStoreOptions configures a SQLiteBranchStore.
type SQLiteBranchStoreOptions struct {
	// BusyTimeout configures SQLite's busy_timeout pragma when opening or initializing
	// the store. If zero, a small default is used. If negative, no pragma is set.
	BusyTimeout time.Duration

	// StatusNew is the status written to newly-created branches. If empty, "active"
	// is used.
	StatusNew string

	// Now returns the current time. If nil, time.Now is used.
	Now func() time.Time

	// GenerateID returns an ID for branches whose create options do not specify one.
	// If nil, a timestamp-based ID is used.
	GenerateID func(time.Time) threads.BranchID

	// Locker serializes durable store transactions. If nil, transactions are only
	// serialized within this process/store instance.
	//
	// A custom Locker should provide mutual exclusion for all store instances that
	// can write the same database. For example, applications with multiple
	// processes pointing at one DB can wrap fn in an advisory file lock. The lock
	// must be held until fn returns; if fn returns an error, the store transaction
	// is rolled back before the lock is released.
	Locker SQLiteBranchStoreLocker

	// LeaseManager acquires durable branch leases. If nil, branch opens are only
	// guarded within this store instance.
	//
	// A custom LeaseManager should reject concurrent opens of the same durable
	// branch across every actor that might mutate that branch. A typical
	// implementation creates an advisory lock file per branch and returns a lease
	// that releases the file lock from Close. Close must be idempotent. The
	// returned lease's BranchID should be the acquired branch ID.
	LeaseManager SQLiteBranchLeaseManager

	// Hooks run inside the same SQL transaction as the store mutation.
	Hooks SQLiteBranchStoreHooks

	// TxRunner optionally owns the store-wide lock and SQL transaction lifecycle.
	// If nil, SQLiteBranchStore uses Locker and db.BeginTx. Applications that need
	// custom transaction begin semantics (for example SQLite BEGIN IMMEDIATE) can
	// provide a TxRunner and still receive one *sql.Tx for branch writes and hooks.
	TxRunner SQLiteBranchStoreTxRunner
}

// SQLiteBranchStoreLocker runs fn while holding a store-wide lock.
//
// The lock is outside the SQL transaction lifecycle: SQLiteBranchStore calls
// WithStoreLock, then begins/commits/rolls back the SQL transaction inside fn.
// Implementations must not return before fn completes. They should honor ctx
// while waiting for the lock, but should not cancel fn after it starts; transaction
// cancellation is handled by the context passed to SQL operations.
type SQLiteBranchStoreLocker interface {
	WithStoreLock(ctx context.Context, label string, fn func() error) error
}

// SQLiteBranchStoreTxRunner runs fn inside one store-wide locked SQL transaction.
//
// The transaction must be committed only if fn returns nil and rolled back
// otherwise. fn must receive the same *sql.Tx used for all branch-store writes
// and hook writes in that logical mutation.
type SQLiteBranchStoreTxRunner interface {
	WithStoreTx(ctx context.Context, label string, db *sql.DB, fn func(*sql.Tx) error) error
}

// SQLiteBranchLeaseManager acquires durable branch leases.
//
// A lease protects a branch's mutable head while it is open. SQLiteBranchStore
// still keeps a local in-process open map, but that map cannot protect against a
// second process or a second store instance unless the LeaseManager does. If an
// implementation detects an existing live lease for id, it should return
// threads.ErrBranchAlreadyOpen.
type SQLiteBranchLeaseManager interface {
	AcquireBranchLease(ctx context.Context, id threads.BranchID, owner string) (threads.BranchLease, error)
}

// SQLiteBranchDeletePredicate can veto deletion of a durable branch.
//
// The predicate runs inside the same SQL transaction as the branch deletion and
// DeleteBranchTx hook. It must use the passed *sql.Tx for any related database
// checks so callers can make "delete if ..." decisions atomically with the
// generic branch rows and application-owned hook rows.
type SQLiteBranchDeletePredicate func(context.Context, *sql.Tx, threads.BranchRecord) (bool, error)

// SQLiteBranchStoreHooks allows callers to extend store transactions with
// application-specific rows or side effects. Hooks are called after the built-in
// store mutation has been applied, but before commit. Returning an error rolls
// back the transaction.
//
// Hooks run inside SQLiteBranchStore's transaction. They should use the *sql.Tx
// they are passed for all related database work. Do not call back into store or
// application methods that start their own transaction or acquire the same store
// lock unless those methods can accept and use this tx; doing so can deadlock or
// split one logical mutation across multiple transactions.
//
// Integration code that keeps application-owned metadata in tables next to the
// branch tables should store only references to branch IDs and app-specific
// fields in those hook tables. When application reads need branch kind, lineage,
// source metadata, or head sequence, query or join the AgentLoom-owned
// thread_branches/checkpoint/WAL tables instead of copying that branch metadata
// into application tables.
type SQLiteBranchStoreHooks struct {
	InitTx                 func(context.Context, *sql.Tx) error
	CreateBranchTx         func(context.Context, *sql.Tx, threads.BranchRecord, threads.Checkpoint) error
	BranchFromCheckpointTx func(context.Context, *sql.Tx, threads.BranchRecord, threads.Checkpoint, threads.BranchRecord) error
	ReplaceSnapshotTx      func(context.Context, *sql.Tx, threads.BranchID, threads.Checkpoint) error
	AppendWALDiffTx        func(context.Context, *sql.Tx, threads.BranchID, []threads.WALEvent, uint32) error
	DeleteBranchTx         func(context.Context, *sql.Tx, threads.BranchRecord) error
}

// SQLiteBranchStore is a durable SQLite-backed threads.BranchStore.
type SQLiteBranchStore struct {
	db         *sql.DB
	ownsDB     bool
	txMu       sync.Mutex
	branchMu   sync.Mutex
	ephemeral  sync.Mutex
	open       map[threads.BranchID]bool
	ephemerals map[threads.BranchID]*sqliteEphemeralBranch
	opts       SQLiteBranchStoreOptions
}

type sqliteDurableStore struct {
	store *SQLiteBranchStore
	id    threads.BranchID
	mu    sync.Mutex
}

type sqliteBranchLease struct {
	mu     sync.Mutex
	store  *SQLiteBranchStore
	id     threads.BranchID
	inner  threads.BranchLease
	closed bool
}

type sqliteNoopLocker struct{}

type sqliteNoopLease struct {
	id threads.BranchID
}

type sqliteNoopLeaseManager struct{}

type sqliteEphemeralBranch struct {
	record threads.BranchRecord
	store  *threads.MemoryDurableStore
}

type sqliteBranchRow struct {
	ID              string
	Kind            string
	Ancestors       []threads.BranchRef
	SourceTurnIndex int
	SourceTurnRole  string
	SourceSeq       uint32
	SourceHeadSeq   uint32
	Label           string
	Status          string
	CreatedAt       time.Time
	UpdatedAt       time.Time
	LastSeq         uint32
}

// OpenSQLiteBranchStore opens or creates a SQLite branch store at path. The
// returned store owns the database handle and Close closes it.
func OpenSQLiteBranchStore(path string, opts SQLiteBranchStoreOptions) (*SQLiteBranchStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	store, err := NewSQLiteBranchStore(db, opts)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	store.ownsDB = true
	return store, nil
}

// NewSQLiteBranchStore initializes a branch store over an existing database
// handle. The caller remains responsible for closing db.
func NewSQLiteBranchStore(db *sql.DB, opts SQLiteBranchStoreOptions) (*SQLiteBranchStore, error) {
	if db == nil {
		return nil, errors.New("nil sqlite branch db")
	}
	store := &SQLiteBranchStore{db: db, open: map[threads.BranchID]bool{}, ephemerals: map[threads.BranchID]*sqliteEphemeralBranch{}, opts: normalizeSQLiteBranchOptions(opts)}
	if err := store.init(context.Background()); err != nil {
		return nil, err
	}
	return store, nil
}

func normalizeSQLiteBranchOptions(opts SQLiteBranchStoreOptions) SQLiteBranchStoreOptions {
	if opts.StatusNew == "" {
		opts.StatusNew = defaultBranchStatus
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.GenerateID == nil {
		opts.GenerateID = generatedSQLiteBranchID
	}
	if opts.Locker == nil {
		opts.Locker = sqliteNoopLocker{}
	}
	if opts.LeaseManager == nil {
		opts.LeaseManager = sqliteNoopLeaseManager{}
	}
	return opts
}

// Close closes the database handle if this store opened it.
func (s *SQLiteBranchStore) Close() error {
	if s == nil || !s.ownsDB {
		return nil
	}
	return s.db.Close()
}

// DurableStore returns a DurableStore for an existing durable branch ID. It does
// not acquire a branch lease; callers that may concurrently mutate the same
// branch should use OpenBranch instead.
func (s *SQLiteBranchStore) DurableStore(id threads.BranchID) threads.DurableStore {
	return &sqliteDurableStore{store: s, id: id}
}

func (s *SQLiteBranchStore) init(ctx context.Context) error {
	pragmas := []string{"PRAGMA foreign_keys = ON", "PRAGMA journal_mode = WAL", "PRAGMA synchronous = NORMAL"}
	if s.opts.BusyTimeout == 0 {
		pragmas = append(pragmas, fmt.Sprintf("PRAGMA busy_timeout = %d", defaultSQLiteBusyTimeout.Milliseconds()))
	} else if s.opts.BusyTimeout > 0 {
		pragmas = append(pragmas, fmt.Sprintf("PRAGMA busy_timeout = %d", s.opts.BusyTimeout.Milliseconds()))
	}
	for _, stmt := range pragmas {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("init sqlite branch store: %w", err)
		}
	}
	return s.withTx(ctx, "init sqlite branch store", func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, sqliteBranchSchema); err != nil {
			return err
		}
		var existingVersion string
		if err := tx.QueryRowContext(ctx, `SELECT value FROM thread_branch_meta WHERE key = 'schema_version'`).Scan(&existingVersion); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if existingVersion != "" && existingVersion != sqliteBranchSchemaVersion {
			return fmt.Errorf("unsupported sqlite branch schema version %q; expected %q", existingVersion, sqliteBranchSchemaVersion)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO thread_branch_meta(key, value) VALUES('schema_version', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, sqliteBranchSchemaVersion); err != nil {
			return err
		}
		if s.opts.Hooks.InitTx != nil {
			return s.opts.Hooks.InitTx(ctx, tx)
		}
		return nil
	})
}

const sqliteBranchSchema = `
CREATE TABLE IF NOT EXISTS thread_branch_meta (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS thread_branches (
	id TEXT PRIMARY KEY,
	kind TEXT NOT NULL,
	ancestors_json TEXT NOT NULL DEFAULT '[]',
	source_turn_index INTEGER NOT NULL DEFAULT 0,
	source_turn_role TEXT NOT NULL DEFAULT '',
	source_seq INTEGER NOT NULL DEFAULT 0,
	source_head_seq INTEGER NOT NULL DEFAULT 0,
	label TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'active',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	last_seq INTEGER NOT NULL DEFAULT 0,
	head_version INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS thread_checkpoints (
	branch_id TEXT PRIMARY KEY REFERENCES thread_branches(id) ON DELETE CASCADE,
	seq INTEGER NOT NULL,
	unsafe INTEGER NOT NULL,
	snapshot_json TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS thread_wal_events (
	branch_id TEXT NOT NULL REFERENCES thread_branches(id) ON DELETE CASCADE,
	seq INTEGER NOT NULL,
	op TEXT NOT NULL,
	event_json TEXT NOT NULL,
	created_at TEXT NOT NULL,
	PRIMARY KEY (branch_id, seq)
);

CREATE INDEX IF NOT EXISTS thread_branches_updated_idx ON thread_branches(updated_at DESC);
CREATE INDEX IF NOT EXISTS thread_wal_events_branch_seq_idx ON thread_wal_events(branch_id, seq);
`

func (s *SQLiteBranchStore) CreateBranch(ctx context.Context, opts threads.BranchCreateOptions) (*threads.Branch, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	kind, err := validateSQLiteBranchKind(opts.Kind)
	if err != nil {
		return nil, err
	}
	if kind == threads.BranchKindEphemeral {
		return s.createEphemeralRoot(ctx, opts)
	}
	now := s.opts.Now()
	id := opts.ID
	if id == "" {
		id = s.opts.GenerateID(now)
	}
	rec := threads.BranchRecord{ID: id, Kind: kind, Label: opts.Label, Status: s.opts.StatusNew, CreatedAt: now, UpdatedAt: now}
	empty := threads.New()
	cp, err := empty.Checkpoint(threads.CheckpointOptions{Policy: threads.InflightSkip})
	if err != nil {
		return nil, err
	}
	if err := s.insertDurableBranch(ctx, rec, cp, nil); err != nil {
		return nil, err
	}
	branch, err := s.OpenBranch(ctx, id, threads.BranchOpenOptions{Owner: opts.Owner})
	if err != nil {
		_ = s.DeleteBranch(ctx, id)
		return nil, err
	}
	return branch, nil
}

func (s *SQLiteBranchStore) OpenBranch(ctx context.Context, id threads.BranchID, opts threads.BranchOpenOptions) (*threads.Branch, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if branch, ok := s.openEphemeralBranch(id); ok {
		return branch, nil
	}
	s.branchMu.Lock()
	defer s.branchMu.Unlock()
	if branch, ok := s.openEphemeralBranch(id); ok {
		return branch, nil
	}
	if s.open[id] {
		return nil, threads.ErrBranchAlreadyOpen
	}
	rec, err := s.GetBranch(ctx, id)
	if err != nil {
		return nil, err
	}
	if rec.Kind != threads.BranchKindDurable {
		return nil, threads.ErrInvalidBranchKind
	}
	inner, err := s.opts.LeaseManager.AcquireBranchLease(ctx, id, opts.Owner)
	if err != nil {
		return nil, err
	}
	s.open[id] = true
	return &threads.Branch{Record: rec, Lease: &sqliteBranchLease{store: s, id: id, inner: inner}, Durable: &sqliteDurableStore{store: s, id: id}}, nil
}

func (s *SQLiteBranchStore) BranchFromCheckpoint(ctx context.Context, parent *threads.Branch, opts threads.BranchFromCheckpointOptions) (*threads.Branch, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	parentRecord, err := s.openParentBranch(parent)
	if err != nil {
		return nil, err
	}
	kind, err := validateSQLiteBranchKind(opts.Kind)
	if err != nil {
		return nil, err
	}
	ancestors := append(cloneSQLiteBranchRefs(parentRecord.Ancestors), parentRecord.Ref())
	if kind == threads.BranchKindEphemeral {
		return s.createEphemeralChild(ctx, opts, ancestors)
	}
	return s.createDurableChildFromRecord(ctx, parentRecord, opts, ancestors)
}

func (s *SQLiteBranchStore) createDurableChildFromRecord(ctx context.Context, parentRecord threads.BranchRecord, opts threads.BranchFromCheckpointOptions, ancestors []threads.BranchRef) (*threads.Branch, error) {
	now := s.opts.Now()
	id := opts.ID
	if id == "" {
		id = s.opts.GenerateID(now)
	}
	rec := threads.BranchRecord{ID: id, Kind: threads.BranchKindDurable, Ancestors: ancestors, SourceTurnIndex: opts.SourceTurnIndex, SourceTurnRole: opts.SourceTurnRole, SourceSeq: opts.SourceSeq, SourceHeadSeq: opts.SourceHeadSeq, Label: opts.Label, Status: s.opts.StatusNew, CreatedAt: now, UpdatedAt: now}
	if err := s.insertDurableBranch(ctx, rec, opts.Checkpoint, &parentRecord); err != nil {
		return nil, err
	}
	branch, err := s.OpenBranch(ctx, id, threads.BranchOpenOptions{Owner: opts.Owner})
	if err != nil {
		_ = s.DeleteBranch(ctx, id)
		return nil, err
	}
	return branch, nil
}

func (s *SQLiteBranchStore) GetBranch(ctx context.Context, id threads.BranchID) (threads.BranchRecord, error) {
	if err := ctx.Err(); err != nil {
		return threads.BranchRecord{}, err
	}
	if branch, ok := s.openEphemeralBranch(id); ok {
		return branch.Record, nil
	}
	row := s.db.QueryRowContext(ctx, `SELECT id, kind, ancestors_json, source_turn_index, source_turn_role, source_seq, source_head_seq, label, status, created_at, updated_at, last_seq FROM thread_branches WHERE id = ?`, id)
	br, err := scanSQLiteBranchRow(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return threads.BranchRecord{}, threads.ErrBranchNotFound
		}
		return threads.BranchRecord{}, err
	}
	return sqliteBranchRecord(br), nil
}

func (s *SQLiteBranchStore) ListBranches(ctx context.Context, filter threads.BranchFilter) ([]threads.BranchRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	query := `SELECT id, kind, ancestors_json, source_turn_index, source_turn_role, source_seq, source_head_seq, label, status, created_at, updated_at, last_seq FROM thread_branches WHERE 1=1`
	args := []any{}
	if filter.Status != "" {
		query += ` AND status = ?`
		args = append(args, filter.Status)
	}
	query += ` ORDER BY updated_at DESC, id`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []threads.BranchRecord
	for rows.Next() {
		br, err := scanSQLiteBranchRow(rows)
		if err != nil {
			return nil, err
		}
		rec := sqliteBranchRecord(br)
		if sqliteBranchMatchesFilter(rec, filter) {
			out = append(out, rec)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	s.ephemeral.Lock()
	for _, ephemeral := range s.ephemerals {
		rec := cloneSQLiteBranchRecord(ephemeral.record)
		if sqliteBranchMatchesFilter(rec, filter) {
			out = append(out, rec)
		}
	}
	s.ephemeral.Unlock()
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

func (s *SQLiteBranchStore) DeleteBranch(ctx context.Context, id threads.BranchID) error {
	deleted, err := s.DeleteBranchIf(ctx, id, nil)
	if err != nil {
		return err
	}
	if !deleted {
		return threads.ErrBranchNotFound
	}
	return nil
}

// DeleteBranchIf deletes id when pred is nil or pred returns true.
//
// For durable branches, pred and DeleteBranchTx run in the same SQL transaction
// as the generic branch row deletion. A false predicate leaves the branch and any
// hook-owned rows untouched and returns (false, nil). Missing durable branches
// also return (false, nil).
func (s *SQLiteBranchStore) DeleteBranchIf(ctx context.Context, id threads.BranchID, pred SQLiteBranchDeletePredicate) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.branchMu.Lock()
	defer s.branchMu.Unlock()
	s.ephemeral.Lock()
	if _, ok := s.ephemerals[id]; ok {
		if pred != nil {
			s.ephemeral.Unlock()
			return false, fmt.Errorf("delete branch %s conditionally: ephemeral branch", id)
		}
		delete(s.ephemerals, id)
		s.ephemeral.Unlock()
		return true, nil
	}
	s.ephemeral.Unlock()
	if s.open[id] {
		return false, threads.ErrBranchOpen
	}
	var rec threads.BranchRecord
	deleted := false
	if err := s.withTx(ctx, "delete branch", func(tx *sql.Tx) error {
		br, err := scanSQLiteBranchRow(tx.QueryRowContext(ctx, `SELECT id, kind, ancestors_json, source_turn_index, source_turn_role, source_seq, source_head_seq, label, status, created_at, updated_at, last_seq FROM thread_branches WHERE id = ?`, id))
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		rec = sqliteBranchRecord(br)
		if pred != nil {
			ok, err := pred(ctx, tx, rec)
			if err != nil {
				return err
			}
			if !ok {
				return nil
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM thread_branches WHERE id = ?`, id); err != nil {
			return err
		}
		if s.opts.Hooks.DeleteBranchTx != nil {
			if err := s.opts.Hooks.DeleteBranchTx(ctx, tx, rec); err != nil {
				return err
			}
		}
		deleted = true
		return nil
	}); err != nil {
		return false, err
	}
	return deleted, nil
}

func (s *SQLiteBranchStore) insertDurableBranch(ctx context.Context, rec threads.BranchRecord, cp threads.Checkpoint, parent *threads.BranchRecord) error {
	if cp.Snapshot.Version == 0 {
		return threads.ErrBranchCheckpointEmpty
	}
	snapshot, err := json.Marshal(cp.Snapshot)
	if err != nil {
		return err
	}
	ancestors, err := json.Marshal(rec.Ancestors)
	if err != nil {
		return err
	}
	now := rec.CreatedAt
	label := "create branch"
	if parent != nil {
		label = "branch from checkpoint"
	}
	return s.withTx(ctx, label, func(tx *sql.Tx) error {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM thread_branches WHERE id = ?`, rec.ID).Scan(&exists); err == nil {
			return threads.ErrBranchAlreadyExists
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO thread_branches(id, kind, ancestors_json, source_turn_index, source_turn_role, source_seq, source_head_seq, label, status, created_at, updated_at, last_seq) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, rec.ID, rec.Kind, string(ancestors), rec.SourceTurnIndex, string(rec.SourceTurnRole), rec.SourceSeq, rec.SourceHeadSeq, rec.Label, rec.Status, formatSQLiteBranchTime(now), formatSQLiteBranchTime(rec.UpdatedAt), cp.Seq); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO thread_checkpoints(branch_id, seq, unsafe, snapshot_json, updated_at) VALUES(?, ?, ?, ?, ?)`, rec.ID, cp.Seq, boolInt(cp.Unsafe), string(snapshot), formatSQLiteBranchTime(now)); err != nil {
			return err
		}
		if parent == nil {
			if s.opts.Hooks.CreateBranchTx != nil {
				return s.opts.Hooks.CreateBranchTx(ctx, tx, rec, cp)
			}
		} else if s.opts.Hooks.BranchFromCheckpointTx != nil {
			return s.opts.Hooks.BranchFromCheckpointTx(ctx, tx, rec, cp, *parent)
		}
		return nil
	})
}

func (s *SQLiteBranchStore) createEphemeralRoot(ctx context.Context, opts threads.BranchCreateOptions) (*threads.Branch, error) {
	now := s.opts.Now()
	id := opts.ID
	if id == "" {
		id = threads.BranchID("ephemeral-" + string(s.opts.GenerateID(now)))
	}
	empty := threads.New()
	cp, err := empty.Checkpoint(threads.CheckpointOptions{Policy: threads.InflightSkip})
	if err != nil {
		return nil, err
	}
	rec := threads.BranchRecord{ID: id, Kind: threads.BranchKindEphemeral, Label: opts.Label, Status: s.opts.StatusNew, CreatedAt: now, UpdatedAt: now}
	return s.putEphemeralBranch(ctx, rec, threads.NewMemoryDurableStore(cp))
}

func (s *SQLiteBranchStore) createEphemeralChild(ctx context.Context, opts threads.BranchFromCheckpointOptions, ancestors []threads.BranchRef) (*threads.Branch, error) {
	now := s.opts.Now()
	id := opts.ID
	if id == "" {
		id = threads.BranchID("ephemeral-" + string(s.opts.GenerateID(now)))
	}
	rec := threads.BranchRecord{ID: id, Kind: threads.BranchKindEphemeral, Ancestors: ancestors, SourceTurnIndex: opts.SourceTurnIndex, SourceTurnRole: opts.SourceTurnRole, SourceSeq: opts.SourceSeq, SourceHeadSeq: opts.SourceHeadSeq, Label: opts.Label, Status: s.opts.StatusNew, CreatedAt: now, UpdatedAt: now}
	return s.putEphemeralBranch(ctx, rec, threads.NewMemoryDurableStore(opts.Checkpoint))
}

func (s *SQLiteBranchStore) putEphemeralBranch(ctx context.Context, rec threads.BranchRecord, store *threads.MemoryDurableStore) (*threads.Branch, error) {
	s.branchMu.Lock()
	defer s.branchMu.Unlock()
	s.ephemeral.Lock()
	if _, exists := s.ephemerals[rec.ID]; exists {
		s.ephemeral.Unlock()
		return nil, threads.ErrBranchAlreadyExists
	}
	s.ephemeral.Unlock()
	if _, err := s.GetBranch(ctx, rec.ID); err == nil {
		return nil, threads.ErrBranchAlreadyExists
	} else if !errors.Is(err, threads.ErrBranchNotFound) {
		return nil, err
	}
	s.ephemeral.Lock()
	defer s.ephemeral.Unlock()
	if _, exists := s.ephemerals[rec.ID]; exists {
		return nil, threads.ErrBranchAlreadyExists
	}
	s.ephemerals[rec.ID] = &sqliteEphemeralBranch{record: cloneSQLiteBranchRecord(rec), store: store}
	return &threads.Branch{Record: cloneSQLiteBranchRecord(rec), Durable: store}, nil
}

func (s *SQLiteBranchStore) openEphemeralBranch(id threads.BranchID) (*threads.Branch, bool) {
	s.ephemeral.Lock()
	defer s.ephemeral.Unlock()
	branch := s.ephemerals[id]
	if branch == nil {
		return nil, false
	}
	return &threads.Branch{Record: cloneSQLiteBranchRecord(branch.record), Durable: branch.store}, true
}

func (s *SQLiteBranchStore) openParentBranch(branch *threads.Branch) (threads.BranchRecord, error) {
	if branch == nil || branch.Record.ID == "" {
		return threads.BranchRecord{}, threads.ErrBranchParentRequired
	}
	id := branch.Record.ID
	if branch.Record.Kind == threads.BranchKindEphemeral {
		s.ephemeral.Lock()
		ephemeral := s.ephemerals[id]
		s.ephemeral.Unlock()
		if ephemeral == nil || ephemeral.store != branch.Durable {
			return threads.BranchRecord{}, threads.ErrBranchParentRequired
		}
		return cloneSQLiteBranchRecord(ephemeral.record), nil
	}
	lease, ok := branch.Lease.(*sqliteBranchLease)
	if !ok || lease == nil || lease.store != s || lease.id != id || lease.closed {
		return threads.BranchRecord{}, threads.ErrBranchParentRequired
	}
	durable, ok := branch.Durable.(*sqliteDurableStore)
	if !ok || durable == nil || durable.store != s || durable.id != id {
		return threads.BranchRecord{}, threads.ErrBranchParentRequired
	}
	s.branchMu.Lock()
	open := s.open[id]
	s.branchMu.Unlock()
	if !open {
		return threads.BranchRecord{}, threads.ErrBranchParentRequired
	}
	return s.GetBranch(context.Background(), id)
}

func (d *sqliteDurableStore) ReplaceSnapshot(cp threads.Checkpoint) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.replaceSnapshot(context.Background(), cp); err != nil {
		panic("sqlite branch store replace snapshot failed: " + err.Error())
	}
}

func (d *sqliteDurableStore) AppendWALDiff(diff []threads.WALEvent) {
	if len(diff) == 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.appendWALDiff(context.Background(), diff); err != nil {
		panic("sqlite branch store append wal failed: " + err.Error())
	}
}

func (d *sqliteDurableStore) Load() (threads.Checkpoint, []threads.WALEvent) {
	d.mu.Lock()
	defer d.mu.Unlock()
	cp, wal, err := d.load(context.Background())
	if err != nil {
		panic("sqlite branch store load failed: " + err.Error())
	}
	return cp, wal
}

func (d *sqliteDurableStore) replaceSnapshot(ctx context.Context, cp threads.Checkpoint) error {
	snapshot, err := json.Marshal(cp.Snapshot)
	if err != nil {
		return err
	}
	now := d.store.opts.Now()
	return d.store.withTx(ctx, "replace branch snapshot", func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO thread_checkpoints(branch_id, seq, unsafe, snapshot_json, updated_at) VALUES(?, ?, ?, ?, ?) ON CONFLICT(branch_id) DO UPDATE SET seq = excluded.seq, unsafe = excluded.unsafe, snapshot_json = excluded.snapshot_json, updated_at = excluded.updated_at`, d.id, cp.Seq, boolInt(cp.Unsafe), string(snapshot), formatSQLiteBranchTime(now)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM thread_wal_events WHERE branch_id = ?`, d.id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE thread_branches SET updated_at = ?, last_seq = ?, head_version = head_version + 1 WHERE id = ?`, formatSQLiteBranchTime(now), cp.Seq, d.id); err != nil {
			return err
		}
		if d.store.opts.Hooks.ReplaceSnapshotTx != nil {
			return d.store.opts.Hooks.ReplaceSnapshotTx(ctx, tx, d.id, cp)
		}
		return nil
	})
}

func (d *sqliteDurableStore) appendWALDiff(ctx context.Context, diff []threads.WALEvent) error {
	now := d.store.opts.Now()
	return d.store.withTx(ctx, "append branch wal", func(tx *sql.Tx) error {
		var lastSeqRaw int64
		if err := tx.QueryRowContext(ctx, `SELECT last_seq FROM thread_branches WHERE id = ?`, d.id).Scan(&lastSeqRaw); err != nil {
			return err
		}
		lastSeq := uint32(lastSeqRaw)
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO thread_wal_events(branch_id, seq, op, event_json, created_at) VALUES(?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		appended := false
		for _, ev := range diff {
			if ev.Seq <= lastSeq {
				continue
			}
			if ev.Seq != lastSeq+1 {
				return fmt.Errorf("wal sequence gap for branch %s: got %d after %d", d.id, ev.Seq, lastSeq)
			}
			raw, err := json.Marshal(ev)
			if err != nil {
				return err
			}
			if _, err := stmt.ExecContext(ctx, d.id, ev.Seq, ev.Op, string(raw), formatSQLiteBranchTime(now)); err != nil {
				return err
			}
			lastSeq = ev.Seq
			appended = true
		}
		if !appended {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE thread_branches SET updated_at = ?, last_seq = ?, head_version = head_version + 1 WHERE id = ?`, formatSQLiteBranchTime(now), lastSeq, d.id); err != nil {
			return err
		}
		if d.store.opts.Hooks.AppendWALDiffTx != nil {
			return d.store.opts.Hooks.AppendWALDiffTx(ctx, tx, d.id, diff, lastSeq)
		}
		return nil
	})
}

func (d *sqliteDurableStore) load(ctx context.Context) (threads.Checkpoint, []threads.WALEvent, error) {
	var seq int64
	var unsafe int
	var snapshotJSON string
	if err := d.store.db.QueryRowContext(ctx, `SELECT seq, unsafe, snapshot_json FROM thread_checkpoints WHERE branch_id = ?`, d.id).Scan(&seq, &unsafe, &snapshotJSON); err != nil {
		return threads.Checkpoint{}, nil, err
	}
	var snapshot threads.ThreadSnapshot
	if err := json.Unmarshal([]byte(snapshotJSON), &snapshot); err != nil {
		return threads.Checkpoint{}, nil, fmt.Errorf("decode checkpoint snapshot: %w", err)
	}
	rows, err := d.store.db.QueryContext(ctx, `SELECT event_json FROM thread_wal_events WHERE branch_id = ? AND seq > ? ORDER BY seq`, d.id, seq)
	if err != nil {
		return threads.Checkpoint{}, nil, err
	}
	defer rows.Close()
	var wal []threads.WALEvent
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return threads.Checkpoint{}, nil, err
		}
		var ev threads.WALEvent
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			return threads.Checkpoint{}, nil, fmt.Errorf("decode wal event: %w", err)
		}
		wal = append(wal, ev)
	}
	if err := rows.Err(); err != nil {
		return threads.Checkpoint{}, nil, err
	}
	return threads.Checkpoint{Seq: uint32(seq), Unsafe: unsafe != 0, Snapshot: snapshot}, wal, nil
}

func (l *sqliteBranchLease) BranchID() threads.BranchID { return l.id }

func (l *sqliteBranchLease) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	var err error
	if l.inner != nil {
		err = l.inner.Close()
	}
	l.store.branchMu.Lock()
	delete(l.store.open, l.id)
	l.store.branchMu.Unlock()
	l.closed = true
	return err
}

func (s *SQLiteBranchStore) withTx(ctx context.Context, label string, fn func(*sql.Tx) error) error {
	if s.opts.TxRunner != nil {
		s.txMu.Lock()
		defer s.txMu.Unlock()
		return s.opts.TxRunner.WithStoreTx(ctx, label, s.db, fn)
	}
	return s.opts.Locker.WithStoreLock(ctx, label, func() error {
		s.txMu.Lock()
		defer s.txMu.Unlock()
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if err := fn(tx); err != nil {
			_ = tx.Rollback()
			return err
		}
		return tx.Commit()
	})
}

func (sqliteNoopLocker) WithStoreLock(ctx context.Context, _ string, fn func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn()
}

func (sqliteNoopLeaseManager) AcquireBranchLease(ctx context.Context, id threads.BranchID, _ string) (threads.BranchLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return sqliteNoopLease{id: id}, nil
}

func (l sqliteNoopLease) BranchID() threads.BranchID { return l.id }

func (sqliteNoopLease) Close() error { return nil }

func scanSQLiteBranchRow(row interface{ Scan(dest ...any) error }) (sqliteBranchRow, error) {
	var br sqliteBranchRow
	var ancestorsJSON, created, updated string
	var sourceSeq, sourceHeadSeq, lastSeq int64
	if err := row.Scan(&br.ID, &br.Kind, &ancestorsJSON, &br.SourceTurnIndex, &br.SourceTurnRole, &sourceSeq, &sourceHeadSeq, &br.Label, &br.Status, &created, &updated, &lastSeq); err != nil {
		return sqliteBranchRow{}, err
	}
	if ancestorsJSON != "" {
		if err := json.Unmarshal([]byte(ancestorsJSON), &br.Ancestors); err != nil {
			return sqliteBranchRow{}, err
		}
	}
	var err error
	br.CreatedAt, err = parseSQLiteBranchTime(created)
	if err != nil {
		return sqliteBranchRow{}, err
	}
	br.UpdatedAt, err = parseSQLiteBranchTime(updated)
	if err != nil {
		return sqliteBranchRow{}, err
	}
	br.SourceSeq = uint32(sourceSeq)
	br.SourceHeadSeq = uint32(sourceHeadSeq)
	br.LastSeq = uint32(lastSeq)
	return br, nil
}

func sqliteBranchRecord(br sqliteBranchRow) threads.BranchRecord {
	return threads.BranchRecord{ID: threads.BranchID(br.ID), Kind: threads.BranchKind(br.Kind), Ancestors: cloneSQLiteBranchRefs(br.Ancestors), SourceTurnIndex: br.SourceTurnIndex, SourceTurnRole: threads.TurnRole(br.SourceTurnRole), SourceSeq: br.SourceSeq, SourceHeadSeq: br.SourceHeadSeq, Label: br.Label, Status: br.Status, CreatedAt: br.CreatedAt, UpdatedAt: br.UpdatedAt}
}

func validateSQLiteBranchKind(kind threads.BranchKind) (threads.BranchKind, error) {
	if kind == "" {
		return threads.BranchKindDurable, nil
	}
	switch kind {
	case threads.BranchKindDurable, threads.BranchKindEphemeral:
		return kind, nil
	default:
		return "", threads.ErrInvalidBranchKind
	}
}

func sqliteBranchMatchesFilter(branch threads.BranchRecord, filter threads.BranchFilter) bool {
	if filter.RootID != "" && branch.RootID() != filter.RootID {
		return false
	}
	if filter.ParentID != "" && branch.ParentID() != filter.ParentID {
		return false
	}
	if filter.Kind != "" && branch.Kind != filter.Kind {
		return false
	}
	if filter.Status != "" && branch.Status != filter.Status {
		return false
	}
	return true
}

func cloneSQLiteBranchRecord(record threads.BranchRecord) threads.BranchRecord {
	record.Ancestors = cloneSQLiteBranchRefs(record.Ancestors)
	return record
}

func cloneSQLiteBranchRefs(refs []threads.BranchRef) []threads.BranchRef {
	if len(refs) == 0 {
		return nil
	}
	out := make([]threads.BranchRef, len(refs))
	copy(out, refs)
	return out
}

func formatSQLiteBranchTime(t time.Time) string { return t.UTC().Format(sqliteBranchTimeLayout) }

func generatedSQLiteBranchID(now time.Time) threads.BranchID {
	return threads.BranchID(formatSQLiteBranchTime(now))
}

func parseSQLiteBranchTime(v string) (time.Time, error) { return time.Parse(sqliteBranchTimeLayout, v) }

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
