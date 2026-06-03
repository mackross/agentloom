package filetool

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/mackross/agentloom/threads/tool"
)

func TestMutationQueueSerializesOverlappingLockSets(t *testing.T) {
	q := NewMutationQueue()

	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- q.withLocks([]string{"b.txt", "a.txt", "a.txt"}, func() error {
			close(firstEntered)
			<-releaseFirst
			return nil
		})
	}()
	<-firstEntered

	secondAttempted := make(chan struct{})
	secondEntered := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		close(secondAttempted)
		secondDone <- q.withLocks([]string{"a.txt", "b.txt"}, func() error {
			close(secondEntered)
			return nil
		})
	}()
	<-secondAttempted

	select {
	case <-secondEntered:
		t.Fatal("overlapping lock set entered before the first lock set was released")
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatalf("first withLocks: %v", err)
	}
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatalf("second withLocks: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("overlapping lock set did not enter after release")
	}
}

func TestMutationQueueAllowsDisjointLockSets(t *testing.T) {
	q := NewMutationQueue()

	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- q.withLocks([]string{"a.txt"}, func() error {
			close(firstEntered)
			<-releaseFirst
			return nil
		})
	}()
	<-firstEntered

	secondEntered := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- q.withLocks([]string{"b.txt"}, func() error {
			close(secondEntered)
			return nil
		})
	}()

	select {
	case <-secondEntered:
	case <-time.After(time.Second):
		t.Fatal("disjoint lock set was blocked by unrelated mutation")
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second withLocks: %v", err)
	}

	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatalf("first withLocks: %v", err)
	}
}

func TestDefaultMutationQueueSerializesWriteAndApplyPatchAcrossCatalogs(t *testing.T) {
	dir := t.TempDir()
	path, err := safePatchPath(dir, "notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, path, "old\n")

	restoreDefaultMutationQueue(t, NewMutationQueue())
	releaseDefault := lockPathForTest(t, DefaultMutationQueue, path)

	writeDone := make(chan error, 1)
	go func() {
		_, err := AddWrite(tool.NewCatalog(), WriteConfig{CWD: dir}).Dispatch(context.Background(), nil, tool.Call{
			CallID:  "write-1",
			Name:    "write",
			Payload: `{"path":"notes.txt","content":"written\n"}`,
		})
		writeDone <- err
	}()
	assertStillBlocked(t, writeDone, "write using DefaultMutationQueue")
	close(releaseDefault)
	if err := awaitDone(t, writeDone, "write using DefaultMutationQueue"); err != nil {
		t.Fatalf("write dispatch: %v", err)
	}
	assertFile(t, path, "written\n")

	releaseDefault = lockPathForTest(t, DefaultMutationQueue, path)
	patchDone := make(chan error, 1)
	go func() {
		_, err := AddApplyPatch(tool.NewCatalog(), ApplyPatchConfig{CWD: dir}).Dispatch(context.Background(), nil, tool.Call{
			CallID: "patch-1",
			Name:   "apply_patch",
			Payload: `*** Begin Patch
*** Update File: notes.txt
@@
-written
+patched
*** End Patch`,
		})
		patchDone <- err
	}()
	assertStillBlocked(t, patchDone, "apply_patch using DefaultMutationQueue")
	close(releaseDefault)
	if err := awaitDone(t, patchDone, "apply_patch using DefaultMutationQueue"); err != nil {
		t.Fatalf("apply_patch dispatch: %v", err)
	}
	assertFile(t, path, "patched\n")
}

func TestToolboxMutationQueueCoordinatesWriteAndApplyPatch(t *testing.T) {
	dir := t.TempDir()
	path, err := safePatchPath(dir, "notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, path, "old\n")

	q := NewMutationQueue()
	release := lockPathForTest(t, q, path)
	cat := NewCatalog(ToolboxConfig{
		MutationQueue: q,
		Write:         WriteConfig{CWD: dir},
		ApplyPatch:    ApplyPatchConfig{CWD: dir},
	})

	done := make(chan error, 1)
	go func() {
		_, err := cat.Dispatch(context.Background(), nil, tool.Call{
			CallID:  "write-1",
			Name:    "write",
			Payload: `{"path":"notes.txt","content":"queued\n"}`,
		})
		done <- err
	}()
	assertStillBlocked(t, done, "toolbox write using configured MutationQueue")
	close(release)
	if err := awaitDone(t, done, "toolbox write using configured MutationQueue"); err != nil {
		t.Fatalf("toolbox write dispatch: %v", err)
	}
	assertFile(t, filepath.Join(dir, "notes.txt"), "queued\n")
}

func TestToolMutationQueueConfigOverridesDefault(t *testing.T) {
	dir := t.TempDir()
	path, err := safePatchPath(dir, "notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	restoreDefaultMutationQueue(t, NewMutationQueue())
	releaseDefault := lockPathForTest(t, DefaultMutationQueue, path)
	defer close(releaseDefault)

	dispatch, err := NewWriteTool(WriteConfig{
		CWD:           dir,
		MutationQueue: NewMutationQueue(),
	}).ResolveTool(context.Background(), nil, tool.Call{
		CallID:  "c1",
		Name:    "write",
		Payload: `{"path":"notes.txt","content":"custom queue\n"}`,
	}, nil)
	if err != nil {
		t.Fatalf("ResolveTool: %v", err)
	}
	assertWriteResult(t, dispatch, "Wrote notes.txt")
	assertFile(t, path, "custom queue\n")
}

func restoreDefaultMutationQueue(t *testing.T, q *MutationQueue) {
	t.Helper()
	old := DefaultMutationQueue
	DefaultMutationQueue = q
	t.Cleanup(func() { DefaultMutationQueue = old })
}

func lockPathForTest(t *testing.T, q *MutationQueue, path string) chan<- struct{} {
	t.Helper()
	entered := make(chan struct{})
	release := make(chan struct{})
	go func() {
		if err := q.withLocks([]string{path}, func() error {
			close(entered)
			<-release
			return nil
		}); err != nil {
			panic(err)
		}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for test lock")
	}
	return release
}

func assertStillBlocked(t *testing.T, done <-chan error, name string) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("%s completed before overlapping mutation lock was released: %v", name, err)
	case <-time.After(50 * time.Millisecond):
	}
}

func awaitDone(t *testing.T, done <-chan error, name string) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(time.Second):
		t.Fatalf("%s did not complete after overlapping mutation lock was released", name)
		return nil
	}
}
