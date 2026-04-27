package threads

import (
	"context"
	"errors"
	"sync"
)

var ErrEventLoopClosed = errors.New("threads event loop closed")
var ErrEventLoopAlreadyStarted = errors.New("threads event loop already started")

type eventLoopOp struct {
	fn   func(*Thread) error
	done chan error
}

// EventLoop owns the mutation lane for a Thread.
//
// While an EventLoop owns a Thread, callers must not call Thread methods
// directly. Use Do to inspect or mutate the Thread so async tool completions,
// model stream events, recovery, and user actions are serialized in one place.
type EventLoop struct {
	thread *Thread
	ops    chan eventLoopOp
	closed chan struct{}
	done   chan struct{}

	closeOnce sync.Once
	runOnce   sync.Once
}

func NewEventLoop(t *Thread) *EventLoop {
	l := &EventLoop{
		thread: t,
		ops:    make(chan eventLoopOp),
		closed: make(chan struct{}),
		done:   make(chan struct{}),
	}
	t.setEventLoop(l)
	return l
}

// Run processes EventLoop.Do calls until ctx is canceled or Close is called.
func (l *EventLoop) Run(ctx context.Context) error {
	started := false
	l.runOnce.Do(func() { started = true })
	if !started {
		return ErrEventLoopAlreadyStarted
	}
	defer close(l.done)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-l.closed:
			return nil
		case op := <-l.ops:
			op.done <- op.fn(l.thread)
		}
	}
}

// Do runs fn on the EventLoop's thread mutation lane. The supplied Thread may
// be used with its full API only for the duration of fn.
func (l *EventLoop) Do(ctx context.Context, fn func(*Thread) error) error {
	if fn == nil {
		panic("threads event loop do requires a function")
	}
	done := make(chan error, 1)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-l.closed:
		return ErrEventLoopClosed
	case <-l.done:
		return ErrEventLoopClosed
	case l.ops <- eventLoopOp{fn: fn, done: done}:
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close stops accepting new Do calls and lets Run return. It is idempotent.
func (l *EventLoop) Close() error {
	l.closeOnce.Do(func() {
		l.thread.setEventLoop(nil)
		close(l.closed)
	})
	return nil
}
