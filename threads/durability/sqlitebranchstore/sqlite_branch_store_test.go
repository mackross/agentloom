package sqlitebranchstore

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/mackross/agentloom/threads"
)

func TestSQLiteBranchStoreBranchesListAndPersist(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "threads.sqlite3")
	store, err := OpenSQLiteBranchStore(path, SQLiteBranchStoreOptions{})
	if err != nil {
		t.Fatalf("OpenSQLiteBranchStore: %v", err)
	}
	defer store.Close()

	root, err := store.CreateBranch(ctx, threads.BranchCreateOptions{ID: "root", Label: "root"})
	if err != nil {
		t.Fatalf("CreateBranch root: %v", err)
	}
	rootCP, _ := root.Durable.Load()
	ephemeral, err := store.BranchFromCheckpoint(ctx, root, threads.BranchFromCheckpointOptions{ID: "try-1", Kind: threads.BranchKindEphemeral, Label: "try one", Checkpoint: rootCP})
	if err != nil {
		t.Fatalf("BranchFromCheckpoint ephemeral: %v", err)
	}
	thread, err := threads.RestoreCheckpoint(rootCP, threads.RestoreOptions{})
	if err != nil {
		t.Fatalf("restore root checkpoint: %v", err)
	}
	thread.SetDurableStore(ephemeral.Durable)
	thread.QueueItem(threads.UserText("experiment"))
	saveCP, err := thread.Checkpoint(threads.CheckpointOptions{Policy: threads.InflightSkip})
	if err != nil {
		t.Fatalf("checkpoint ephemeral: %v", err)
	}
	saved, err := store.BranchFromCheckpoint(ctx, ephemeral, threads.BranchFromCheckpointOptions{ID: "saved", Kind: threads.BranchKindDurable, Label: "saved experiment", Checkpoint: saveCP})
	if err != nil {
		t.Fatalf("BranchFromCheckpoint saved: %v", err)
	}
	if got := saved.Record.RootID(); got != "root" {
		t.Fatalf("saved RootID = %q, want root", got)
	}
	if got := saved.Record.ParentID(); got != "try-1" {
		t.Fatalf("saved ParentID = %q, want try-1", got)
	}
	records, err := store.ListBranches(ctx, threads.BranchFilter{RootID: "root"})
	if err != nil {
		t.Fatalf("ListBranches: %v", err)
	}
	if got, want := len(records), 3; got != want {
		t.Fatalf("ListBranches count = %d, want %d: %#v", got, want, records)
	}
	if err := saved.Close(); err != nil {
		t.Fatalf("close saved: %v", err)
	}
	if err := root.Close(); err != nil {
		t.Fatalf("close root: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	reopened, err := OpenSQLiteBranchStore(path, SQLiteBranchStoreOptions{})
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer reopened.Close()
	openedSaved, err := reopened.OpenBranch(ctx, "saved", threads.BranchOpenOptions{})
	if err != nil {
		t.Fatalf("OpenBranch saved after reopen: %v", err)
	}
	cp, wal := openedSaved.Durable.Load()
	restored, err := threads.RestoreFromCheckpointAndWAL(cp, wal, threads.RestoreOptions{})
	if err != nil {
		t.Fatalf("restore saved: %v", err)
	}
	turns := restored.CompletedTurns()
	if len(turns) != 1 || turns[0].Role() != threads.TurnUser || turns[0].Text() != "experiment" {
		t.Fatalf("saved turns = %#v, want user experiment", turns)
	}
}

func TestSQLiteBranchStoreDurableLease(t *testing.T) {
	ctx := context.Background()
	store, err := OpenSQLiteBranchStore(filepath.Join(t.TempDir(), "threads.sqlite3"), SQLiteBranchStoreOptions{})
	if err != nil {
		t.Fatalf("OpenSQLiteBranchStore: %v", err)
	}
	defer store.Close()
	branch, err := store.CreateBranch(ctx, threads.BranchCreateOptions{ID: "durable"})
	if err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if _, err := store.OpenBranch(ctx, "durable", threads.BranchOpenOptions{}); !errors.Is(err, threads.ErrBranchAlreadyOpen) {
		t.Fatalf("OpenBranch while open err = %v, want ErrBranchAlreadyOpen", err)
	}
	if err := store.DeleteBranch(ctx, "durable"); !errors.Is(err, threads.ErrBranchOpen) {
		t.Fatalf("DeleteBranch while open err = %v, want ErrBranchOpen", err)
	}
	if err := branch.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	opened, err := store.OpenBranch(ctx, "durable", threads.BranchOpenOptions{})
	if err != nil {
		t.Fatalf("OpenBranch after close: %v", err)
	}
	if opened.Lease == nil {
		t.Fatalf("opened durable lease is nil")
	}
}

func TestSQLiteBranchStoreHooksAreTransactional(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "threads.sqlite3"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	wantErr := errors.New("hook failed")
	store, err := NewSQLiteBranchStore(db, SQLiteBranchStoreOptions{
		Now: func() time.Time { return time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC) },
		Hooks: SQLiteBranchStoreHooks{
			InitTx: func(ctx context.Context, tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS app_branches(id TEXT PRIMARY KEY)`)
				return err
			},
			CreateBranchTx: func(ctx context.Context, tx *sql.Tx, rec threads.BranchRecord, _ threads.Checkpoint) error {
				if _, err := tx.ExecContext(ctx, `INSERT INTO app_branches(id) VALUES(?)`, rec.ID); err != nil {
					return err
				}
				return wantErr
			},
		},
	})
	if err != nil {
		t.Fatalf("NewSQLiteBranchStore: %v", err)
	}
	if _, err := store.CreateBranch(ctx, threads.BranchCreateOptions{ID: "root"}); !errors.Is(err, wantErr) {
		t.Fatalf("CreateBranch err = %v, want hook failed", err)
	}
	if _, err := store.GetBranch(ctx, "root"); !errors.Is(err, threads.ErrBranchNotFound) {
		t.Fatalf("GetBranch after rollback err = %v, want ErrBranchNotFound", err)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM app_branches WHERE id = 'root'`).Scan(&count); err != nil {
		t.Fatalf("count app rows: %v", err)
	}
	if count != 0 {
		t.Fatalf("app branch rows = %d, want rollback", count)
	}
}

func TestSQLiteBranchStoreDeleteBranchIfIsAtomic(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "threads.sqlite3"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	deletedHookCalls := 0
	store, err := NewSQLiteBranchStore(db, SQLiteBranchStoreOptions{
		Hooks: SQLiteBranchStoreHooks{
			InitTx: func(ctx context.Context, tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, `CREATE TABLE app_branches(id TEXT PRIMARY KEY, protected INTEGER NOT NULL)`)
				return err
			},
			CreateBranchTx: func(ctx context.Context, tx *sql.Tx, rec threads.BranchRecord, _ threads.Checkpoint) error {
				_, err := tx.ExecContext(ctx, `INSERT INTO app_branches(id, protected) VALUES(?, 1)`, rec.ID)
				return err
			},
			DeleteBranchTx: func(ctx context.Context, tx *sql.Tx, rec threads.BranchRecord) error {
				deletedHookCalls++
				_, err := tx.ExecContext(ctx, `DELETE FROM app_branches WHERE id = ?`, rec.ID)
				return err
			},
		},
	})
	if err != nil {
		t.Fatalf("NewSQLiteBranchStore: %v", err)
	}
	branch, err := store.CreateBranch(ctx, threads.BranchCreateOptions{ID: "root"})
	if err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if err := branch.Close(); err != nil {
		t.Fatalf("Close branch: %v", err)
	}

	deleted, err := store.DeleteBranchIf(ctx, "root", func(ctx context.Context, tx *sql.Tx, rec threads.BranchRecord) (bool, error) {
		var protected int
		if err := tx.QueryRowContext(ctx, `SELECT protected FROM app_branches WHERE id = ?`, rec.ID).Scan(&protected); err != nil {
			return false, err
		}
		return protected == 0, nil
	})
	if err != nil {
		t.Fatalf("DeleteBranchIf protected: %v", err)
	}
	if deleted {
		t.Fatalf("DeleteBranchIf deleted protected branch")
	}
	if deletedHookCalls != 0 {
		t.Fatalf("DeleteBranchTx calls = %d, want 0", deletedHookCalls)
	}
	if _, err := store.GetBranch(ctx, "root"); err != nil {
		t.Fatalf("GetBranch after veto: %v", err)
	}

	if _, err := db.ExecContext(ctx, `UPDATE app_branches SET protected = 0 WHERE id = 'root'`); err != nil {
		t.Fatalf("unprotect app branch: %v", err)
	}
	deleted, err = store.DeleteBranchIf(ctx, "root", func(ctx context.Context, tx *sql.Tx, rec threads.BranchRecord) (bool, error) {
		var protected int
		if err := tx.QueryRowContext(ctx, `SELECT protected FROM app_branches WHERE id = ?`, rec.ID).Scan(&protected); err != nil {
			return false, err
		}
		return protected == 0, nil
	})
	if err != nil {
		t.Fatalf("DeleteBranchIf unprotected: %v", err)
	}
	if !deleted {
		t.Fatalf("DeleteBranchIf did not delete unprotected branch")
	}
	if deletedHookCalls != 1 {
		t.Fatalf("DeleteBranchTx calls = %d, want 1", deletedHookCalls)
	}
	var appRows int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM app_branches WHERE id = 'root'`).Scan(&appRows); err != nil {
		t.Fatalf("count app branches: %v", err)
	}
	if appRows != 0 {
		t.Fatalf("app branch rows = %d, want 0", appRows)
	}
}

func TestSQLiteBranchStoreRejectsUnsupportedSchemaVersion(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "threads.sqlite3"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `CREATE TABLE thread_branch_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		t.Fatalf("create meta: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO thread_branch_meta(key, value) VALUES('schema_version', '999')`); err != nil {
		t.Fatalf("seed schema version: %v", err)
	}

	if _, err := NewSQLiteBranchStore(db, SQLiteBranchStoreOptions{}); err == nil {
		t.Fatalf("NewSQLiteBranchStore succeeded with unsupported schema version")
	}
	var version string
	if err := db.QueryRowContext(ctx, `SELECT value FROM thread_branch_meta WHERE key = 'schema_version'`).Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if version != "999" {
		t.Fatalf("schema version = %q, want original unsupported version", version)
	}
}

func TestSQLiteBranchStoreAcceptsCurrentSchemaVersion(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "threads.sqlite3"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `CREATE TABLE thread_branch_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		t.Fatalf("create meta: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO thread_branch_meta(key, value) VALUES('schema_version', ?)`, sqliteBranchSchemaVersion); err != nil {
		t.Fatalf("seed schema version: %v", err)
	}
	if _, err := NewSQLiteBranchStore(db, SQLiteBranchStoreOptions{}); err != nil {
		t.Fatalf("NewSQLiteBranchStore with current version: %v", err)
	}
}

func TestSQLiteBranchStoreUsesExternalLockerAroundDurableMutations(t *testing.T) {
	ctx := context.Background()
	locker := &recordingBranchLocker{}
	store, err := OpenSQLiteBranchStore(filepath.Join(t.TempDir(), "threads.sqlite3"), SQLiteBranchStoreOptions{Locker: locker})
	if err != nil {
		t.Fatalf("OpenSQLiteBranchStore: %v", err)
	}
	defer store.Close()

	branch, err := store.CreateBranch(ctx, threads.BranchCreateOptions{ID: "root"})
	if err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	thread, err := threads.RestoreCheckpoint(mustLoadCheckpoint(t, branch), threads.RestoreOptions{})
	if err != nil {
		t.Fatalf("restore checkpoint: %v", err)
	}
	thread.SetDurableStore(branch.Durable)
	thread.QueueItem(threads.UserText("hello"))
	cp, err := thread.Checkpoint(threads.CheckpointOptions{Policy: threads.InflightSkip})
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	branch.Durable.ReplaceSnapshot(cp)
	if err := branch.Close(); err != nil {
		t.Fatalf("Close branch: %v", err)
	}
	if err := store.DeleteBranch(ctx, "root"); err != nil {
		t.Fatalf("DeleteBranch: %v", err)
	}

	got := locker.Labels()
	for _, want := range []string{"init sqlite branch store", "create branch", "append branch wal", "replace branch snapshot", "delete branch"} {
		if !slices.Contains(got, want) {
			t.Fatalf("locker labels = %#v, missing %q", got, want)
		}
	}
}

func TestSQLiteBranchStoreUsesTxRunnerForDurableMutations(t *testing.T) {
	ctx := context.Background()
	runner := &recordingTxRunner{}
	locker := &recordingBranchLocker{}
	store, err := OpenSQLiteBranchStore(filepath.Join(t.TempDir(), "threads.sqlite3"), SQLiteBranchStoreOptions{
		Locker:   locker,
		TxRunner: runner,
	})
	if err != nil {
		t.Fatalf("OpenSQLiteBranchStore: %v", err)
	}
	defer store.Close()

	branch, err := store.CreateBranch(ctx, threads.BranchCreateOptions{ID: "root"})
	if err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	thread, err := threads.RestoreCheckpoint(mustLoadCheckpoint(t, branch), threads.RestoreOptions{})
	if err != nil {
		t.Fatalf("restore checkpoint: %v", err)
	}
	thread.SetDurableStore(branch.Durable)
	thread.QueueItem(threads.UserText("hello"))
	cp, err := thread.Checkpoint(threads.CheckpointOptions{Policy: threads.InflightSkip})
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	branch.Durable.ReplaceSnapshot(cp)
	if err := branch.Close(); err != nil {
		t.Fatalf("Close branch: %v", err)
	}
	if err := store.DeleteBranch(ctx, "root"); err != nil {
		t.Fatalf("DeleteBranch: %v", err)
	}

	got := runner.Labels()
	for _, want := range []string{"init sqlite branch store", "create branch", "append branch wal", "replace branch snapshot", "delete branch"} {
		if !slices.Contains(got, want) {
			t.Fatalf("tx runner labels = %#v, missing %q", got, want)
		}
	}
	if labels := locker.Labels(); len(labels) != 0 {
		t.Fatalf("locker labels = %#v, want none when TxRunner is configured", labels)
	}
}

func TestSQLiteBranchStoreUsesExternalLeaseManagerAcrossStoreInstances(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "threads.sqlite3")
	leases := newRecordingLeaseManager()
	storeA, err := OpenSQLiteBranchStore(path, SQLiteBranchStoreOptions{LeaseManager: leases})
	if err != nil {
		t.Fatalf("open store A: %v", err)
	}
	defer storeA.Close()
	storeB, err := OpenSQLiteBranchStore(path, SQLiteBranchStoreOptions{LeaseManager: leases})
	if err != nil {
		t.Fatalf("open store B: %v", err)
	}
	defer storeB.Close()

	branchA, err := storeA.CreateBranch(ctx, threads.BranchCreateOptions{ID: "root", Owner: "owner-a"})
	if err != nil {
		t.Fatalf("CreateBranch store A: %v", err)
	}
	if _, err := storeB.OpenBranch(ctx, "root", threads.BranchOpenOptions{Owner: "owner-b"}); !errors.Is(err, threads.ErrBranchAlreadyOpen) {
		t.Fatalf("OpenBranch from store B while leased err = %v, want ErrBranchAlreadyOpen", err)
	}
	if err := branchA.Close(); err != nil {
		t.Fatalf("Close branch A: %v", err)
	}
	branchB, err := storeB.OpenBranch(ctx, "root", threads.BranchOpenOptions{Owner: "owner-b"})
	if err != nil {
		t.Fatalf("OpenBranch store B after close: %v", err)
	}
	if got := branchB.Lease.BranchID(); got != "root" {
		t.Fatalf("BranchID = %q, want root", got)
	}
	if got := leases.Owners("root"); !slices.Equal(got, []string{"owner-a", "owner-b"}) {
		t.Fatalf("lease owners = %#v, want owner-a then owner-b", got)
	}
}

