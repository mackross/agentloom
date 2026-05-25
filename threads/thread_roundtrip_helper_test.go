package threads

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

const roundTripEnv = "THREAD_TEST_SERIALIZE_ROUNDTRIP"

type testThread struct {
	t *testing.T
	*thread

	executor         stateObserver
	delegate         ThreadDelegate
	recoveryAttached bool

	roundTripEnabled bool
	queueDepth       int
}

func newTestThread(t *testing.T) *testThread {
	t.Helper()
	return &testThread{
		t:                t,
		thread:           newThread(),
		roundTripEnabled: os.Getenv(roundTripEnv) == "1",
	}
}

func (x *testThread) SetExecutor(e stateObserver) {
	x.executor = e
	x.recoveryAttached = false
	x.thread.setExecutor(e)
}

func (x *testThread) AttachExecutorForRecovery(e stateObserver) error {
	err := x.thread.attachExecutorForRecoveryWithOptions(e, RecoveryOptions{})
	if err != nil {
		return err
	}
	x.executor = e
	x.recoveryAttached = true
	return nil
}

func (x *testThread) SetDelegate(d ThreadDelegate) {
	x.delegate = d
	x.thread.SetDelegate(d)
}

func (x *testThread) QueueItem(v Item) {
	x.queueDepth++
	x.thread.QueueItem(v)
	x.queueDepth--

	if x.queueDepth == 0 {
		x.maybeRoundTrip()
	}
}

func (x *testThread) maybeRoundTrip() {
	if !x.roundTripEnabled {
		return
	}
	firstSnap, err := x.thread.Snapshot()
	if err != nil {
		x.t.Fatalf("snapshot thread: %v", err)
	}
	first, err := json.Marshal(firstSnap)
	if err != nil {
		x.t.Fatalf("marshal snapshot: %v", err)
	}
	var decodedSnap ThreadSnapshot
	if err := json.Unmarshal(first, &decodedSnap); err != nil {
		x.t.Fatalf("unmarshal snapshot: %v", err)
	}
	next, err := RestoreThreadSnapshot(decodedSnap)
	if err != nil {
		x.t.Fatalf("restore snapshot: %v", err)
	}
	if x.executor != nil {
		if x.recoveryAttached {
			if err := next.attachExecutorForRecoveryWithOptions(x.executor, RecoveryOptions{}); err != nil {
				x.t.Fatalf("re-attach executor for recovery: %v", err)
			}
		} else {
			next.setExecutor(x.executor)
		}
	}
	if x.delegate != nil {
		next.SetDelegate(x.delegate)
	}
	secondSnap, err := next.Snapshot()
	if err != nil {
		x.t.Fatalf("snapshot restored thread: %v", err)
	}
	second, err := json.Marshal(secondSnap)
	if err != nil {
		x.t.Fatalf("re-marshal snapshot: %v", err)
	}
	if !bytes.Equal(first, second) {
		x.t.Fatalf("thread snapshot encode/decode/encode mismatch\nfirst:  %s\nsecond: %s", string(first), string(second))
	}
	x.thread = next
}
