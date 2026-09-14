package ollama

import (
	"context"
	"io"
	"sync"
	"time"
)

// streamProgressWatchdog bounds response inactivity, not total generation time.
// It starts before the HTTP request, then resets on headers and every body read,
// including partial JSON and reasoning that the decoder has not yet emitted.
type streamProgressWatchdog struct {
	mu           sync.Mutex
	timer        *time.Timer
	timeout      time.Duration
	lastProgress time.Time
	cancel       context.CancelCauseFunc
	stopped      bool
}

func newStreamProgressWatchdog(parent context.Context, timeout time.Duration) (context.Context, *streamProgressWatchdog) {
	ctx, cancel := context.WithCancelCause(parent)
	w := &streamProgressWatchdog{
		timeout: timeout, lastProgress: time.Now(), cancel: cancel,
	}
	w.mu.Lock()
	w.timer = time.AfterFunc(timeout, w.expire)
	w.mu.Unlock()
	return ctx, w
}

func (w *streamProgressWatchdog) expire() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return
	}
	// Reset cannot prevent an AfterFunc callback that is already running.
	// Recheck progress under the lock before allowing such a callback to cancel.
	if remaining := w.timeout - time.Since(w.lastProgress); remaining > 0 {
		w.timer.Reset(remaining)
		return
	}
	w.stopped = true
	w.cancel(ErrStreamProgressTimeout)
}

func (w *streamProgressWatchdog) progress() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.stopped {
		w.lastProgress = time.Now()
		w.timer.Reset(w.timeout)
	}
}

func (w *streamProgressWatchdog) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopped = true
	w.timer.Stop()
	w.cancel(context.Canceled)
}

type streamProgressReader struct {
	io.Reader
	watchdog *streamProgressWatchdog
}

func (r streamProgressReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if n > 0 {
		r.watchdog.progress()
	}
	return n, err
}
