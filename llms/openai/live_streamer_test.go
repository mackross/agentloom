//go:build live

package openai

import (
	"os"
	"strings"
	"testing"

	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/streamertest"
)

func TestLiveResponsesStreamerCapabilities(t *testing.T) {
	if os.Getenv("RUN_LIVE_API_TESTS") != "1" {
		t.Skip("set RUN_LIVE_API_TESTS=1 to run live API tests")
	}
	if strings.TrimSpace(os.Getenv("OPENAI_API_KEY")) == "" {
		t.Skip("OPENAI_API_KEY is not set")
	}

	model := strings.TrimSpace(os.Getenv("OPENAI_MODEL"))
	if model == "" {
		model = "gpt-5.2"
	}

	streamertest.RunLiveCapabilityTests(t, openAILiveHarness{
		streamer: NewResponsesStreamer(model),
	})
}

type openAILiveHarness struct {
	streamer *ResponsesStreamer
}

func (h openAILiveHarness) Capabilities() streamertest.Capabilities {
	return streamertest.Capabilities{
		ToolCallChunks:      true,
		AssistantTextChunks: true,
		ParallelToolCalls:   true,
	}
}

func (h openAILiveHarness) Stream(t testing.TB, req threads.Req, emit func(threads.Item) error) error {
	t.Helper()
	return h.streamer.StreamReq(req, emit)
}
