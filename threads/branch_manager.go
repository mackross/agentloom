package threads

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

var (
	ErrBranchTargetCodecRequired = errors.New("threads branch target codec required")
	ErrBranchCopyOptionRequired  = errors.New("threads branch copy option required")
)

var DefaultBranchTargetCodec BranchTargetCodec[string] = StringBranchTargetCodec{}

// BranchTarget identifies either a branch head or a completed turn within a
// branch. A nil TurnIndex means the branch head.
type BranchTarget struct {
	BranchID  BranchID
	TurnIndex *int
}

func BranchHeadTarget(id BranchID) BranchTarget { return BranchTarget{BranchID: id} }

func BranchTurnTarget(id BranchID, turnIndex int) BranchTarget {
	return BranchTarget{BranchID: id, TurnIndex: &turnIndex}
}

func (t BranchTarget) IsHead() bool { return t.TurnIndex == nil }

// BranchTargetCodec adapts application-owned reference forms, such as URLs or
// route structs, to BranchTarget.
type BranchTargetCodec[T any] interface {
	Parse(T) (BranchTarget, error)
	Format(BranchTarget) (T, error)
}

type BranchOpenOption func(*branchOpenConfig)

type branchOpenConfig struct {
	copySet          bool
	copyID           BranchID
	copyKind         BranchKind
	withoutEventLoop bool
	allowUnsafe      bool
}

// OpenAsDurableCopy makes opening a turn target create a durable child branch with
// id. Empty id lets the store choose an id. It has no effect for branch heads.
func OpenAsDurableCopy(id BranchID) BranchOpenOption {
	return func(c *branchOpenConfig) {
		c.copySet = true
		c.copyID = id
		c.copyKind = BranchKindDurable
	}
}

// OpenAsEphemeralCopy makes opening a turn target create an ephemeral child branch
// with id. Empty id lets the store choose an id. It has no effect for branch
// heads.
func OpenAsEphemeralCopy(id BranchID) BranchOpenOption {
	return func(c *branchOpenConfig) {
		c.copySet = true
		c.copyID = id
		c.copyKind = BranchKindEphemeral
	}
}

func OpenWithoutEventLoop() BranchOpenOption {
	return func(c *branchOpenConfig) {
		c.withoutEventLoop = true
	}
}

func OpenAllowUnsafeRestore() BranchOpenOption {
	return func(c *branchOpenConfig) {
		c.allowUnsafe = true
	}
}

// BranchManager is a small application helper for opening branch targets. It
// composes BranchStore, BranchTargetCodec, and Thread restoration; it does not
// own goroutines or event loops.
type BranchManager[T any] struct {
	Store BranchStore
	Owner string
	Codec BranchTargetCodec[T]
}

func NewDefaultBranchManager(store BranchStore, owner string) BranchManager[string] {
	return BranchManager[string]{Store: store, Owner: owner}
}

func (m BranchManager[T]) Parse(ref T) (BranchTarget, error) {
	codec, err := m.codec()
	if err != nil {
		return BranchTarget{}, err
	}
	return codec.Parse(ref)
}

func (m BranchManager[T]) Format(target BranchTarget) (T, error) {
	var zero T
	codec, err := m.codec()
	if err != nil {
		return zero, err
	}
	return codec.Format(target)
}

// Open resolves ref and returns a loaded live Branch. Branch-head targets open
// the existing branch as stored. Turn targets require OpenAs*Copy and create a
// child branch from that turn checkpoint.
func (m BranchManager[T]) Open(ctx context.Context, ref T, opts ...BranchOpenOption) (*Branch, error) {
	target, err := m.Parse(ref)
	if err != nil {
		return nil, err
	}
	cfg := branchOpenConfig{}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	if target.IsHead() {
		stored, err := m.Store.OpenBranch(ctx, target.BranchID, BranchOpenOptions{Owner: m.Owner, ReadOnly: cfg.copySet})
		if err != nil {
			return nil, err
		}
		if cfg.copySet {
			parent, err := stored.Load(RestoreOptions{AllowUnsafe: cfg.allowUnsafe})
			if err != nil {
				_ = stored.Close()
				return nil, err
			}
			cp, err := parent.Checkpoint(CheckpointOptions{Policy: InflightSkip})
			if err != nil {
				_ = parent.Close()
				return nil, err
			}
			childStored, err := m.Store.BranchFromCheckpoint(ctx, stored, BranchFromCheckpointOptions{
				ID:         cfg.copyID,
				Kind:       cfg.copyKind,
				Owner:      m.Owner,
				Checkpoint: cp,
			})
			if err != nil {
				_ = parent.Close()
				return nil, err
			}
			_ = parent.Close()
			return loadStoredBranch(childStored, RestoreOptions{AllowUnsafe: cfg.allowUnsafe}, !cfg.withoutEventLoop)
		}
		return loadStoredBranch(stored, RestoreOptions{AllowUnsafe: cfg.allowUnsafe}, !cfg.withoutEventLoop)
	}
	return m.openTurnTarget(ctx, target, cfg)
}

