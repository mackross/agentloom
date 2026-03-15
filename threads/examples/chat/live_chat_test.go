//go:build live

package main

import (
	"os"
	"strings"
	"testing"

	fireworkswrap "github.com/mackross/agentloom/llms/fireworks"
	"github.com/mackross/agentloom/threads"
)

func TestLiveThreadsChatExampleWithOpenAIResponses(t *testing.T) {
	if os.Getenv("RUN_LIVE_API_TESTS") != "1" {
		t.Skip("set RUN_LIVE_API_TESTS=1 to run live API tests")
	}
	if strings.TrimSpace(os.Getenv("OPENAI_API_KEY")) == "" {
		t.Skip("OPENAI_API_KEY is not set")
	}

	runLiveChatExampleTest(t, "gpt-5.2", "live-example-openai-ok-42")
}

func TestLiveThreadsChatExampleWithAnthropicMessages(t *testing.T) {
	if os.Getenv("RUN_LIVE_API_TESTS") != "1" {
		t.Skip("set RUN_LIVE_API_TESTS=1 to run live API tests")
	}
	if strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")) == "" {
		t.Skip("ANTHROPIC_API_KEY is not set")
	}

	runLiveChatExampleTest(t, "claude-sonnet-4-6", "live-example-anthropic-ok-42")
}

func TestLiveThreadsChatExampleWithFireworksKimi25(t *testing.T) {
	if os.Getenv("RUN_LIVE_API_TESTS") != "1" {
		t.Skip("set RUN_LIVE_API_TESTS=1 to run live API tests")
	}
	if !hasProviderAPIKey(fireworkswrap.Kimi25Model) {
		t.Skip("FIREWORKS_API_KEY is not set")
	}

	runLiveChatExampleTest(t, fireworkswrap.Kimi25Model, "live-example-fireworks-ok-42")
}

func runLiveChatExampleTest(t testing.TB, model, token string) {
	t.Helper()

	streamer, resolvedModel := newStreamerForModel(model)
	if resolvedModel != model {
		t.Fatalf("unexpected resolved model: %q", resolvedModel)
	}

	thread := threads.New()
	thread.SetExecutor(threads.NewThreadExecutor(streamer))
	var out strings.Builder
	thread.SetDelegate(threads.ThreadDelegateFuncs{
		OnStreamItemAppended: func(_ *threads.Thread, item threads.Item) {
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