func mustLoadCheckpoint(t *testing.T, branch *threads.StoredBranch) threads.Checkpoint {
	t.Helper()
	cp, _ := branch.Durable.Load()
	return cp
}

type recordingBranchLocker struct {
	mu     sync.Mutex
	labels []string
}

func (l *recordingBranchLocker) WithStoreLock(ctx context.Context, label string, fn func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	l.mu.Lock()
	l.labels = append(l.labels, label)
	defer l.mu.Unlock()
	return fn()
}

func (l *recordingBranchLocker) Labels() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.labels)
}

type recordingTxRunner struct {
	mu     sync.Mutex
	labels []string
}

func (r *recordingTxRunner) WithStoreTx(ctx context.Context, label string, db *sql.DB, fn func(*sql.Tx) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	r.labels = append(r.labels, label)
	r.mu.Unlock()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (r *recordingTxRunner) Labels() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.labels)
}

type recordingLeaseManager struct {
	mu     sync.Mutex
	open   map[threads.BranchID]bool
	owners map[threads.BranchID][]string
}

func newRecordingLeaseManager() *recordingLeaseManager {
	return &recordingLeaseManager{open: map[threads.BranchID]bool{}, owners: map[threads.BranchID][]string{}}
}

func (m *recordingLeaseManager) AcquireBranchLease(ctx context.Context, id threads.BranchID, owner string) (threads.BranchLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.open[id] {
		return nil, threads.ErrBranchAlreadyOpen
	}
	m.open[id] = true
	m.owners[id] = append(m.owners[id], owner)
	return &recordingBranchLease{manager: m, id: id}, nil
}

func (m *recordingLeaseManager) Owners(id threads.BranchID) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.owners[id])
}

type recordingBranchLease struct {
	manager *recordingLeaseManager
	id      threads.BranchID
	once    sync.Once
}

func (l *recordingBranchLease) BranchID() threads.BranchID { return l.id }

func (l *recordingBranchLease) Close() error {
	l.once.Do(func() {
		l.manager.mu.Lock()
		delete(l.manager.open, l.id)
		l.manager.mu.Unlock()
	})
	return nil
}
