//go:build live

package googlegenai

import (
	"os"
	"strings"
	"testing"

	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/streamertest"
)

func TestLiveGenerateContentStreamerCapabilities(t *testing.T) {
	if os.Getenv("RUN_LIVE_API_TESTS") != "1" {
		t.Skip("set RUN_LIVE_API_TESTS=1 to run live API tests")
	}
	if strings.TrimSpace(os.Getenv("GOOGLE_API_KEY")) == "" {
		t.Skip("GOOGLE_API_KEY is not set")
	}

	model := strings.TrimSpace(os.Getenv("GOOGLE_GENAI_MODEL"))
	if model == "" {
		model = "gemini-3.1-flash-lite"
	}

	streamertest.RunLiveCapabilityTests(t, googlegenaiLiveHarness{streamer: NewGenerateContentStreamer(model)})
}

type googlegenaiLiveHarness struct{ streamer *GenerateContentStreamer }

func (h googlegenaiLiveHarness) Capabilities() streamertest.Capabilities {
	return streamertest.Capabilities{AssistantTextChunks: true, ParallelToolCalls: true}
}

func (h googlegenaiLiveHarness) Stream(t testing.TB, req threads.Req, emit func(threads.Item) error) error {
	t.Helper()
	return h.streamer.StreamReq(req, emit)
}
