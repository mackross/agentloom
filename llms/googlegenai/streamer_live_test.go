//go:build live

package googlegenai

import (
	"os"
	"strings"
	"testing"

	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/streamerlivetest"
	"google.golang.org/genai"
)

func TestLiveCapabilities(t *testing.T) {
	if !hasGoogleAPIKey() {
		t.Fatal("GEMINI_API_KEY and GOOGLE_API_KEY are not set")
	}

	model := strings.TrimSpace(os.Getenv("GOOGLE_GENAI_MODEL"))
	if model == "" {
		model = "gemini-3.1-flash-lite"
	}

	reasoningStreamer := NewGenerateContentStreamer(model)
	reasoningStreamer.Config.ThinkingConfig = &genai.ThinkingConfig{IncludeThoughts: true}
	h := googlegenaiLiveHarness{
		SupportsReasoningToolLoop: streamerlivetest.SupportsReasoningToolLoop{Streamer: reasoningStreamer},
		streamer:                  NewGenerateContentStreamer(model),
	}
	streamerlivetest.Run(t, h)
}

func hasGoogleAPIKey() bool {
	return strings.TrimSpace(os.Getenv("GEMINI_API_KEY")) != "" || strings.TrimSpace(os.Getenv("GOOGLE_API_KEY")) != ""
}

type googlegenaiLiveHarness struct {
	streamerlivetest.SupportsAssistantTextChunking
	streamerlivetest.SupportsParallelToolCalls
	streamerlivetest.SupportsReasoningToolLoop
	streamer *GenerateContentStreamer
}

func (h googlegenaiLiveHarness) Stream(t testing.TB, req threads.Req, emit func(threads.Item) error) error {
	t.Helper()
	return h.streamer.StreamReq(req, emit)
}
