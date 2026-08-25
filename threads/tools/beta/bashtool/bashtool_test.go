package bashtool_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/tools/beta/bashtool"
)

func TestBashToolImplementsAgentLoomInterfacesAndOffersBash(t *testing.T) {
	tool := bashtool.New(bashtool.Config{CWD: t.TempDir()})
	var _ threads.ToolProvider = tool
	var _ threads.ToolResolver = tool

	toolLoad(t, tool, "bash")
}

func TestBashToolRunsCommandInDurableCWD(t *testing.T) {
	oldDir := t.TempDir()
	newDir := t.TempDir()
	oldProvider := bashtool.New(bashtool.Config{CWD: oldDir})
	restartedResolver := bashtool.New(bashtool.Config{CWD: newDir})

	result := runBash(t, restartedResolver, toolLoad(t, oldProvider, "bash"), `{"command":"printf %s \"$PWD\""}`)
	if result.Output != oldDir {
		t.Fatalf("expected command to run in durable cwd %q, got %q", oldDir, result.Output)
	}
}

func TestBashToolNonZeroExitReturnsToolError(t *testing.T) {
	dir := t.TempDir()
	tool := bashtool.New(bashtool.Config{CWD: dir})

	result := runBash(t, tool, toolLoad(t, tool, "bash"), `{"command":"printf fail >&2; exit 7"}`)
	if !strings.Contains(result.Output, "fail") || !strings.Contains(result.Output, "exit code 7") {
		t.Fatalf("expected stderr and exit code in output, got %q", result.Output)
	}
	if result.Data["error"] == nil {
		t.Fatalf("expected structured error data, got %#v", result.Data)
	}
}

func TestBashToolCapturesStdoutAndStderr(t *testing.T) {
	tool := bashtool.New(bashtool.Config{CWD: t.TempDir()})

	result := runBash(t, tool, toolLoad(t, tool, "bash"), `{"command":"printf stdout; printf stderr >&2"}`)
	if !strings.Contains(result.Output, "stdout") || !strings.Contains(result.Output, "stderr") {
		t.Fatalf("expected stdout and stderr to be captured, got %q", result.Output)
	}
}

func TestBashToolEmptyOutputReturnsUsefulMessage(t *testing.T) {
	tool := bashtool.New(bashtool.Config{CWD: t.TempDir()})

	result := runBash(t, tool, toolLoad(t, tool, "bash"), `{"command":"true"}`)
	if !strings.Contains(result.Output, "(no output)") {
		t.Fatalf("expected useful no-output result, got %q", result.Output)
	}
}

func TestBashToolTimeoutReturnsStructuredError(t *testing.T) {
	tool := bashtool.New(bashtool.Config{CWD: t.TempDir()})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	t.Cleanup(cancel)

	dispatch := resolveBash(t, ctx, tool, toolLoad(t, tool, "bash"), `{"command":"sleep 10"}`)
	if !dispatch.Started || dispatch.Recovery != threads.ToolRecoveryUnsafe {
		t.Fatalf("timed-out bash should be marked started/unsafe, got started=%v recovery=%q", dispatch.Started, dispatch.Recovery)
	}
	result := onlyResult(t, dispatch)
	if result.Data["error"] == nil {
		t.Fatalf("expected structured timeout error data, got %#v", result.Data)
	}
	if !strings.Contains(strings.ToLower(result.Output), "timeout") && !strings.Contains(strings.ToLower(result.Output), "deadline") && !strings.Contains(strings.ToLower(result.Output), "killed") {
		t.Fatalf("expected timeout-like output, got %q", result.Output)
	}
}

