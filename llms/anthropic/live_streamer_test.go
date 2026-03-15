//go:build live

package anthropic

import (
	"os"
	"strings"
	"testing"

	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/streamertest"
)

func TestLiveMessagesStreamerCapabilities(t *testing.T) {
	if os.Getenv("RUN_LIVE_API_TESTS") != "1" {
		t.Skip("set RUN_LIVE_API_TESTS=1 to run live API tests")
	}
	if strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")) == "" {
		t.Skip("ANTHROPIC_API_KEY is not set")
	}

	model := strings.TrimSpace(os.Getenv("ANTHROPIC_MODEL"))
	if model == "" {
		model = string(DefaultModel)
	}

	streamertest.RunLiveCapabilityTests(t, anthropicLiveHarness{
		streamer: NewMessagesStreamer(model),
	})
}

type anthropicLiveHarness struct {
	streamer *MessagesStreamer
}

func (anthropicLiveHarness) Capabilities() streamertest.Capabilities {
	return streamertest.Capabilities{
		ToolCallChunks:      true,
		AssistantTextChunks: true,
		ParallelToolCalls:   true,
	}
}

func (h anthropicLiveHarness) Stream(t testing.TB, req threads.Req, emit func(threads.Item) error) error {
	t.Helper()
	return h.streamer.StreamReq(req, emit)
}
