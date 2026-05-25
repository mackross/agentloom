package threads

import (
	"context"
	"errors"
	"testing"
)

func TestSeqAndCompletedTurnsExposeBranchableConversation(t *testing.T) {
	thread := newThread()
	if got := thread.Seq(); got != 0 {
		t.Fatalf("new thread seq = %d, want 0", got)
	}

	thread.QueueItem(UserText("hello"))
	thread.QueueItem(UserText(" world"))
	thread.QueueItem(SendItem{})
	thread.QueueItem(AssistantText("hi"))
	thread.QueueItem(AssistantText(" there"))
	thread.QueueItem(UserText("again"))

	if got := thread.Seq(); got == 0 {
		t.Fatal("seq did not advance")
	}

	turns := thread.CompletedTurns()
	if got, want := len(turns), 3; got != want {
		t.Fatalf("completed turns = %d, want %d", got, want)
	}
	assertTurn := func(i int, role TurnRole, text string) {
		t.Helper()
		if turns[i].Index() != i || turns[i].Role() != role || turns[i].Text() != text {
			t.Fatalf("turn %d = index %d role %q text %q", i, turns[i].Index(), turns[i].Role(), turns[i].Text())
		}
	}
	assertTurn(0, TurnUser, "hello world")
	assertTurn(1, TurnAssistant, "hi there")
	assertTurn(2, TurnUser, "again")
}

func TestTurnCheckpointFromUserRestoresRequestReadyPrefix(t *testing.T) {
	thread := newThread()
	thread.QueueItem(UserText("first"))
	thread.QueueItem(SendItem{})
	thread.QueueItem(AssistantText("old"))
	thread.QueueItem(UserText("later"))

	cp, err := thread.CompletedTurns()[0].Checkpoint()
	if err != nil {
		t.Fatalf("checkpoint user turn: %v", err)
	}
	if !cp.Unsafe {
		t.Fatal("user turn checkpoint should be unsafe/request-ready")
	}
	child, err := RestoreCheckpoint(cp, RestoreOptions{AllowUnsafe: true})
	if err != nil {
		t.Fatalf("restore user checkpoint: %v", err)
	}
	if got := child.State(); got != StateConstructLLMRequest {
		t.Fatalf("restored state = %q, want %q", got, StateConstructLLMRequest)
	}

	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
		b.AssertRequest(func(req Req) {
			if got, want := req.Items, []Item{UserText("first")}; len(got) != len(want) || got[0] != want[0] {
				t.Fatalf("request items = %#v, want %#v", got, want)
			}
		})
		b.Emit(AssistantText("new"))
	})
	if err := child.AttachExecutorForRecovery(NewThreadExecutor(streamer.Streamer())); err != nil {
		t.Fatalf("attach restored child executor: %v", err)
	}
	streamer.AssertCallCount(t)
	turns := child.CompletedTurns()
	if got := turns[len(turns)-1].Text(); got != "new" {
		t.Fatalf("alternate assistant = %q, want new", got)
	}
}

func TestTurnCheckpointFromAssistantRestoresSettledPrefix(t *testing.T) {
	thread := newThread()
	thread.QueueItem(UserText("first"))
	thread.QueueItem(SendItem{})
	thread.QueueItem(AssistantText("answer"))
	thread.QueueItem(UserText("discard"))

	cp, err := thread.CompletedTurns()[1].Checkpoint()
	if err != nil {
		t.Fatalf("checkpoint assistant turn: %v", err)
	}
	if cp.Unsafe {
		t.Fatal("assistant turn checkpoint should be safe")
	}
	child, err := RestoreCheckpoint(cp, RestoreOptions{})
	if err != nil {
		t.Fatalf("restore assistant checkpoint: %v", err)
	}
	if got := child.State(); got != StateIdle {
		t.Fatalf("restored state = %q, want idle", got)
	}
	turns := child.CompletedTurns()
	if got, want := len(turns), 2; got != want {
		t.Fatalf("restored turns = %d, want %d", got, want)
	}
	if got := turns[1].Text(); got != "answer" {
		t.Fatalf("assistant text = %q", got)
	}
}