func TestBashToolTailTruncatesOutput(t *testing.T) {
	tests := []struct {
		name    string
		config  bashtool.Config
		command string
		want    []string
		notWant []string
	}{
		{
			name:    "max lines keeps final lines",
			config:  bashtool.Config{MaxLines: 2},
			command: "printf 'drop-alpha\\ndrop-beta\\nkeep-gamma\\nkeep-delta\\n'",
			want:    []string{"keep-gamma", "keep-delta"},
			notWant: []string{"drop-alpha", "drop-beta"},
		},
		{
			name:    "max bytes keeps final bytes",
			config:  bashtool.Config{MaxBytes: 8},
			command: "printf abcdefghijklmnopqrstuvwxyz",
			want:    []string{"stuvwxyz"},
			notWant: []string{"abcdefgh"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.config.CWD = t.TempDir()
			tool := bashtool.New(tt.config)
			payload, err := json.Marshal(map[string]string{"command": tt.command})
			if err != nil {
				t.Fatalf("marshal payload: %v", err)
			}

			result := runBash(t, tool, toolLoad(t, tool, "bash"), string(payload))
			for _, want := range tt.want {
				if !strings.Contains(result.Output, want) {
					t.Fatalf("expected output to contain %q, got %q", want, result.Output)
				}
			}
			for _, notWant := range tt.notWant {
				if strings.Contains(result.Output, notWant) {
					t.Fatalf("expected output not to contain truncated content %q, got %q", notWant, result.Output)
				}
			}
			if !strings.Contains(strings.ToLower(result.Output), "truncated") {
				t.Fatalf("expected truncation hint, got %q", result.Output)
			}
		})
	}
}

func TestBashToolInvalidJSONReturnsStructuredErrorBeforeStarted(t *testing.T) {
	tool := bashtool.New(bashtool.Config{CWD: t.TempDir()})

	dispatch := resolveBash(t, context.Background(), tool, toolLoad(t, tool, "bash"), `{`)
	if dispatch.Started {
		t.Fatalf("invalid JSON should fail before command execution starts")
	}
	result := onlyResult(t, dispatch)
	if result.Data["error"] == nil {
		t.Fatalf("expected structured error data, got %#v", result.Data)
	}
}

func TestBashToolAsyncResolveRequiresThreadEventLoop(t *testing.T) {
	tool := bashtool.New(bashtool.Config{CWD: t.TempDir(), Async: true})
	load := toolLoad(t, tool, "bash")
	call := threads.ToolCall{CallID: "c1", Name: "bash", Payload: `{"command":"true"}`}

	_, err := tool.ResolveTool(context.Background(), nil, call, load)
	if err == nil || !strings.Contains(err.Error(), "event loop") {
		t.Fatalf("expected event loop error, got %v", err)
	}
}

func TestBashToolCompletesAsyncCallThroughEventLoop(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "runner.txt")
	thread := threads.New()
	loop := startEventLoop(t, thread)
	tool := bashtool.New(bashtool.Config{CWD: dir, Async: true, Thread: thread})
	var _ threads.ToolProvider = tool
	var _ threads.ToolResolver = tool

	streamer := &toolCallStreamer{call: threads.ToolCall{CallID: "c1", Name: "bash", Payload: `{"command":"printf runner > runner.txt"}`}}
	if err := loop.Do(context.Background(), func(t threads.Thread) error {
		t.SetToolProvider(tool)
		t.SetToolResolver(tool)
		thread.SetExecutor(threads.NewThreadExecutor(streamer))
		t.QueueItem(threads.UserText("run it"))
		t.QueueItem(threads.SendItem{})
		return nil
	}); err != nil {
		t.Fatalf("queue async bash request: %v", err)
	}

	waitFor(t, func() bool {
		buf, err := os.ReadFile(marker)
		return err == nil && string(buf) == "runner"
	})
	waitFor(t, func() bool {
		order := toolLifecycleOrder(t, loop, "c1")
		return strings.Join(order, ",") == "tool_call,tool_call_resolving,tool_call_started,tool_result"
	})

	order := toolLifecycleOrder(t, loop, "c1")
	want := []string{"tool_call", "tool_call_resolving", "tool_call_started", "tool_result"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("unexpected tool lifecycle order: got %#v want %#v", order, want)
	}
	if calls := streamCallCount(t, loop, streamer); calls != 2 {
		t.Fatalf("expected async completion to auto-continue once, got %d stream calls", calls)
	}
}

func TestBashToolAsyncSkipsResultWithoutStartedMarker(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker.txt")
	thread := threads.New()
	loop := startEventLoop(t, thread)
	tool := bashtool.New(bashtool.Config{CWD: dir, Async: true, Thread: thread})
	load := toolLoad(t, tool, "bash")
	call := threads.ToolCall{CallID: "c1", Name: "bash", Payload: `{"command":"printf ran > marker.txt"}`}

	dispatch, err := tool.ResolveTool(context.Background(), nil, call, load)
	if err != nil {
		t.Fatalf("resolve bash: %v", err)
	}
	if !dispatch.Started || dispatch.Recovery != threads.ToolRecoveryUnsafe || len(dispatch.Items) != 0 {
		t.Fatalf("unexpected async dispatch: %#v", dispatch)
	}

	waitFor(t, func() bool {
		buf, err := os.ReadFile(marker)
		return err == nil && string(buf) == "ran"
	})
	if got := toolLifecycleOrder(t, loop, "c1"); len(got) != 0 {
		t.Fatalf("unexpected queued result without durable started marker: %#v", got)
	}
}

