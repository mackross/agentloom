//go:build live

package fireworks

import (
	"os"
	"testing"

	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/streamertest"
)

func TestLiveChatCompletionsStreamerCapabilities(t *testing.T) {
	if os.Getenv("RUN_LIVE_API_TESTS") != "1" {
		t.Skip("set RUN_LIVE_API_TESTS=1 to run live API tests")
	}
	if fireworksAPIKey() == "" {
		t.Skip("FIREWORKS_API_KEY is not set")
	}

	streamertest.RunLiveCapabilityTests(t, fireworksLiveHarness{
		streamer: NewChatCompletionsStreamer(Kimi25Model),
	})
}

type fireworksLiveHarness struct {
	streamer *ChatCompletionsStreamer
}

func (fireworksLiveHarness) Capabilities() streamertest.Capabilities {
	return streamertest.Capabilities{
		ToolCallChunks:      true,
		AssistantTextChunks: true,
		ParallelToolCalls:   true,
	}
}

func (h fireworksLiveHarness) Stream(t testing.TB, req threads.Req, emit func(threads.Item) error) error {
	t.Helper()
	return h.streamer.StreamReq(req, emit)
}
