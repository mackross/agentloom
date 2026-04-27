package threads

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestEventLoopDoRunsOnThreadLane(t *testing.T) {
	thread := New()
	loop := NewEventLoop(thread)
	if thread.loop != loop {
		t.Fatal("expected event loop to mark thread ownership")
	}
	runErr := make(chan error, 1)
	go func() { runErr <- loop.Run(context.Background()) }()
	defer func() {
		_ = loop.Close()
		<-runErr
		if thread.loop != nil {
			t.Fatal("expected close to clear thread event loop ownership")
		}
	}()

	if err := loop.Do(context.Background(), func(t *Thread) error {
		t.QueueItem(UserText("hello"))
		return nil
	}); err != nil {
		t.Fatalf("event loop do: %v", err)
	}

	var got []Item
	if err := loop.Do(context.Background(), func(t *Thread) error {
		got = t.items.Slice()
		return nil
	}); err != nil {
		t.Fatalf("event loop read: %v", err)
	}
	if want := []Item{UserText("hello")}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected thread items: %#v", got)
	}
}

func TestEventLoopCloseStopsRunAndRejectsDo(t *testing.T) {
	loop := NewEventLoop(New())
	runErr := make(chan error, 1)
	go func() { runErr <- loop.Run(context.Background()) }()

	if err := loop.Close(); err != nil {
		t.Fatalf("event loop close: %v", err)
	}
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("event loop run after close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event loop to stop")
	}

	err := loop.Do(context.Background(), func(*Thread) error { return nil })
	if !errors.Is(err, ErrEventLoopClosed) {
		t.Fatalf("expected closed error, got %v", err)
	}
}

func TestEventLoopPanicsWhenThreadAlreadyOwned(t *testing.T) {
	thread := New()
	loop := NewEventLoop(thread)
	defer func() { _ = loop.Close() }()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	_ = NewEventLoop(thread)
}

func TestEventLoopRunCanOnlyStartOnce(t *testing.T) {
	loop := NewEventLoop(New())
	if err := loop.Close(); err != nil {
		t.Fatalf("event loop close: %v", err)
	}
	if err := loop.Run(context.Background()); err != nil {
		t.Fatalf("first run after close should stop cleanly: %v", err)
	}
	if err := loop.Run(context.Background()); !errors.Is(err, ErrEventLoopAlreadyStarted) {
		t.Fatalf("expected already-started error, got %v", err)
	}
}

func TestEventLoopDoRejectsAfterRunContextEnds(t *testing.T) {
	loop := NewEventLoop(New())
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- loop.Run(ctx) }()
	cancel()
	select {
	case err := <-runErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected canceled run, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event loop context cancellation")
	}

	err := loop.Do(context.Background(), func(*Thread) error { return nil })
	if !errors.Is(err, ErrEventLoopClosed) {
		t.Fatalf("expected closed error, got %v", err)
	}
}

func TestEventLoopDoPanicsWithNilFunc(t *testing.T) {
	loop := NewEventLoop(New())
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	if err := loop.Do(context.Background(), nil); err != nil {
		t.Fatalf("unexpected error before panic: %v", err)
	}
}
