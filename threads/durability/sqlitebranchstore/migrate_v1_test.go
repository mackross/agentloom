package sqlitebranchstore

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mackross/agentloom/threads"
)

//go:embed testdata/v1.sql
var v1Schema string

const v1Snapshot = `{"ver":1,"state":"idle","items":[
	{"kind":"assistant_instruction","text":"system"},
	{"kind":"user_text","text":"first question"},
	{"kind":"item_meta","data":"{\"cache\":true,\"tag\":\"old\"}"},
	{"kind":"item_meta","data":"{\"tag\":\"new\"}"},
	{"kind":"send"},
	{"kind":"assistant_text","text":"first answer"}
],"ip":5,"queue":-1,"stream":-1}`

var v1Tail = []string{
	`{"s":11,"o":"queue_item","i":{"kind":"user_text","text":"second question"}}`,
	`{"s":12,"o":"queue_item","i":{"kind":"item_meta","data":"{\"cache\":true}"}}`,
	`{"s":13,"o":"queue_item","i":{"kind":"send"}}`,
	`{"s":14,"o":"begin_stream"}`,
	`{"s":15,"o":"append_stream_item","i":{"kind":"assistant_text","text":"second answer"}}`,
	`{"s":16,"o":"append_stream_item","i":{"kind":"item_meta","data":"{\"response\":\"kept\"}"}}`,
	`{"s":17,"o":"end_stream"}`,
}

func newV1DB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "v1.sqlite3")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(v1Schema); err != nil {
		t.Fatal(err)
	}
	return db, path
}

