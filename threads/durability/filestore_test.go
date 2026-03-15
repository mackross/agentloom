package durability

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mackross/agentloom/threads"
)

func TestFileStoreSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "thread.dur")
	store := NewFileStore(path)

	thread := threads.New()
	base, err := thread.Checkpoint(threads.CheckpointOptions{Policy: threads.InflightSkip})
	if err != nil {
		t.Fatalf("base checkpoint: %v", err)
	}
	thread.QueueItem(threads.UserText("hello"))
	thread.QueueItem(threads.UserText("world"))
	wal := thread.WALAfter(base.Seq)

	store.ReplaceSnapshot(base)
	store.AppendWALDiff(wal)
	loadedCP, loadedWAL := store.Load()

	restored, err := threads.RestoreFromCheckpointAndWAL(loadedCP, loadedWAL, threads.RestoreOptions{})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	got, err := restored.Snapshot()
	if err != nil {
		t.Fatalf("snapshot restored: %v", err)
	}
	want, err := thread.Snapshot()
	if err != nil {
		t.Fatalf("snapshot original: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("restored thread mismatch\nwant=%#v\ngot=%#v", want, got)
	}
}

func TestFileStoreAppendsWalWhenCheckpointUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "thread.dur")
	store := NewFileStore(path)

	thread := threads.New()
	cp, err := thread.Checkpoint(threads.CheckpointOptions{Policy: threads.InflightSkip})
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	walA := []threads.WALEvent{{Seq: cp.Seq + 1, Op: "queue_item", Item: threads.SnapshotItem{Type: "user_text", Text: "a"}}}
	walB := []threads.WALEvent{{Seq: cp.Seq + 2, Op: "end_stream"}}

	store.ReplaceSnapshot(cp)
	store.AppendWALDiff(walA)
	infoA, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after walA: %v", err)
	}
	store.AppendWALDiff(walB)
	infoB, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after walB: %v", err)
	}
	encodedB, err := encodeWAL(walB)
	if err != nil {
		t.Fatalf("encode walB: %v", err)
	}
	if got, want := infoB.Size()-infoA.Size(), int64(len(encodedB)); got != want {
		t.Fatalf("expected append growth %d, got %d", want, got)
	}

	_, loadedWAL := store.Load()
	if !reflect.DeepEqual(loadedWAL, append(walA, walB...)) {
		t.Fatalf("unexpected wal events: %#v", loadedWAL)
	}
}

func TestFileStoreRewritesWhenCheckpointChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "thread.dur")
	store := NewFileStore(path)

	threadA := threads.New()
	cpA, err := threadA.Checkpoint(threads.CheckpointOptions{Policy: threads.InflightSkip})
	if err != nil {
		t.Fatalf("checkpoint A: %v", err)
	}
	walA := []threads.WALEvent{{Seq: cpA.Seq + 1, Op: "queue_item", Item: threads.SnapshotItem{Type: "user_text", Text: "a"}}}
	store.ReplaceSnapshot(cpA)
	store.AppendWALDiff(walA)

	threadB := threads.New()
	threadB.QueueItem(threads.UserText("different"))
	cpB, err := threadB.Checkpoint(threads.CheckpointOptions{Policy: threads.InflightSkip})
	if err != nil {
		t.Fatalf("checkpoint B: %v", err)
	}
	walB := []threads.WALEvent{{Seq: cpB.Seq + 1, Op: "end_stream"}}
	store.ReplaceSnapshot(cpB)
	store.AppendWALDiff(walB)

	loadedCP, loadedWAL := store.Load()
	if loadedCP.Seq != cpB.Seq || loadedCP.Unsafe != cpB.Unsafe || !reflect.DeepEqual(loadedCP.Snapshot, cpB.Snapshot) {
		t.Fatal("expected rewritten checkpoint B")
	}
	if !reflect.DeepEqual(loadedWAL, walB) {
		t.Fatalf("expected rewritten wal %#v, got %#v", walB, loadedWAL)
	}
}

func TestFileStoreHeaderContainsSnapshotLength(t *testing.T) {
	path := filepath.Join(t.TempDir(), "thread.dur")
	store := NewFileStore(path)

	thread := threads.New()
	thread.QueueItem(threads.UserText("hello"))
	cp, err := thread.Checkpoint(threads.CheckpointOptions{Policy: threads.InflightSkip})
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	wal := []threads.WALEvent{{Seq: cp.Seq + 1, Op: "end_stream"}}
	store.ReplaceSnapshot(cp)
	store.AppendWALDiff(wal)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if len(raw) < headerSize {
		t.Fatalf("expected at least %d header bytes, got %d", headerSize, len(raw))
	}
	if got := string(raw[0:4]); got != fileMagic {
		t.Fatalf("unexpected file magic: %q", got)
	}
	if got := raw[4]; got != fileVersion {
		t.Fatalf("unexpected file version: %d", got)
	}
	encodedSnap, err := json.Marshal(cp.Snapshot)
	if err != nil {
		t.Fatalf("encode snapshot: %v", err)
	}
	if got := binary.BigEndian.Uint32(raw[8:12]); got != uint32(len(encodedSnap)) {
		t.Fatalf("unexpected snapshot length in header: got %d want %d", got, len(encodedSnap))
	}
}

func TestEncodeWALUsesTinyKeys(t *testing.T) {
	wal := []threads.WALEvent{{Seq: 1, Op: "append_stream_item", Item: threads.SnapshotItem{Type: "assistant_text", Text: "x"}}}
	b, err := encodeWAL(wal)
	if err != nil {
		t.Fatalf("encode wal: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, `"s":`) || !strings.Contains(s, `"o":"append_stream_item"`) || !strings.Contains(s, `"i":`) {
		t.Fatalf("expected tiny wal keys/codes, got %s", s)
	}
	if strings.Contains(s, `"seq":`) || strings.Contains(s, `"op":`) || strings.Contains(s, `"item":`) {
		t.Fatalf("expected compact wal json keys, got %s", s)
	}
}
