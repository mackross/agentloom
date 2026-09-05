package sqlitebranchstore

import (
	"context"
	"database/sql"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mackross/agentloom/threads"
	"modernc.org/sqlite"
)

func init() {
	// A separate driver has none of the functions registered on the default
	// driver. Its open connections behave like a pre-fence application binary.
	sql.Register("sqlite-legacy-writer-test", &sqlite.Driver{})
}

func TestSQLiteWriteFenceRejectsAlreadyOpenLegacyWriter(t *testing.T) {
	db, path := newV1DB(t)
	seedV1Branch(t, db, "root", v1Snapshot, 10, false, v1Tail)
	legacy, err := sql.Open("sqlite-legacy-writer-test", path)
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	legacy.SetMaxOpenConns(1)
	// Prepare and execute on the old connection before migration, keeping the
	// cached SQLite statement alive while a second connection upgrades the DB.
	prepared, err := legacy.Prepare(`UPDATE thread_branches SET label = ? WHERE id = 'root'`)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	if _, err := prepared.Exec("before migration"); err != nil {
		t.Fatal(err)
	}
	store, err := OpenSQLiteBranchStore(path, SQLiteBranchStoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cp, wal := store.DurableStore("root").Load()
	assertFenced := func(err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), sqliteWriterVersionFunction) {
			t.Fatalf("legacy writer not fenced: %v", err)
		}
	}
	_, err = prepared.Exec("stale overwrite")
	assertFenced(err)
	for _, stmt := range []string{
		`INSERT INTO thread_wal_events VALUES('root', 1000, 'queue_item', '{"s":1000,"o":"queue_item","i":{"kind":"user_text","text":"stale"}}', 'old')`,
		`UPDATE thread_wal_events SET event_json = '{}' WHERE branch_id = 'root'`,
		`DELETE FROM thread_wal_events WHERE branch_id = 'root'`,
		`INSERT INTO thread_checkpoints VALUES('root', 1, 0, '{"ver":1}', 'old') ON CONFLICT(branch_id) DO UPDATE SET seq=excluded.seq,snapshot_json=excluded.snapshot_json`,
		`UPDATE thread_checkpoints SET snapshot_json = '{"ver":1}' WHERE branch_id = 'root'`,
		`DELETE FROM thread_checkpoints WHERE branch_id = 'root'`,
		`INSERT INTO thread_branches(id,kind,created_at,updated_at) VALUES('stale','durable','old','old')`,
		`UPDATE thread_branches SET last_seq = 1 WHERE id = 'root'`,
		`DELETE FROM thread_branches WHERE id = 'root'`,
		`INSERT INTO thread_branch_meta VALUES('stale','old')`,
		`UPDATE thread_branch_meta SET value = '1' WHERE key = 'schema_version'`,
		`DELETE FROM thread_branch_meta`,
	} {
		_, err := legacy.Exec(stmt)
		assertFenced(err)
	}
	// The old AppendWALDiff implementation prepares its INSERT before skipping
	// events below the migrated head. Fence even that otherwise silent no-op.
	stmt, err := legacy.Prepare(`INSERT INTO thread_wal_events(branch_id, seq, op, event_json, created_at) VALUES(?, ?, ?, ?, ?)`)
	if stmt != nil {
		stmt.Close()
	}
	assertFenced(err)
	gotCP, gotWAL := store.DurableStore("root").Load()
	if !reflect.DeepEqual(cp, gotCP) || !reflect.DeepEqual(wal, gotWAL) {
		t.Fatal("stale writer changed migrated data")
	}
	rec, err := store.GetBranch(context.Background(), "root")
	if err != nil || rec.Label != "before migration" {
		t.Fatalf("stale branch mutation: %#v %v", rec, err)
	}
	var version string
	if err := db.QueryRow(`SELECT value FROM thread_branch_meta WHERE key='schema_version'`).Scan(&version); err != nil || version != "2" {
		t.Fatalf("stale version mutation: %q %v", version, err)
	}
	// A second compatible process is allowed to use the same database.
	second, err := OpenSQLiteBranchStore(path, SQLiteBranchStoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	branch, err := second.CreateBranch(context.Background(), threads.BranchCreateOptions{ID: "compatible"})
	if err != nil {
		t.Fatal(err)
	}
	defer branch.Close()
}