func TestPartialAssistantAndUnresolvedToolsAreNotBranchable(t *testing.T) {
	settledThenStreaming := newThread()
	settledThenStreaming.QueueItem(UserText("u1"))
	settledThenStreaming.QueueItem(SendItem{})
	if err := settledThenStreaming.beginStreaming(); err != nil {
		t.Fatal(err)
	}
	if err := settledThenStreaming.appendStreamItem(AssistantText("a1")); err != nil {
		t.Fatal(err)
	}
	if err := settledThenStreaming.endStreaming(); err != nil {
		t.Fatal(err)
	}
	settledThenStreaming.QueueItem(UserText("u2"))
	settledThenStreaming.QueueItem(SendItem{})
	if err := settledThenStreaming.beginStreaming(); err != nil {
		t.Fatal(err)
	}
	if err := settledThenStreaming.appendStreamItem(AssistantText("partial")); err != nil {
		t.Fatal(err)
	}
	turns := settledThenStreaming.CompletedTurns()
	if got, want := len(turns), 3; got != want {
		t.Fatalf("branchable turns during later stream = %d, want %d", got, want)
	}
	if turns[2].Role() != TurnUser || turns[2].Text() != "u2" {
		t.Fatalf("last branchable turn during later stream = role %q text %q", turns[2].Role(), turns[2].Text())
	}

	streaming := newThread()
	streaming.QueueItem(UserText("u"))
	streaming.QueueItem(SendItem{})
	if err := streaming.beginStreaming(); err != nil {
		t.Fatal(err)
	}
	if err := streaming.appendStreamItem(AssistantText("partial")); err != nil {
		t.Fatal(err)
	}
	turns = streaming.CompletedTurns()
	if got := len(turns); got != 1 {
		t.Fatalf("branchable turns during stream = %d, want only user turn", got)
	}

	tools := newThread()
	tools.QueueItem(UserText("u"))
	tools.QueueItem(SendItem{})
	tools.QueueItem(AssistantText("planning"))
	tools.QueueItem(ToolCall{CallID: "c", Name: "x", Payload: `{}`})
	turns = tools.CompletedTurns()
	if got := len(turns); got != 1 {
		t.Fatalf("unresolved tool call exposed %d turns, want only user turn", got)
	}
}

func TestCompletedTurnsAfterRestoreAndToolResultPrefix(t *testing.T) {
	thread := newThread()
	base, err := thread.Checkpoint(CheckpointOptions{Policy: InflightSkip})
	if err != nil {
		t.Fatalf("base checkpoint: %v", err)
	}
	thread.QueueItem(UserText("u"))
	thread.QueueItem(SendItem{})
	thread.QueueItem(AssistantText("using tool"))
	thread.QueueItem(ToolCall{CallID: "c1", Name: "lookup", Payload: `{}`})
	thread.QueueItem(ToolCallResult{CallID: "c1", Output: "42"})
	thread.QueueItem(AssistantText("answer"))

	restored, err := RestoreFromCheckpointAndWAL(base, thread.WALAfter(base.Seq), RestoreOptions{AllowUnsafe: true})
	if err != nil {
		t.Fatalf("restore checkpoint: %v", err)
	}
	turns := restored.CompletedTurns()
	if got, want := len(turns), 2; got != want {
		t.Fatalf("restored turns = %d, want %d", got, want)
	}
	if turns[1].Role() != TurnAssistant || turns[1].Text() != "using toolanswer" {
		t.Fatalf("assistant turn after restore = role %q text %q", turns[1].Role(), turns[1].Text())
	}
	if _, err := turns[1].Checkpoint(); err != nil {
		t.Fatalf("checkpoint restored assistant/tool turn: %v", err)
	}
}

func TestTurnCheckpointInsideEventLoopCanBranch(t *testing.T) {
	th := newThread()
	loop := NewEventLoop(th)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- loop.Run(ctx) }()

	var cp Checkpoint
	var sourceHeadSeq uint32
	err := loop.doLocal(ctx, func(th *thread) error {
		th.QueueItem(UserText("branch me"))
		turn := th.CompletedTurns()[0]
		var err error
		cp, err = turn.Checkpoint()
		sourceHeadSeq = th.Seq()
		return err
	})
	if err != nil {
		t.Fatalf("event loop checkpoint: %v", err)
	}
	if cp.Seq != sourceHeadSeq {
		t.Fatalf("checkpoint seq = %d, source head seq = %d", cp.Seq, sourceHeadSeq)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("event loop run = %v", err)
	}
}

func TestTurnCheckpointRejectsZeroAndStaleTurns(t *testing.T) {
	if _, err := (Turn{}).Checkpoint(); err == nil {
		t.Fatal("zero turn checkpoint succeeded")
	}
	thread := newThread()
	thread.QueueItem(UserText("u"))
	turn := thread.CompletedTurns()[0]
	thread.QueueItem(UserText(" mutation"))
	if _, err := turn.Checkpoint(); err == nil {
		t.Fatal("stale turn checkpoint succeeded")
	}
}
