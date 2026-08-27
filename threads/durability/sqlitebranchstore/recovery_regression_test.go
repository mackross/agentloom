package sqlitebranchstore_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/durability/sqlitebranchstore"
)

var errInterruptedRecoveryRegressionStream = errors.New("test stream interrupted")

type recoveryRegressionToolProvider struct{}

func (recoveryRegressionToolProvider) ToolsSnapshot(threads.Thread) threads.ToolsSnapshot {
	return threads.ToolsSnapshot{
		Handlers: []threads.ToolHandlerBinding{{Name: "historical_tool"}},
	}
}

type recoveryRegressionToolResolver struct{}

func (recoveryRegressionToolResolver) ResolveTool(_ context.Context, _ threads.Thread, call threads.ToolCall, _ json.RawMessage) (threads.ToolDispatch, error) {
	return threads.ToolDispatch{
		Continue: threads.ToolContinueManual,
		Items: []threads.Item{
			threads.ToolCallResult{CallID: call.CallID, Output: "historical-tool-result"},
		},
	}, nil
}

type recoveryRegressionHistoryStreamer struct {
	requests      int
	interruptNext bool
}

func (s *recoveryRegressionHistoryStreamer) Capabilities() threads.StreamerCapabilities {
	return threads.StreamerCapabilities{AssistantPrefix: true}
}

func (*recoveryRegressionHistoryStreamer) RegisterToolNormalizer(string, threads.ToolNormalizer) {
}

func (*recoveryRegressionHistoryStreamer) UnregisterToolNormalizer(string) {}

func (s *recoveryRegressionHistoryStreamer) StreamReq(_ threads.Req, emit func(threads.Item) error) error {
	s.requests++
	if s.interruptNext {
		if err := emit(threads.AssistantText("interrupted-current-prefix")); err != nil {
			return err
		}
		if err := emit(threads.ToolCallChunk{
			CallID:       "interrupted-tool",
			Name:         "write",
			PayloadDelta: `{"content":"partial`,
		}); err != nil {
			return err
		}
		return errInterruptedRecoveryRegressionStream
	}
	if s.requests == 1 {
		if err := emit(threads.AssistantText("assistant-before-historical-tool")); err != nil {
			return err
		}
		return emit(threads.ToolCall{
			CallID:  "historical-tool-call",
			Name:    "historical_tool",
			Payload: `{}`,
		})
	}
	return emit(threads.AssistantText(fmt.Sprintf("completed-assistant-%02d", s.requests)))
}

type recoveryRegressionResumeStreamer struct{}

func (recoveryRegressionResumeStreamer) Capabilities() threads.StreamerCapabilities {
	return threads.StreamerCapabilities{AssistantPrefix: true}
}

func (recoveryRegressionResumeStreamer) RegisterToolNormalizer(string, threads.ToolNormalizer) {
}

func (recoveryRegressionResumeStreamer) UnregisterToolNormalizer(string) {}

func (recoveryRegressionResumeStreamer) StreamReq(_ threads.Req, emit func(threads.Item) error) error {
	return emit(threads.AssistantText("recovered-current-response"))
}

