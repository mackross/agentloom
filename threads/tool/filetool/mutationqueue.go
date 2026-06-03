package filetool

import (
	"sort"
	"sync"
)

type mutationLock struct {
	mu sync.Mutex
}

// MutationQueue serializes file mutations that touch overlapping paths while
// allowing disjoint path sets to proceed concurrently.
type MutationQueue struct {
	locks sync.Map
}

// DefaultMutationQueue is used by file mutation tools when their config does
// not specify a queue. It coordinates writes and patches across independently
// created filetool catalogs in the current process.
var DefaultMutationQueue = NewMutationQueue()

// NewMutationQueue returns an isolated mutation queue. Pass the same queue to
// multiple tool configs to coordinate their mutations, or pass distinct queues
// when isolation is desired.
func NewMutationQueue() *MutationQueue { return &MutationQueue{} }

func mutationQueueOrDefault(q *MutationQueue) *MutationQueue {
	if q != nil {
		return q
	}
	if DefaultMutationQueue != nil {
		return DefaultMutationQueue
	}
	return NewMutationQueue()
}

func (q *MutationQueue) withLocks(paths []string, fn func() error) error {
	locks := make([]*mutationLock, 0, len(paths))
	for _, path := range uniqueStrings(paths) {
		value, _ := q.locks.LoadOrStore(path, &mutationLock{})
		locks = append(locks, value.(*mutationLock))
	}
	for _, lock := range locks {
		lock.mu.Lock()
	}
	defer func() {
		for i := len(locks) - 1; i >= 0; i-- {
			locks[i].mu.Unlock()
		}
	}()
	return fn()
}

func uniqueStrings(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
