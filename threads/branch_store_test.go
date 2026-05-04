package threads

import (
	"context"
	"testing"
)

func TestMemoryBranchStoreEphemeralBranchesListAndSave(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryBranchStore()

	root, err := store.CreateBranch(ctx, BranchCreateOptions{ID: "root", Label: "root"})
	if err != nil {
		t.Fatalf("CreateBranch root: %v", err)
	}
	if root.Record.Kind != BranchKindDurable {
		t.Fatalf("root kind = %q, want durable", root.Record.Kind)
	}
	if root.Lease == nil {
		t.Fatalf("durable root lease is nil")
	}
	if got := root.Record.RootID(); got != "root" {
		t.Fatalf("root RootID = %q, want root", got)
	}

	rootCP, _ := root.Durable.Load()
	ephemeral, err := store.BranchFromCheckpoint(ctx, root, BranchFromCheckpointOptions{
		ID:         "try-1",
		Kind:       BranchKindEphemeral,
		Label:      "try one",
		Checkpoint: rootCP,
	})
	if err != nil {
		t.Fatalf("BranchFromCheckpoint ephemeral: %v", err)
	}
	if ephemeral.Lease != nil {
		t.Fatalf("ephemeral lease = %#v, want nil", ephemeral.Lease)
	}
	if got := ephemeral.Record.RootID(); got != "root" {
		t.Fatalf("ephemeral RootID = %q, want root", got)
	}
	if got := ephemeral.Record.ParentID(); got != "root" {
		t.Fatalf("ephemeral ParentID = %q, want root", got)
	}
	if len(ephemeral.Record.Ancestors) != 1 || ephemeral.Record.Ancestors[0].ID != "root" || ephemeral.Record.Ancestors[0].Kind != BranchKindDurable {
		t.Fatalf("ephemeral ancestors = %#v, want [root durable]", ephemeral.Record.Ancestors)
	}

	ephemeralCP, ephemeralWAL := ephemeral.Durable.Load()
	thread, err := RestoreFromCheckpointAndWAL(ephemeralCP, ephemeralWAL, RestoreOptions{})
	if err != nil {
		t.Fatalf("restore ephemeral: %v", err)
	}
	thread.SetDurableStore(ephemeral.Durable)
	thread.QueueItem(UserText("experiment"))
	saveCP, err := thread.Checkpoint(CheckpointOptions{Policy: InflightSkip})
	if err != nil {
		t.Fatalf("checkpoint ephemeral: %v", err)
	}

	saved, err := store.BranchFromCheckpoint(ctx, ephemeral, BranchFromCheckpointOptions{
		ID:         "saved",
		Kind:       BranchKindDurable,
		Label:      "saved experiment",
		Checkpoint: saveCP,
	})
	if err != nil {
		t.Fatalf("BranchFromCheckpoint save durable: %v", err)
	}
	if saved.Lease == nil {
		t.Fatalf("saved durable lease is nil")
	}
	if got := saved.Record.RootID(); got != "root" {
		t.Fatalf("saved RootID = %q, want root", got)
	}
	if got := saved.Record.ParentID(); got != "try-1" {
		t.Fatalf("saved ParentID = %q, want try-1", got)
	}
	if got := saved.Record.NearestDurableAncestorID(); got != "root" {
		t.Fatalf("saved NearestDurableAncestorID = %q, want root", got)
	}
	if len(saved.Record.Ancestors) != 2 || saved.Record.Ancestors[1].ID != "try-1" || saved.Record.Ancestors[1].Kind != BranchKindEphemeral {
		t.Fatalf("saved ancestors = %#v, want root -> try-1", saved.Record.Ancestors)
	}

	records, err := store.ListBranches(ctx, BranchFilter{RootID: "root"})
	if err != nil {
		t.Fatalf("ListBranches: %v", err)
	}
	if got, want := len(records), 3; got != want {
		t.Fatalf("ListBranches root count = %d, want %d: %#v", got, want, records)
	}

	ephOnly, err := store.ListBranches(ctx, BranchFilter{RootID: "root", Kind: BranchKindEphemeral})
	if err != nil {
		t.Fatalf("ListBranches ephemeral: %v", err)
	}
	if got, want := len(ephOnly), 1; got != want {
		t.Fatalf("ephemeral count = %d, want %d", got, want)
	}
	if ephOnly[0].ID != "try-1" {
		t.Fatalf("ephemeral ID = %q, want try-1", ephOnly[0].ID)
	}

	savedCP, savedWAL := saved.Durable.Load()
	restoredSaved, err := RestoreFromCheckpointAndWAL(savedCP, savedWAL, RestoreOptions{})
	if err != nil {
		t.Fatalf("restore saved: %v", err)
	}
	turns := restoredSaved.CompletedTurns()
	if len(turns) != 1 || turns[0].Role() != TurnUser || turns[0].Text() != "experiment" {
		t.Fatalf("saved turns = %#v, want one user experiment turn", turns)
	}
}

