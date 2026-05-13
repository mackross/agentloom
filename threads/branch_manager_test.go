package threads

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

type branchTargetTestCodec struct{}

func (branchTargetTestCodec) Parse(ref string) (BranchTarget, error) {
	switch ref {
	case "root":
		return BranchHeadTarget("root"), nil
	case "root/turn/0":
		return BranchTurnTarget("root", 0), nil
	default:
		return BranchTarget{}, ErrBranchNotFound
	}
}

func (branchTargetTestCodec) Format(target BranchTarget) (string, error) {
	if target.IsHead() {
		return string(target.BranchID), nil
	}
	return string(target.BranchID) + "/turn/0", nil
}

func TestStoredBranchLoadAndBranchManagerOpen(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryBranchStore()

	stored, err := store.CreateBranch(ctx, BranchCreateOptions{ID: "root"})
	if err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	branch, err := stored.Load(RestoreOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	branch.SetDurableStore(stored.Durable)
	branch.QueueItem(UserText("hello"))
	branch.QueueItem(AssistantText("hi"))
	if err := branch.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	manager := BranchManager[string]{Store: store, Owner: "test", Codec: branchTargetTestCodec{}}
	loaded, err := manager.Open(ctx, "root")
	if err != nil {
		t.Fatalf("Open head: %v", err)
	}
	turns := loaded.CompletedTurns()
	if len(turns) != 2 || turns[0].Text() != "hello" || turns[1].Text() != "hi" {
		t.Fatalf("head turns = %#v", turns)
	}
	if loaded.ID() != "root" {
		t.Fatalf("ID = %q, want root", loaded.ID())
	}
	if loaded.Record().ID != "root" {
		t.Fatalf("Record ID = %q, want root", loaded.Record().ID)
	}
	if err := loaded.RunOnEventLoop(ctx, func(thread *Thread) error {
		if got := len(thread.CompletedTurns()); got != 2 {
			t.Fatalf("event loop turns = %d, want 2", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("RunOnEventLoop: %v", err)
	}
	if err := loaded.Close(); err != nil {
		t.Fatalf("Close loaded: %v", err)
	}

	loadedWithoutLoop, err := manager.Open(ctx, "root", OpenWithoutEventLoop())
	if err != nil {
		t.Fatalf("OpenWithoutEventLoop: %v", err)
	}
	if err := loadedWithoutLoop.RunOnEventLoop(ctx, func(*Thread) error { return nil }); !errors.Is(err, ErrEventLoopClosed) {
		t.Fatalf("RunOnEventLoop without loop err = %v, want ErrEventLoopClosed", err)
	}
	if err := loadedWithoutLoop.WaitUntilIdle(ctx); !errors.Is(err, ErrEventLoopClosed) {
		t.Fatalf("WaitUntilIdle without loop err = %v, want ErrEventLoopClosed", err)
	}
	if err := loadedWithoutLoop.Close(); err != nil {
		t.Fatalf("Close loaded without loop: %v", err)
	}

	if _, err := manager.Open(ctx, "root/turn/0"); !errors.Is(err, ErrBranchCopyOptionRequired) {
		t.Fatalf("Open turn without copy err = %v, want ErrBranchCopyOptionRequired", err)
	}

	turnBranch, err := manager.Open(ctx, "root/turn/0", OpenAsEphemeralCopy("try-1"))
	if err != nil {
		t.Fatalf("Open turn target: %v", err)
	}
	defer turnBranch.Close()
	if turnBranch.Record().Kind != BranchKindEphemeral {
		t.Fatalf("turn branch kind = %q, want ephemeral", turnBranch.Record().Kind)
	}
	if turnBranch.ID() != "try-1" {
		t.Fatalf("turn branch ID = %q, want try-1", turnBranch.ID())
	}
	turns = turnBranch.CompletedTurns()
	if len(turns) != 1 || turns[0].Text() != "hello" {
		t.Fatalf("turn target turns = %#v, want just user hello", turns)
	}
}

func TestDefaultBranchTargetCodec(t *testing.T) {
	manager := BranchManager[string]{}
	if got := NewDefaultBranchManager(nil, "owner").Owner; got != "owner" {
		t.Fatalf("NewDefaultBranchManager owner = %q, want owner", got)
	}

	head, err := manager.Parse("/branch/root")
	if err != nil {
		t.Fatalf("Parse head: %v", err)
	}
	if !head.IsHead() || head.BranchID != "root" {
		t.Fatalf("head = %#v", head)
	}

	turn, err := manager.Parse("/branch/root/turn/12")
	if err != nil {
		t.Fatalf("Parse turn: %v", err)
	}
	if turn.IsHead() || turn.BranchID != "root" || turn.TurnIndex == nil || *turn.TurnIndex != 12 {
		t.Fatalf("turn = %#v", turn)
	}

	ref, err := manager.Format(BranchTurnTarget("root", 3))
	if err != nil {
		t.Fatalf("Format turn: %v", err)
	}
	if ref != "/branch/root/turn/3" {
		t.Fatalf("ref = %q", ref)
	}
}

func TestBranchWaitUntilIdleAlreadyIdle(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryBranchStore()
	stored, err := store.CreateBranch(ctx, BranchCreateOptions{ID: "root"})
	if err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if err := stored.Close(); err != nil {
		t.Fatalf("Close stored: %v", err)
	}

	branch, err := NewDefaultBranchManager(store, "test").Open(ctx, "/branch/root")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer branch.Close()
	if err := branch.WaitUntilIdle(ctx); err != nil {
		t.Fatalf("WaitUntilIdle: %v", err)
	}
}

func TestBranchWaitUntilIdleReturnsAfterDelegate(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryBranchStore()
	stored, err := store.CreateBranch(ctx, BranchCreateOptions{ID: "root"})
	if err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if err := stored.Close(); err != nil {
		t.Fatalf("Close stored: %v", err)
	}

	branch, err := NewDefaultBranchManager(store, "test").Open(ctx, "/branch/root")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer branch.Close()
	if err := branch.RunOnEventLoop(ctx, func(thread *Thread) error {
		thread.QueueItem(UserText("pending"))
		thread.QueueItem(SendItem{})
		if thread.State() == StateIdle {
			return fmt.Errorf("thread unexpectedly idle after pending send")
		}
		return nil
	}); err != nil {
		t.Fatalf("queue pending user turn: %v", err)
	}

	delegateStarted := make(chan struct{})
	releaseDelegate := make(chan struct{})
	waitReturned := make(chan error, 1)
	branch.SetDelegate(ThreadDelegateFuncs{
		OnIdle: func(*Thread) {
			close(delegateStarted)
			<-releaseDelegate
		},
	})
	go func() { waitReturned <- branch.WaitUntilIdle(ctx) }()

	select {
	case err := <-waitReturned:
		t.Fatalf("WaitUntilIdle returned before idle: %v", err)
	case <-time.After(10 * time.Millisecond):
	}

	go branch.OnThreadIdle(branch.Thread)
	select {
	case <-delegateStarted:
	case <-time.After(time.Second):
		t.Fatal("delegate was not called")
	}
	select {
	case err := <-waitReturned:
		t.Fatalf("WaitUntilIdle returned before delegate completed: %v", err)
	case <-time.After(10 * time.Millisecond):
	}

	close(releaseDelegate)
	select {
	case err := <-waitReturned:
		if err != nil {
			t.Fatalf("WaitUntilIdle: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("WaitUntilIdle did not return")
	}
}
