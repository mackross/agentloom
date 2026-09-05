package threads

import (
	"errors"
	"iter"
	"math"
)

type item[T any] struct {
	Item     T
	Next     *item[T]
	Seq      ItemSeq
	Metadata map[string]any // nil for the common case
}

var errItemSequenceOverflow = errors.New("threads item sequence overflow")

// advanceMutationSeq allocates and commits the next mutation sequence. It is
// used by QueueItem, streaming control, metadata patches, and WAL replay, which
// all consume exactly one sequence. Sequence zero is never produced and
// wraparound beyond ItemSeq panics; mutation sequences are durable identities
// and must never reuse an old value.
func (t *thread) advanceMutationSeq() ItemSeq {
	seq, err := t.candidateMutationSeq()
	if err != nil {
		panic(err.Error())
	}
	t.mutationSeq = uint32(seq)
	return seq
}

// candidateMutationSeq computes the next mutation sequence without committing
// it. It is used where the candidate may be discarded, such as before-send
// placement and user-turn branch checkpoint construction. The caller can
// return the overflow error without moving the source sequence forward.
func (t *thread) candidateMutationSeq() (ItemSeq, error) {
	if t.mutationSeq == math.MaxUint32 {
		return 0, errItemSequenceOverflow
	}
	return ItemSeq(t.mutationSeq + 1), nil
}

// itemCandidate carries a possible node through live or replay insertion.
// Its sequence may become a materialized item identity or be discarded by
// coalescing/chunk replacement; metadata follows the same candidate either way.
type itemCandidate struct {
	Seq      ItemSeq
	Item     Item
	Metadata map[string]any
}

type itemList[T any] struct {
	head *item[T]
	tail *item[T]
}

func (l *itemList[T]) Head() *item[T] {
	return l.head
}

func (l *itemList[T]) Tail() *item[T] {
	return l.tail
}

func (l *itemList[T]) Append(post itemCandidate) *item[T] {
	n := &item[T]{Item: any(post.Item).(T), Seq: post.Seq, Metadata: post.Metadata}
	if l.tail == nil {
		l.head = n
		l.tail = n
		return n
	}
	l.tail.Next = n
	l.tail = n
	return n
}

func (l *itemList[T]) InsertAfter(after *item[T], post itemCandidate) *item[T] {
	n := &item[T]{Item: any(post.Item).(T), Seq: post.Seq, Metadata: post.Metadata}
	if after == nil {
		n.Next = l.head
		l.head = n
		if l.tail == nil {
			l.tail = n
		}
		return n
	}
	n.Next = after.Next
	after.Next = n
	if l.tail == after {
		l.tail = n
	}
	return n
}

func (l *itemList[T]) RemoveAfter(after *item[T]) *item[T] {
	if after == nil {
		removed := l.head
		if removed == nil {
			return nil
		}
		l.head = removed.Next
		if l.tail == removed {
			l.tail = nil
		}
		return removed
	}

	removed := after.Next
	if removed == nil {
		return nil
	}
	after.Next = removed.Next
	if l.tail == removed {
		l.tail = after
	}
	return removed
}

func (l *itemList[T]) Iter2() iter.Seq2[int, T] {
	return func(yield func(int, T) bool) {
		i := 0
		for n := l.head; n != nil; n = n.Next {
			if !yield(i, n.Item) {
				return
			}
			i++
		}
	}
}

func (l *itemList[T]) Slice() []T {
	out := []T{}
	for _, v := range l.Iter2() {
		out = append(out, v)
	}
	return out
}

func (l *itemList[T]) SliceThrough(end *item[T]) []T {
	out := []T{}
	if end == nil {
		return out
	}
	for n := l.head; n != nil; n = n.Next {
		out = append(out, n.Item)
		if n == end {
			break
		}
	}
	return out
}
