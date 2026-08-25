package websearchtool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mackross/agentloom/threads"
)

type testSearcher struct {
	out string
	err error
}

func (s testSearcher) Search(context.Context, args) (string, error) {
	return s.out, s.err
}

func (testSearcher) Payload() threads.ToolPayload { return threads.ToolPayloadFor[args]() }

func TestResolveToolStartsAsyncSearch(t *testing.T) {
	thread := threads.New()
	_ = startEventLoop(t, thread)
	tool := New(Config{APIKey: "invalid-key", MaxLines: 100, MaxBytes: 10000, Thread: thread})
	load, err := json.Marshal(Config{APIKey: "invalid-key", MaxLines: 100, MaxBytes: 10000})
	if err != nil {
		t.Fatal(err)
	}
	dispatch, err := tool.ResolveTool(context.Background(), nil, threads.ToolCall{
		CallID:  "c1",
		Name:    name,
		Payload: `{"query":"Go programming language","count":1}`,
	}, load)
	if err != nil {
		t.Fatalf("ResolveTool: %v", err)
	}
	if !dispatch.Started || dispatch.Recovery != threads.ToolRecoveryUnsafe {
		t.Fatalf("web search should be marked started/unsafe, got started=%v recovery=%q", dispatch.Started, dispatch.Recovery)
	}
	if len(dispatch.Items) != 0 {
		t.Fatalf("async dispatch should not include immediate items: %#v", dispatch.Items)
	}
}

func TestResolveToolAsyncRequiresThreadEventLoop(t *testing.T) {
	tool := New(Config{APIKey: "invalid-key", MaxLines: 100, MaxBytes: 10000})
	load, err := json.Marshal(Config{APIKey: "invalid-key", MaxLines: 100, MaxBytes: 10000})
	if err != nil {
		t.Fatal(err)
	}
	_, err = tool.ResolveTool(context.Background(), nil, threads.ToolCall{
		CallID:  "c1",
		Name:    name,
		Payload: `{"query":"Go programming language","count":1}`,
	}, load)
	if err == nil || !strings.Contains(err.Error(), "event loop") {
		t.Fatalf("expected event loop error, got %v", err)
	}
}

func TestResolveAsyncQueuesSearchResult(t *testing.T) {
	thread := threads.New()
	loop := startEventLoop(t, thread)
	call := threads.ToolCall{CallID: "c1", Name: name, Payload: `{"query":"Go programming language","count":1}`}
	if err := loop.Do(context.Background(), func(thread threads.Thread) error {
		thread.QueueItem(call)
		thread.QueueItem(threads.ToolCallStarted{CallID: call.CallID, Recovery: threads.ToolRecoveryUnsafe})
		return nil
	}); err != nil {
		t.Fatalf("queue call: %v", err)
	}
	if _, err := resolveAsync(context.Background(), thread, call, Config{MaxLines: 100, MaxBytes: 10000}, testSearcher{out: "search output"}, args{Query: "Go programming language", Count: 1}); err != nil {
		t.Fatalf("resolve async: %v", err)
	}
	waitFor(t, func() bool {
		return hasToolResult(t, loop, call.CallID, "search output")
	})
}

func TestResolveAsyncQueuesSearchError(t *testing.T) {
	thread := threads.New()
	loop := startEventLoop(t, thread)
	call := threads.ToolCall{CallID: "c1", Name: name, Payload: `{"query":"Go programming language","count":1}`}
	if err := loop.Do(context.Background(), func(thread threads.Thread) error {
		thread.QueueItem(call)
		thread.QueueItem(threads.ToolCallStarted{CallID: call.CallID, Recovery: threads.ToolRecoveryUnsafe})
		return nil
	}); err != nil {
		t.Fatalf("queue call: %v", err)
	}
	if _, err := resolveAsync(context.Background(), thread, call, Config{MaxLines: 100, MaxBytes: 10000}, testSearcher{err: errors.New("boom")}, args{Query: "Go programming language", Count: 1}); err != nil {
		t.Fatalf("resolve async: %v", err)
	}
	waitFor(t, func() bool {
		return hasToolResult(t, loop, call.CallID, "error: boom")
	})
}

func startEventLoop(t *testing.T, thread threads.Thread) *threads.EventLoop {
	t.Helper()
	loop := threads.NewEventLoop(thread)
	runErr := make(chan error, 1)
	go func() { runErr <- loop.Run(context.Background()) }()
	t.Cleanup(func() {
		if err := loop.Close(); err != nil {
			t.Errorf("close event loop: %v", err)
		}
		select {
		case err := <-runErr:
			if err != nil {
				t.Errorf("run event loop: %v", err)
			}
		case <-time.After(time.Second):
			t.Errorf("timed out waiting for event loop to stop")
		}
	})
	return loop
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition was not met before timeout")
}

func hasToolResult(t *testing.T, loop *threads.EventLoop, callID, want string) bool {
	t.Helper()
	var found bool
	if err := loop.Do(context.Background(), func(thread threads.Thread) error {
		snap, err := thread.Snapshot()
		if err != nil {
			return err
		}
		for _, item := range snap.Items {
			if item.ID == callID && item.Type == "tool_result" && strings.Contains(item.Output, want) {
				found = true
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	return found
}