func TestMemoryBranchStoreDurableLeaseAndEphemeralNoLease(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryBranchStore()

	durable, err := store.CreateBranch(ctx, BranchCreateOptions{ID: "durable"})
	if err != nil {
		t.Fatalf("CreateBranch durable: %v", err)
	}
	if _, err := store.OpenBranch(ctx, "durable", BranchOpenOptions{}); err != ErrBranchAlreadyOpen {
		t.Fatalf("OpenBranch durable while open err = %v, want ErrBranchAlreadyOpen", err)
	}
	if err := durable.Close(); err != nil {
		t.Fatalf("close durable: %v", err)
	}
	opened, err := store.OpenBranch(ctx, "durable", BranchOpenOptions{})
	if err != nil {
		t.Fatalf("OpenBranch durable after close: %v", err)
	}
	if opened.Lease == nil {
		t.Fatalf("opened durable lease is nil")
	}

	cp, _ := opened.Durable.Load()
	eph, err := store.BranchFromCheckpoint(ctx, opened, BranchFromCheckpointOptions{ID: "ephemeral", Kind: BranchKindEphemeral, Checkpoint: cp})
	if err != nil {
		t.Fatalf("BranchFromCheckpoint ephemeral: %v", err)
	}
	if eph.Lease != nil {
		t.Fatalf("ephemeral lease = %#v, want nil", eph.Lease)
	}
	if _, err := store.OpenBranch(ctx, "ephemeral", BranchOpenOptions{}); err != nil {
		t.Fatalf("OpenBranch ephemeral first: %v", err)
	}
	if _, err := store.OpenBranch(ctx, "ephemeral", BranchOpenOptions{}); err != nil {
		t.Fatalf("OpenBranch ephemeral second: %v", err)
	}
}

func TestMemoryBranchStoreErrorsAndLeaseCloseIdempotent(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryBranchStore()

	if _, err := store.CreateBranch(ctx, BranchCreateOptions{ID: "bad", Kind: BranchKind("bogus")}); err != ErrInvalidBranchKind {
		t.Fatalf("CreateBranch invalid kind err = %v, want ErrInvalidBranchKind", err)
	}
	if _, err := store.OpenBranch(ctx, "missing", BranchOpenOptions{}); err != ErrBranchNotFound {
		t.Fatalf("OpenBranch missing err = %v, want ErrBranchNotFound", err)
	}
	if _, err := store.BranchFromCheckpoint(ctx, nil, BranchFromCheckpointOptions{}); err != ErrBranchParentRequired {
		t.Fatalf("BranchFromCheckpoint nil parent err = %v, want ErrBranchParentRequired", err)
	}

	branch, err := store.CreateBranch(ctx, BranchCreateOptions{ID: "root"})
	if err != nil {
		t.Fatalf("CreateBranch root: %v", err)
	}
	if err := branch.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := branch.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	reopened, err := store.OpenBranch(ctx, "root", BranchOpenOptions{})
	if err != nil {
		t.Fatalf("OpenBranch after idempotent close: %v", err)
	}
	if err := store.DeleteBranch(ctx, "root"); err != ErrBranchOpen {
		t.Fatalf("DeleteBranch open err = %v, want ErrBranchOpen", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("close reopened: %v", err)
	}
	if err := store.DeleteBranch(ctx, "root"); err != nil {
		t.Fatalf("DeleteBranch closed: %v", err)
	}
	if err := store.DeleteBranch(ctx, "root"); err != ErrBranchNotFound {
		t.Fatalf("DeleteBranch missing err = %v, want ErrBranchNotFound", err)
	}
}
