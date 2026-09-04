//go:build live

package googlegenai

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

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
		model = DefaultModel
	}

	reasoningStreamer := NewGenerateContentStreamer(model)
	reasoningStreamer.Config.ThinkingConfig = &genai.ThinkingConfig{
		IncludeThoughts: true,
		ThinkingLevel:   genai.ThinkingLevelMedium,
	}
	h := googlegenaiLiveHarness{
		SupportsReasoningToolLoop: streamerlivetest.SupportsReasoningToolLoop{
			Streamer:                   retryingGeminiLiveStreamer{streamer: reasoningStreamer},
			RetainReasoningAcrossTurns: isGemini38Flash(model),
		},
		streamer: NewGenerateContentStreamer(model),
	}
	streamerlivetest.Run(t, h)
}

func hasGoogleAPIKey() bool {
	return strings.TrimSpace(os.Getenv("GEMINI_API_KEY")) != "" || strings.TrimSpace(os.Getenv("GOOGLE_API_KEY")) != ""
}

type googlegenaiLiveHarness struct {
	streamerlivetest.SupportsAssistantTextChunking
	streamerlivetest.SupportsParallelToolCalls
	streamerlivetest.SupportsToolArguments
	streamerlivetest.SupportsReasoningToolLoop
	streamer *GenerateContentStreamer
}

func (h googlegenaiLiveHarness) Stream(t testing.TB, req threads.Req, emit func(threads.Item) error) error {
	t.Helper()
	return streamGeminiLiveWithRetry(h.streamer, req, emit)
}

type retryingGeminiLiveStreamer struct {
	streamer *GenerateContentStreamer
}

func (s retryingGeminiLiveStreamer) Capabilities() threads.StreamerCapabilities {
	return s.streamer.Capabilities()
}

func (s retryingGeminiLiveStreamer) RegisterToolNormalizer(name string, normalizer threads.ToolNormalizer) {
	s.streamer.RegisterToolNormalizer(name, normalizer)
}

func (s retryingGeminiLiveStreamer) UnregisterToolNormalizer(name string) {
	s.streamer.UnregisterToolNormalizer(name)
}

func (s retryingGeminiLiveStreamer) StreamReq(req threads.Req, emit func(threads.Item) error) error {
	return streamGeminiLiveWithRetry(s.streamer, req, emit)
}

func streamGeminiLiveWithRetry(streamer *GenerateContentStreamer, req threads.Req, emit func(threads.Item) error) error {
	const attempts = 5
	for attempt := 1; ; attempt++ {
		emitted := false
		err := streamer.StreamReq(req, func(item threads.Item) error {
			emitted = true
			return emit(item)
		})
		delay, retry := geminiLiveRetryDelay(err, attempt)
		if err == nil || emitted || attempt == attempts || !retry {
			return err
		}
		time.Sleep(delay)
	}
}

func geminiLiveRetryDelay(err error, attempt int) (time.Duration, bool) {
	var apiErr genai.APIError
	if !errors.As(err, &apiErr) {
		return 0, false
	}
	switch apiErr.Code {
	case 429:
		for _, detail := range apiErr.Details {
			raw, ok := detail["retryDelay"].(string)
			if !ok {
				continue
			}
			delay, err := time.ParseDuration(raw)
			if err == nil && delay <= time.Minute {
				return delay + time.Second, true
			}
		}
		return 0, false
	case 500, 502, 503, 504:
		return time.Duration(1<<(attempt-1)) * time.Second, true
	default:
		return 0, false
	}
}
