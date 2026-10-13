package threads

import (
	"context"
	"errors"
	"testing"
)

type recoveryFailureStreamer struct {
	LLMStreamer
	failure error
}

func (*recoveryFailureStreamer) Capabilities() StreamerCapabilities {
	return StreamerCapabilities{AssistantPrefix: true}
}

func (s *recoveryFailureStreamer) StreamReq(Req, func(Item) error) error {
	return s.failure
}

func TestRecoveredRequestReportsEachExecutorErrorOnce(t *testing.T) {
	for _, state := range []State{StateConstructLLMRequest, StateReceivingStream} {
		t.Run(string(state), func(t *testing.T) {
			want := errors.New("recovered request failed")
			executor := NewThreadExecutor(&recoveryFailureStreamer{failure: want})
			thread := newThread()
			if state == StateReceivingStream {
				thread.SetExecutor(executor)
			}
			thread.QueueItem(UserText("hello"))
			thread.QueueItem(SendItem{})
			if got := thread.State(); got != state {
				t.Fatalf("initial state = %q, want %q", got, state)
			}

			var requests, failures int
			thread.SetDelegate(ThreadDelegateFuncs{
				OnRequest: func(Thread) { requests++ },
				OnExecutorError: func(th Thread, err error) {
					failures++
					if !errors.Is(err, want) {
						t.Errorf("reported error = %v, want %v", err, want)
					}
					if th.State() != StateReceivingStream {
						t.Errorf("reported state = %q, want receiving_stream", th.State())
					}
				},
			})
			for attempt := 1; attempt <= 3; attempt++ {
				if err := thread.AttachExecutorForRecovery(executor); !errors.Is(err, want) {
					t.Fatalf("attach error = %v, want %v", err, want)
				}
				if requests != attempt || failures != attempt {
					t.Fatalf("attempt %d: requests=%d failures=%d, want one notification each",
						attempt, requests, failures)
				}
				if thread.CancelCurrentTurn() {
					t.Fatal("failed request still has a live cancellation handle")
				}
			}
		})
	}
}

func TestRecoveredRequestCancellationDoesNotReportExecutorError(t *testing.T) {
	thread := newThread()
	thread.QueueItem(UserText("hello"))
	thread.QueueItem(SendItem{})
	thread.SetDelegate(ThreadDelegateFuncs{
		OnExecutorError: func(_ Thread, err error) {
			t.Errorf("unexpected executor error after cancellation: %v", err)
		},
	})
	if err := thread.AttachExecutorForRecovery(
		NewThreadExecutor(&recoveryFailureStreamer{failure: context.Canceled}),
	); err != nil {
		t.Fatalf("attach canceled request: %v", err)
	}
	if thread.State() != StateIdle {
		t.Fatalf("state after cancellation = %q, want idle", thread.State())
	}
}