func toolLifecycleOrder(t *testing.T, loop *threads.EventLoop, callID string) []string {
	t.Helper()
	var order []string
	if err := loop.Do(context.Background(), func(thread threads.Thread) error {
		snap, err := thread.Snapshot()
		if err != nil {
			return err
		}
		for _, item := range snap.Items {
			if item.ID == callID {
				order = append(order, item.Type)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("snapshot lifecycle: %v", err)
	}
	return order
}

func streamCallCount(t *testing.T, loop *threads.EventLoop, streamer *toolCallStreamer) int {
	t.Helper()
	var calls int
	if err := loop.Do(context.Background(), func(threads.Thread) error {
		calls = streamer.calls
		return nil
	}); err != nil {
		t.Fatalf("read stream call count: %v", err)
	}
	return calls
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

func runBash(t *testing.T, resolver interface {
	ResolveTool(context.Context, threads.Thread, threads.ToolCall, json.RawMessage) (threads.ToolDispatch, error)
}, load json.RawMessage, payload string) threads.ToolCallResult {
	t.Helper()
	dispatch := resolveBash(t, context.Background(), resolver, load, payload)
	if !dispatch.Started || dispatch.Recovery != threads.ToolRecoveryUnsafe {
		t.Fatalf("bash should be marked started/unsafe, got started=%v recovery=%q", dispatch.Started, dispatch.Recovery)
	}
	return onlyResult(t, dispatch)
}

func resolveBash(t *testing.T, ctx context.Context, resolver interface {
	ResolveTool(context.Context, threads.Thread, threads.ToolCall, json.RawMessage) (threads.ToolDispatch, error)
}, load json.RawMessage, payload string) threads.ToolDispatch {
	t.Helper()
	dispatch, err := resolver.ResolveTool(ctx, nil, threads.ToolCall{CallID: "c1", Name: "bash", Payload: payload}, load)
	if err != nil {
		t.Fatalf("resolve bash: %v", err)
	}
	return dispatch
}

func onlyResult(t *testing.T, dispatch threads.ToolDispatch) threads.ToolCallResult {
	t.Helper()
	if len(dispatch.Items) != 1 {
		t.Fatalf("expected one result item, got %#v", dispatch.Items)
	}
	result, ok := dispatch.Items[0].(threads.ToolCallResult)
	if !ok {
		t.Fatalf("expected ToolCallResult, got %T", dispatch.Items[0])
	}
	return result
}

func toolLoad(t *testing.T, provider interface {
	ToolsSnapshot(_ threads.Thread) threads.ToolsSnapshot
}, name string) json.RawMessage {
	t.Helper()
	snap := provider.ToolsSnapshot(nil)
	if len(snap.Snapshot.Offered) != 1 || snap.Snapshot.Offered[0].Name != name {
		t.Fatalf("unexpected offered tools: %#v", snap.Snapshot.Offered)
	}
	for _, binding := range snap.Handlers {
		if binding.Name == name {
			if len(binding.HandlerLoadData) == 0 {
				t.Fatalf("handler load data for %q is empty", name)
			}
			return binding.HandlerLoadData
		}
	}
	t.Fatalf("missing handler binding for %q", name)
	return nil
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	buf, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	return string(buf)
}

type toolCallStreamer struct {
	call  threads.ToolCall
	calls int
}

func (s *toolCallStreamer) Capabilities() threads.StreamerCapabilities {
	return threads.StreamerCapabilities{}
}

func (s *toolCallStreamer) RegisterToolNormalizer(string, threads.ToolNormalizer) {}

func (s *toolCallStreamer) UnregisterToolNormalizer(string) {}

func (s *toolCallStreamer) StreamReq(_ threads.Req, emit func(threads.Item) error) error {
	s.calls++
	if s.calls == 1 {
		return emit(s.call)
	}
	return nil
}
