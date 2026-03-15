package threads

import "iter"

type item[T any] struct {
	Item T
	Next *item[T]
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

func (l *itemList[T]) Append(block T) *item[T] {
	n := &item[T]{Item: block}
	if l.tail == nil {
		l.head = n
		l.tail = n
		return n
	}
	l.tail.Next = n
	l.tail = n
	return n
}

func (l *itemList[T]) InsertAfter(after *item[T], block T) *item[T] {
	n := &item[T]{Item: block}
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