// TestInterruptedStreamRecoveryPreservesCompletedTranscript exercises the full
// durable lifecycle rather than the recovery helper in isolation:
//
//   - generate completed history through a real ThreadExecutor, including a
//     completed historical tool call;
//   - checkpoint it in SQLite;
//   - persist an interrupted later tool stream as WAL;
//   - restore it as Weaver does, attach an assistant-prefix-capable executor
//     with Weaver's recovery policy, and checkpoint the recovered idle state;
//   - close the simulated crash/recovery loop by loading from SQLite again.
//
// Recovery may rewrite the interrupted request, but it must never discard any
// transcript turn that was completed before that request began.
func TestInterruptedStreamRecoveryPreservesCompletedTranscript(t *testing.T) {
	ctx := context.Background()
	store, err := sqlitebranchstore.OpenSQLiteBranchStore(
		filepath.Join(t.TempDir(), "threads.sqlite3"),
		sqlitebranchstore.SQLiteBranchStoreOptions{},
	)
	if err != nil {
		t.Fatalf("open SQLite branch store: %v", err)
	}
	defer store.Close()

	branch, err := store.CreateBranch(ctx, threads.BranchCreateOptions{ID: "recovery-regression"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}
	defer branch.Close()

	historyStreamer := &recoveryRegressionHistoryStreamer{}
	source := threads.New()
	source.SetDurableStore(branch.Durable)
	source.SetToolProvider(recoveryRegressionToolProvider{})
	source.SetToolResolver(recoveryRegressionToolResolver{})
	source.SetExecutor(threads.NewThreadExecutor(historyStreamer))

	source.QueueItem(threads.UserText("historical-user"))
	source.QueueItem(threads.SendItem{})
	if got := source.State(); got != threads.StateIdle {
		t.Fatalf("state after historical tool turn = %s, want idle", got)
	}

	for i := 0; i < 8; i++ {
		source.QueueItem(threads.UserText(fmt.Sprintf("completed-user-%02d", i)))
		source.QueueItem(threads.SendItem{})
		if got := source.State(); got != threads.StateIdle {
			t.Fatalf("state after completed turn %d = %s, want idle", i, got)
		}
	}

	canonicalSnapshot, err := source.Snapshot()
	if err != nil {
		t.Fatalf("snapshot completed history: %v", err)
	}
	wantCompleted := transcriptBlocks(canonicalSnapshot)
	if len(wantCompleted) < 10 {
		t.Fatalf("fixture produced only %d completed transcript blocks: %#v", len(wantCompleted), canonicalSnapshot.Items)
	}
	if !snapshotHasItem(canonicalSnapshot, "tool_call", "historical-tool-call") {
		t.Fatalf("fixture is missing its completed historical tool call")
	}
	canonical, err := source.Checkpoint(threads.CheckpointOptions{Policy: threads.InflightSkip})
	if err != nil {
		t.Fatalf("checkpoint completed history: %v", err)
	}
	branch.Durable.ReplaceSnapshot(canonical)

	var streamErr error
	source.SetDelegate(threads.ThreadDelegateFuncs{
		OnExecutorError: func(_ threads.Thread, err error) {
			streamErr = err
		},
	})
	historyStreamer.interruptNext = true
	source.QueueItem(threads.UserText("current-user"))
	source.QueueItem(threads.SendItem{})
	if !errors.Is(streamErr, errInterruptedRecoveryRegressionStream) {
		t.Fatalf("stream error = %v, want interruption", streamErr)
	}
	if got := source.State(); got != threads.StateReceivingStream {
		t.Fatalf("state after interruption = %s, want receiving_stream", got)
	}

	checkpoint, wal := branch.Durable.Load()
	if len(wal) == 0 {
		t.Fatalf("interrupted request did not reach durable WAL")
	}
	recovered, err := threads.RestoreFromCheckpointAndWAL(
		checkpoint,
		wal,
		threads.RestoreOptions{AllowUnsafe: true},
	)
	if err != nil {
		t.Fatalf("restore interrupted thread: %v", err)
	}
	recovered.SetDurableStore(branch.Durable)
	if err := recovered.AttachExecutorForRecoveryWithOptions(
		threads.NewThreadExecutor(recoveryRegressionResumeStreamer{}),
		threads.RecoveryOptions{
			ToolChunkPolicy: threads.ToolChunkRecoveryKeepAssistantPrefix,
			ToolCallPolicy:  threads.ToolCallRecoveryCancelAll,
		},
	); err != nil {
		t.Fatalf("recover interrupted stream: %v", err)
	}
	if got := recovered.State(); got != threads.StateIdle {
		t.Fatalf("state after recovery = %s, want idle", got)
	}

	safe, err := recovered.Checkpoint(threads.CheckpointOptions{Policy: threads.InflightSkip})
	if err != nil {
		t.Fatalf("checkpoint recovered thread: %v", err)
	}
	branch.Durable.ReplaceSnapshot(safe)

	finalCheckpoint, finalWAL := branch.Durable.Load()
	if len(finalWAL) != 0 {
		t.Fatalf("final checkpoint left %d WAL events", len(finalWAL))
	}
	finalThread, err := threads.RestoreFromCheckpointAndWAL(
		finalCheckpoint,
		finalWAL,
		threads.RestoreOptions{},
	)
	if err != nil {
		t.Fatalf("reload recovered thread: %v", err)
	}
	finalSnapshot, err := finalThread.Snapshot()
	if err != nil {
		t.Fatalf("snapshot reloaded thread: %v", err)
	}
	gotCompleted := transcriptBlocks(finalSnapshot)
	if len(gotCompleted) < len(wantCompleted) {
		t.Fatalf(
			"recovery discarded completed transcript: got %d blocks, want at least %d\nbefore: %v\nafter:  %v",
			len(gotCompleted),
			len(wantCompleted),
			wantCompleted,
			gotCompleted,
		)
	}
	for i, want := range wantCompleted {
		if got := gotCompleted[i]; got != want {
			t.Fatalf(
				"recovery changed completed transcript turn %d: got %q, want %q\nbefore: %v\nafter:  %v",
				i,
				got,
				want,
				wantCompleted,
				gotCompleted,
			)
		}
	}
	if !snapshotHasText(finalSnapshot, "user_text", "current-user") {
		t.Fatalf("recovery did not retain the interrupted request: %v", gotCompleted)
	}
	if !snapshotHasText(finalSnapshot, "assistant_text", "recovered-current-response") {
		t.Fatalf("recovery did not complete the interrupted request: %v", gotCompleted)
	}
}

func transcriptBlocks(snapshot threads.ThreadSnapshot) []string {
	var out []string
	for _, item := range snapshot.Items {
		switch item.Type {
		case "user_text", "assistant_text":
			out = append(out, fmt.Sprintf("%s:%s", item.Type, item.Text))
		}
	}
	return out
}

func snapshotHasItem(snapshot threads.ThreadSnapshot, kind, id string) bool {
	for _, item := range snapshot.Items {
		if item.Type == kind && item.ID == id {
			return true
		}
	}
	return false
}

func snapshotHasText(snapshot threads.ThreadSnapshot, kind, text string) bool {
	for _, item := range snapshot.Items {
		if item.Type == kind && strings.Contains(item.Text, text) {
			return true
		}
	}
	return false
}
