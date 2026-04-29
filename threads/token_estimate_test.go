package threads

import (
	"context"
	"testing"
)

func TestThreadEstimateRequestTokensUsesExecutorEstimator(t *testing.T) {
	streamer := &tokenCountingStreamer{requestTokens: 123}
	thread := New()
	thread.SetExecutor(NewThreadExecutor(streamer))
	thread.QueueItem(AssistantInstruction("be helpful"))
	thread.QueueItem(UserText("hello"))

	got, err := thread.EstimateRequestTokens(context.Background())
	if err != nil {
		t.Fatalf("EstimateRequestTokens: %v", err)
	}
	if got != 123 {
		t.Fatalf("EstimateRequestTokens = %d, want 123", got)
	}
	if streamer.req.Instruction != "be helpful" {
		t.Fatalf("estimator instruction = %q, want be helpful", streamer.req.Instruction)
	}
	if len(streamer.req.Items) != 1 || streamer.req.Items[0] != UserText("hello") {
		t.Fatalf("estimator items = %#v, want user text hello", streamer.req.Items)
	}
}

func TestThreadEstimateRequestTokensFallsBackToApproximation(t *testing.T) {
	thread := New()
	thread.QueueItem(AssistantInstruction("be helpful"))
	thread.QueueItem(UserText("hello world"))

	got, err := thread.EstimateRequestTokens(context.Background())
	if err != nil {
		t.Fatalf("EstimateRequestTokens: %v", err)
	}
	if got <= 0 {
		t.Fatalf("EstimateRequestTokens = %d, want positive fallback", got)
	}
}

func TestThreadEstimateTextTokensUsesExecutorEstimator(t *testing.T) {
	streamer := &tokenCountingStreamer{textTokens: 12}
	thread := New()
	thread.SetExecutor(NewThreadExecutor(streamer))

	got, err := thread.EstimateTextTokens(context.Background(), "hello")
	if err != nil {
		t.Fatalf("EstimateTextTokens: %v", err)
	}
	if got != 12 {
		t.Fatalf("EstimateTextTokens = %d, want 12", got)
	}
	if streamer.text != "hello" {
		t.Fatalf("estimator text = %q, want hello", streamer.text)
	}
}

func TestThreadEstimateTextTokensFallsBackToApproximation(t *testing.T) {
	thread := New()

	got, err := thread.EstimateTextTokens(context.Background(), "hello world")
	if err != nil {
		t.Fatalf("EstimateTextTokens: %v", err)
	}
	if got <= 0 {
		t.Fatalf("EstimateTextTokens = %d, want positive fallback", got)
	}
}

type tokenCountingStreamer struct {
	requestTokens int
	textTokens    int
	req           Req
	text          string
}

func (s *tokenCountingStreamer) Capabilities() StreamerCapabilities { return StreamerCapabilities{} }

func (s *tokenCountingStreamer) StreamReq(Req, func(Item) error) error { return nil }

func (s *tokenCountingStreamer) EstimateRequestTokens(_ context.Context, req Req) (int, error) {
	s.req = req
	return s.requestTokens, nil
}

func (s *tokenCountingStreamer) EstimateTextTokens(_ context.Context, text string) (int, error) {
	s.text = text
	return s.textTokens, nil
}
