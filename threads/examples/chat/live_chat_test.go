//go:build live

package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/mackross/agentloom/harness"
	fireworkswrap "github.com/mackross/agentloom/llms/providers/fireworks"
	"github.com/mackross/agentloom/threads"
)

func TestLiveThreadsChatExampleWithOpenAIResponses(t *testing.T) {
	if strings.TrimSpace(os.Getenv("OPENAI_API_KEY")) == "" {
		t.Fatal("OPENAI_API_KEY is not set")
	}

	runLiveChatExampleTest(t, "gpt-5.2", "live-example-openai-ok-42")
}

func TestLiveThreadsChatExampleWithAnthropicMessages(t *testing.T) {
	if strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")) == "" {
		t.Fatal("ANTHROPIC_API_KEY is not set")
	}

	runLiveChatExampleTest(t, "claude-sonnet-4-6", "live-example-anthropic-ok-42")
}

func TestLiveThreadsChatExampleWithFireworksKimi3(t *testing.T) {
	if strings.TrimSpace(os.Getenv("FIREWORKS_API_KEY")) == "" && strings.TrimSpace(os.Getenv("FIREWORKS_AI_API_KEY")) == "" {
		t.Fatal("FIREWORKS_API_KEY is not set")
	}

	runLiveChatExampleTest(t, fireworkswrap.Kimi3Model, "live-example-fireworks-ok-42")
}

func runLiveChatExampleTest(t testing.TB, model, token string) {
	t.Helper()

	h, err := harness.Open(context.Background(), harness.Options{Store: harness.Memory(harness.Settings{})})
	if err != nil {
		t.Fatal(err)
	}
	streamer, err := h.Streamer(context.Background(), model)
	if err != nil {
		t.Fatal(err)
	}

	thread := threads.New()
	thread.SetExecutor(threads.NewThreadExecutor(streamer))
	var out strings.Builder
	thread.SetDelegate(threads.ThreadDelegateFuncs{
		OnStreamItemAppended: func(_ threads.Thread, item threads.Item) {
			if text, ok := item.(threads.AssistantText); ok {
				out.WriteString(string(text))
			}
		},
	})
	thread.QueueItem(threads.AssistantInstruction("Reply with exactly: " + token))
	thread.QueueItem(threads.UserText("Confirm you can hear me."))
	thread.QueueItem(threads.SendItem{})

	got := strings.ToLower(strings.TrimSpace(out.String()))
	if got == "" {
		t.Fatal("expected non-empty streamed output")
	}
	if !strings.Contains(got, token) {
		t.Fatalf("expected output to contain %q, got %q", token, got)
	}
}
