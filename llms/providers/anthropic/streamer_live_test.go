//go:build live

package anthropic

import (
	"os"
	"strings"
	"testing"

	anthropicapi "github.com/anthropics/anthropic-sdk-go"
	gschema "github.com/google/jsonschema-go/jsonschema"

	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/streamerlivetest"
)

func TestLiveCapabilities(t *testing.T) {
	if strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")) == "" {
		t.Fatal("ANTHROPIC_API_KEY is not set")
	}

	model := strings.TrimSpace(os.Getenv("ANTHROPIC_MODEL"))
	if model == "" {
		model = string(DefaultModel)
	}

	reasoningStreamer := NewMessagesStreamer(model)
	reasoningStreamer.Thinking = anthropicapi.ThinkingConfigParamOfEnabled(2048)
	h := anthropicLiveHarness{
		SupportsReasoningToolLoop: streamerlivetest.SupportsReasoningToolLoop{Streamer: reasoningStreamer},
		// Anthropic normalizes complete result sets around intervening user text,
		// but rejects a partial set in that ordering.
		streamer: NewMessagesStreamer(model),
	}
	streamerlivetest.Run(t, h)
}

func TestLiveHistoricalToolResultsBeforeLaterUserText(t *testing.T) {
	if strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")) == "" {
		t.Fatal("ANTHROPIC_API_KEY is not set")
	}

	model := strings.TrimSpace(os.Getenv("ANTHROPIC_MODEL"))
	if model == "" {
		model = string(DefaultModel)
	}

	streamer := NewMessagesStreamer(model)
	err := streamer.StreamReq(threads.Req{
		Instruction: "Reply with only the word ok.",
		Items: []threads.Item{
			threads.UserText("Use this prior transcript as context."),
			threads.ToolCall{CallID: "toolu_hist_1", Name: "lookup", Payload: `{"query":"alpha"}`},
			threads.UserText("This user text appeared after the historical tool use."),
			threads.ToolCallResult{CallID: "toolu_hist_1", Output: `{"answer":"alpha"}`},
			threads.AssistantText("Recorded."),
			threads.UserText("Now continue."),
		},
		Tools: threads.ToolOfferSnapshot{Offered: []threads.ToolSpec{{
			Name:        "lookup",
			Description: "Lookup a value.",
			Payload: threads.ToolPayloadJSONSchema(gschema.Schema{Type: "object", Properties: map[string]*gschema.Schema{
				"query": {Type: "string"},
			}, Required: []string{"query"}}),
		}}},
	}, func(item threads.Item) error {
		t.Logf("historical follow-up received item: %T", item)
		return nil
	})
	if err != nil {
		t.Fatalf("historical transcript with normalizable tool result ordering failed: %v", err)
	}
}

type anthropicLiveHarness struct {
	streamerlivetest.SupportsToolCallChunking
	streamerlivetest.SupportsAssistantTextChunking
	streamerlivetest.SupportsParallelToolCalls
	streamerlivetest.SupportsReasoningToolLoop
	streamer *MessagesStreamer
}

func (h anthropicLiveHarness) Stream(t testing.TB, req threads.Req, emit func(threads.Item) error) error {
	t.Helper()
	return h.streamer.StreamReq(req, emit)
}