func (m BranchManager[T]) openTurnTarget(ctx context.Context, target BranchTarget, cfg branchOpenConfig) (*Branch, error) {
	if target.TurnIndex == nil || *target.TurnIndex < 0 {
		return nil, ErrInvalidTurn
	}
	if !cfg.copySet {
		return nil, ErrBranchCopyOptionRequired
	}
	parentStored, err := m.Store.OpenBranch(ctx, target.BranchID, BranchOpenOptions{Owner: m.Owner, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	parent, err := parentStored.Load(RestoreOptions{AllowUnsafe: true})
	if err != nil {
		_ = parentStored.Close()
		return nil, err
	}
	defer parent.Close()

	turns := parent.CompletedTurns()
	if *target.TurnIndex >= len(turns) {
		return nil, ErrInvalidTurn
	}
	turn := turns[*target.TurnIndex]
	cp, err := turn.Checkpoint()
	if err != nil {
		return nil, err
	}
	childStored, err := m.Store.BranchFromCheckpoint(ctx, parent.stored, BranchFromCheckpointOptions{
		ID:              cfg.copyID,
		Kind:            cfg.copyKind,
		Owner:           m.Owner,
		Checkpoint:      cp,
		SourceTurnIndex: turn.Index(),
		SourceTurnRole:  turn.Role(),
		SourceSeq:       turn.Seq(),
		SourceHeadSeq:   parent.Seq(),
	})
	if err != nil {
		return nil, err
	}
	return loadStoredBranch(childStored, RestoreOptions{AllowUnsafe: true}, !cfg.withoutEventLoop)
}

func loadStoredBranch(stored *StoredBranch, opts RestoreOptions, withEventLoop bool) (*Branch, error) {
	branch, err := stored.Load(opts)
	if err != nil {
		_ = stored.Close()
		return nil, err
	}
	if withEventLoop {
		branch.eventLoop = NewEventLoop(branch.thread)
		branch.eventLoopDone = make(chan error, 1)
		go func() { branch.eventLoopDone <- branch.eventLoop.Run(context.Background()) }()
	}
	return branch, nil
}

func (m BranchManager[T]) codec() (BranchTargetCodec[T], error) {
	if m.Codec != nil {
		return m.Codec, nil
	}
	if codec, ok := any(DefaultBranchTargetCodec).(BranchTargetCodec[T]); ok && codec != nil {
		return codec, nil
	}
	return nil, ErrBranchTargetCodecRequired
}

type StringBranchTargetCodec struct{}

func (StringBranchTargetCodec) Parse(ref string) (BranchTarget, error) {
	parts := strings.Split(strings.Trim(ref, "/"), "/")
	if len(parts) != 2 && len(parts) != 4 {
		return BranchTarget{}, fmt.Errorf("invalid branch target %q", ref)
	}
	if parts[0] != "branch" || parts[1] == "" {
		return BranchTarget{}, fmt.Errorf("invalid branch target %q", ref)
	}
	if len(parts) == 2 {
		return BranchHeadTarget(BranchID(parts[1])), nil
	}
	if parts[2] != "turn" || parts[3] == "" {
		return BranchTarget{}, fmt.Errorf("invalid branch target %q", ref)
	}
	index, err := strconv.Atoi(parts[3])
	if err != nil || index < 0 {
		return BranchTarget{}, fmt.Errorf("invalid branch target %q", ref)
	}
	return BranchTurnTarget(BranchID(parts[1]), index), nil
}

func (StringBranchTargetCodec) Format(target BranchTarget) (string, error) {
	if target.BranchID == "" {
		return "", fmt.Errorf("invalid branch target: empty branch id")
	}
	ref := "/branch/" + string(target.BranchID)
	if target.TurnIndex != nil {
		if *target.TurnIndex < 0 {
			return "", fmt.Errorf("invalid branch target: negative turn index")
		}
		ref += "/turn/" + strconv.Itoa(*target.TurnIndex)
	}
	return ref, nil
}
