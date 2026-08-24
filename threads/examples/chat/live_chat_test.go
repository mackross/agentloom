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
	if !hasProviderAPIKey(fireworkswrap.Kimi3Model) {
		t.Fatal("FIREWORKS_API_KEY is not set")
	}

	runLiveChatExampleTest(t, fireworkswrap.Kimi3Model, "live-example-fireworks-ok-42")
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