func TestSQLiteWriteFenceCoversCurrentUnfencedDatabases(t *testing.T) {
	db, path := newV1DB(t)
	// Model schema v2 databases created before the writer fence was added.
	if _, err := db.Exec(`DROP TABLE thread_wal_events; DROP TABLE thread_checkpoints; DROP TABLE thread_branches; UPDATE thread_branch_meta SET value='2' WHERE key='schema_version';` + sqliteBranchSchema); err != nil {
		t.Fatal(err)
	}
	legacy, err := sql.Open("sqlite-legacy-writer-test", path)
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	if err := legacy.Ping(); err != nil {
		t.Fatal(err)
	}
	store, err := OpenSQLiteBranchStore(path, SQLiteBranchStoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := legacy.Exec(`UPDATE thread_branch_meta SET value='1'`); err == nil {
		t.Fatal("existing v2 database was not fenced")
	}
}

func TestSQLiteWriteFenceInstallationRollsBackMigration(t *testing.T) {
	db, path := newV1DB(t)
	seedV1Branch(t, db, "root", v1Snapshot, 10, false, v1Tail)
	store, err := OpenSQLiteBranchStore(path, SQLiteBranchStoreOptions{WriteGuardTables: []string{"missing_app_table"}})
	if err == nil {
		store.Close()
		t.Fatal("installed fence on missing table")
	}
	var version string
	if err := db.QueryRow(`SELECT value FROM thread_branch_meta WHERE key='schema_version'`).Scan(&version); err != nil || version != "1" {
		t.Fatalf("migration not rolled back: %q %v", version, err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='trigger' AND name LIKE 'agentloom_writer_%'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial fence committed: %d %v", count, err)
	}
}

func TestSQLiteWriteFenceNewerRequirementDoesNotGetDowngraded(t *testing.T) {
	_, path := newV1DB(t)
	store, err := OpenSQLiteBranchStore(path, SQLiteBranchStoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	// A future incompatible release installs an additional, stricter fence.
	if _, err := store.db.Exec(`CREATE TRIGGER agentloom_writer_v3_test BEFORE INSERT ON thread_branches WHEN agentloom_writer_version() < 3 BEGIN SELECT RAISE(ABORT, 'store upgraded; restart using the updated application'); END`); err != nil {
		t.Fatal(err)
	}
	second, err := OpenSQLiteBranchStore(path, SQLiteBranchStoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	_, err = second.CreateBranch(context.Background(), threads.BranchCreateOptions{ID: "stale-v2"})
	if err == nil || !strings.Contains(err.Error(), "restart") {
		t.Fatalf("stricter fence weakened: %v", err)
	}
}

func TestSQLiteWriteFenceMigrationWaitsForPriorWriter(t *testing.T) {
	db, path := newV1DB(t)
	seedV1Branch(t, db, "root", v1Snapshot, 10, false, v1Tail)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		t.Fatal(err)
	}
	old, err := sql.Open("sqlite-legacy-writer-test", path)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	tx, err := old.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO thread_wal_events VALUES('root',18,'queue_item','{"s":18,"o":"queue_item","i":{"kind":"user_text","text":"committed just before migration"}}','old'); UPDATE thread_branches SET last_seq=18 WHERE id='root'`); err != nil {
		t.Fatal(err)
	}
	type result struct {
		store *SQLiteBranchStore
		err   error
	}
	done := make(chan result, 1)
	go func() {
		// Match Weaver's write-lock configuration; a deferred transaction can
		// return SQLITE_BUSY when upgrading a read snapshot to a writer.
		store, err := OpenSQLiteBranchStore("file:"+path+"?_txlock=immediate", SQLiteBranchStoreOptions{})
		done <- result{store, err}
	}()
	select {
	case result := <-done:
		if result.store != nil {
			result.store.Close()
		}
		t.Fatalf("migration did not wait for the active writer: %v", result.err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var migrated result
	select {
	case migrated = <-done:
	case <-time.After(6 * time.Second):
		t.Fatal("migration did not finish after writer committed")
	}
	if migrated.err != nil {
		t.Fatal(migrated.err)
	}
	defer migrated.store.Close()
	cp, wal := migrated.store.DurableStore("root").Load()
	thread, err := threads.RestoreFromCheckpointAndWAL(cp, wal, threads.RestoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	turns := thread.CompletedTurns()
	if turns[len(turns)-1].Text() != "committed just before migration" {
		t.Fatal("migration lost the preceding writer's commit")
	}
	if _, err := old.Exec(`DELETE FROM thread_checkpoints`); err == nil {
		t.Fatal("prior writer could still mutate after migration")
	}
}