func seedV1Branch(t *testing.T, db *sql.DB, id, snapshot string, seq uint32, unsafe bool, tail []string) {
	t.Helper()
	stamp := "2026-09-01T00-00-00.000000000Z"
	if _, err := db.Exec(`INSERT INTO thread_branches(id, kind, created_at, updated_at, last_seq) VALUES(?, 'durable', ?, ?, ?)`, id, stamp, stamp, seq+uint32(len(tail))); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO thread_checkpoints VALUES(?, ?, ?, ?, ?)`, id, seq, unsafe, snapshot, stamp); err != nil {
		t.Fatal(err)
	}
	for _, raw := range tail {
		var ev threads.WALEvent
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO thread_wal_events VALUES(?, ?, ?, ?, ?)`, id, ev.Seq, ev.Op, raw, stamp); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSQLiteBranchStoreMigratesV1(t *testing.T) {
	db, path := newV1DB(t)
	seedV1Branch(t, db, "root", v1Snapshot, 10, false, v1Tail)
	// The old branch API synthesized a send without allocating a mutation.
	child := `{"ver":1,"state":"construct_llm_request","items":[{"kind":"user_text","text":"branch question"},{"kind":"send"}],"ip":1,"queue":-1,"stream":-1}`
	seedV1Branch(t, db, "child", child, 1, true, nil)
	if _, err := db.Exec(`UPDATE thread_branches SET ancestors_json = '[{"ID":"root","Kind":"durable"}]', source_turn_index = 2, source_turn_role = 'user', source_seq = 17, source_head_seq = 17 WHERE id = 'child'`); err != nil {
		t.Fatal(err)
	}
	// Application-owned rows, foreign keys and triggers must survive migration.
	if _, err := db.Exec(`CREATE TABLE app_sessions(branch_id TEXT PRIMARY KEY REFERENCES thread_branches(id) ON DELETE CASCADE, title TEXT); INSERT INTO app_sessions VALUES('root', 'keep me')`); err != nil {
		t.Fatal(err)
	}
	store, err := OpenSQLiteBranchStore(path, SQLiteBranchStoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cp, wal := store.DurableStore("root").Load()
	if cp.Snapshot.Version != 2 || cp.Seq < 17 || cp.Snapshot.HeadSeq != cp.Seq || len(wal) != len(v1Tail) {
		t.Fatalf("checkpoint/tail not migrated: seq=%d version=%d wal=%d", cp.Seq, cp.Snapshot.Version, len(wal))
	}
	if cp.Snapshot.IPIndex != 3 || cp.Snapshot.Items[1].Metadata != `{"cache":true,"tag":"new"}` {
		t.Fatalf("metadata/control indices: %#v", cp.Snapshot)
	}
	restored, err := threads.RestoreFromCheckpointAndWAL(cp, wal, threads.RestoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	turns := restored.CompletedTurns()
	if len(turns) != 4 || turns[3].Text() != "second answer" {
		t.Fatalf("turns: %#v", turns)
	}
	snap, err := restored.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snap.Items[len(snap.Items)-1].Metadata != `{"response":"kept"}` {
		t.Fatalf("stream metadata lost: %#v", snap.Items)
	}
	childCP, childWAL := store.DurableStore("child").Load()
	if _, err := threads.RestoreFromCheckpointAndWAL(childCP, childWAL, threads.RestoreOptions{AllowUnsafe: true}); err != nil {
		t.Fatal(err)
	}
	if childCP.Seq < 2 || !childCP.Unsafe {
		t.Fatalf("synthesized send: %#v", childCP)
	}
	branch, err := store.GetBranch(context.Background(), "child")
	if err != nil {
		t.Fatal(err)
	}
	if branch.SourceTurnSeq != 0 || branch.ParentID() != "root" || branch.SourceHeadSeq != 17 {
		t.Fatalf("legacy provenance: %#v", branch)
	}
	var oldIndex int
	if err := db.QueryRow(`SELECT source_turn_index FROM thread_branches WHERE id='child'`).Scan(&oldIndex); err != nil || oldIndex != 2 {
		t.Fatalf("old provenance lost: %d %v", oldIndex, err)
	}
	var title string
	if err := db.QueryRow(`SELECT title FROM app_sessions WHERE branch_id='root'`).Scan(&title); err != nil || title != "keep me" {
		t.Fatalf("app metadata: %q %v", title, err)
	}
	// Reopening must not migrate a second time or assign new identities.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenSQLiteBranchStore(path, SQLiteBranchStoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	gotCP, gotWAL := store.DurableStore("root").Load()
	if !reflect.DeepEqual(cp, gotCP) || !reflect.DeepEqual(wal, gotWAL) {
		t.Fatal("reopen changed checkpoint/WAL")
	}
	// Resume, checkpoint, and create another branch using the migrated IDs.
	durable := store.DurableStore("root")
	restored.SetDurableStore(durable)
	head := restored.Seq()
	restored.QueueItem(threads.UserText("after upgrade"))
	if restored.Seq() <= head {
		t.Fatal("sequence reused")
	}
	nextCP, nextWAL := durable.Load()
	if _, err := threads.RestoreFromCheckpointAndWAL(nextCP, nextWAL, threads.RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	parent, err := store.OpenBranch(context.Background(), "root", threads.BranchOpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Lease.Close()
	newChild, err := store.BranchFromCheckpoint(context.Background(), parent, threads.BranchFromCheckpointOptions{ID: "new-child", Checkpoint: nextCP, SourceTurnSeq: turns[0].ID()})
	if err != nil {
		t.Fatal(err)
	}
	defer newChild.Lease.Close()
}

func TestSQLiteBranchStoreV1MigrationPreservesCrashRecovery(t *testing.T) {
	db, path := newV1DB(t)
	seedV1Branch(t, db, "root", v1Snapshot, 10, false, v1Tail[:6])
	store, err := OpenSQLiteBranchStore(path, SQLiteBranchStoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cp, wal := store.DurableStore("root").Load()
	safe, err := threads.RestoreFromCheckpointAndWAL(cp, wal, threads.RestoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s, err := safe.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range s.Items {
		if item.Text == "second answer" {
			t.Fatal("crashed stream survived safe recovery")
		}
	}
	unsafe, err := threads.RestoreFromCheckpointAndWAL(cp, wal, threads.RestoreOptions{AllowUnsafe: true})
	if err != nil {
		t.Fatal(err)
	}
	if safe.Seq() != unsafe.Seq() {
		t.Fatal("recovery reused discarded item identities")
	}
	if unsafe.State() != threads.StateReceivingStream {
		t.Fatalf("state = %s", unsafe.State())
	}
}

func TestSQLiteBranchStoreV1MigrationMetadataBeforeBlockedSend(t *testing.T) {
	db, path := newV1DB(t)
	snapshot := `{"ver":1,"state":"awaiting_tool_results","items":[{"kind":"tool_call","id":"call","name":"read","args":"{}"},{"kind":"send"}],"ip":0,"queue":1,"stream":-1}`
	tail := []string{
		`{"s":11,"o":"queue_item_before_send","i":{"kind":"item_meta","data":"{\"cache\":true}"}}`,
		`{"s":12,"o":"queue_item","i":{"kind":"tool_result","id":"call","output":"saved result","data":"{\"kept\":true}"}}`,
		`{"s":13,"o":"begin_stream"}`,
		`{"s":14,"o":"append_stream_item","i":{"kind":"assistant_text","text":"done"}}`,
		`{"s":15,"o":"end_stream"}`,
	}
	seedV1Branch(t, db, "root", snapshot, 10, false, tail)
	store, err := OpenSQLiteBranchStore(path, SQLiteBranchStoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cp, wal := store.DurableStore("root").Load()
	thread, err := threads.RestoreFromCheckpointAndWAL(cp, wal, threads.RestoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s, err := thread.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if s.State != threads.StateIdle || s.Items[0].Metadata != `{"cache":true}` || s.Items[1].Type != "tool_result" || s.Items[1].Output != "saved result" || s.Items[1].Data != `{"kept":true}` {
		t.Fatalf("lost tool state or misplaced metadata: %#v", s)
	}
}

func TestSQLiteBranchStoreV1MigrationEmptyAndOrphanMetadata(t *testing.T) {
	db, path := newV1DB(t)
	snapshot := `{"ver":1,"state":"idle","items":[],"ip":-1,"queue":-1,"stream":-1}`
	tail := []string{
		`{"s":1,"o":"queue_item","i":{"kind":"item_meta","data":"{\"orphan\":true}"}}`,
		`{"s":2,"o":"queue_item","i":{"kind":"user_text","text":"a"}}`,
		`{"s":3,"o":"queue_item","i":{"kind":"item_meta"}}`,
		`{"s":4,"o":"queue_item","i":{"kind":"user_text","text":"b"}}`,
	}
	seedV1Branch(t, db, "root", snapshot, 0, false, tail)
	store, err := OpenSQLiteBranchStore(path, SQLiteBranchStoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cp, wal := store.DurableStore("root").Load()
	thread, err := threads.RestoreFromCheckpointAndWAL(cp, wal, threads.RestoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	turns := thread.CompletedTurns()
	if len(turns) != 1 || turns[0].Text() != "ab" {
		t.Fatalf("turns: %#v", turns)
	}
	if thread.Seq() < 4 {
		t.Fatal("legacy sequence high-water mark lost")
	}
}

func TestSQLiteBranchStoreV1MigrationRollsBack(t *testing.T) {
	for _, fault := range []string{"json", "version", "gap", "op", "head", "overflow", "hook"} {
		t.Run(fault, func(t *testing.T) {
			db, path := newV1DB(t)
			seedV1Branch(t, db, "a-good", v1Snapshot, 10, false, v1Tail)
			seedV1Branch(t, db, "z-bad", v1Snapshot, 10, false, v1Tail)
			var stmt string
			switch fault {
			case "json":
				stmt = `UPDATE thread_checkpoints SET snapshot_json='{' WHERE branch_id='z-bad'`
			case "version":
				stmt = `UPDATE thread_checkpoints SET snapshot_json=json_set(snapshot_json,'$.ver',99) WHERE branch_id='z-bad'`
			case "gap":
				stmt = `DELETE FROM thread_wal_events WHERE branch_id='z-bad' AND seq=12`
			case "op":
				stmt = `UPDATE thread_wal_events SET op='unknown' WHERE branch_id='z-bad' AND seq=12`
			case "head":
				stmt = `UPDATE thread_branches SET last_seq=999 WHERE id='z-bad'`
			case "overflow":
				stmt = `UPDATE thread_checkpoints SET seq=4294967294 WHERE branch_id='z-bad'; DELETE FROM thread_wal_events WHERE branch_id='z-bad'; INSERT INTO thread_wal_events VALUES('z-bad',4294967295,'queue_item','{"s":4294967295,"o":"queue_item","i":{"kind":"user_text","text":"last"}}','old'); UPDATE thread_branches SET last_seq=4294967295 WHERE id='z-bad'`
			}
			if stmt != "" {
				if _, err := db.Exec(stmt); err != nil {
					t.Fatal(err)
				}
			}
			opts := SQLiteBranchStoreOptions{}
			if fault == "hook" {
				opts.Hooks.InitTx = func(context.Context, *sql.Tx) error { return fmt.Errorf("app init failed") }
			}
			if store, err := OpenSQLiteBranchStore(path, opts); err == nil {
				store.Close()
				t.Fatal("migration succeeded")
			}
			var version, raw string
			if err := db.QueryRow(`SELECT value FROM thread_branch_meta WHERE key='schema_version'`).Scan(&version); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow(`SELECT snapshot_json FROM thread_checkpoints WHERE branch_id='a-good'`).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			if version != "1" || raw != v1Snapshot {
				t.Fatal("failed migration changed version/checkpoint")
			}
			var schema string
			if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE name='thread_branches'`).Scan(&schema); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(schema, "source_turn_seq") {
				t.Fatal("failed migration changed table schema")
			}
			var count int
			if err := db.QueryRow(`SELECT count(*) FROM thread_wal_events WHERE branch_id='a-good' AND seq BETWEEN 11 AND 17`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != len(v1Tail) {
				t.Fatal("failed migration changed WAL")
			}
		})
	}
}
