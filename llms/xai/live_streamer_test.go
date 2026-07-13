//go:build live

package xai

import (
	"os"
	"strings"
	"testing"

	gschema "github.com/google/jsonschema-go/jsonschema"

	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/streamertest"
)

func requireLiveXAI(t *testing.T) string {
	t.Helper()
	if os.Getenv("RUN_LIVE_API_TESTS") != "1" {
		t.Skip("set RUN_LIVE_API_TESTS=1 to run live API tests")
	}
	if strings.TrimSpace(os.Getenv("XAI_API_KEY")) == "" {
		t.Skip("XAI_API_KEY is not set")
	}
	model := strings.TrimSpace(os.Getenv("XAI_MODEL"))
	if model == "" {
		model = DefaultModel
	}
	return model
}

// TestLiveResponsesStreamerCapabilities runs the shared streamertest suite.
// ParallelToolCalls is on (no Tools.Allowed). AllowedTools stays off: xAI does
// not accept OpenAI Responses tool_choice.allowed_tools.
func TestLiveResponsesStreamerCapabilities(t *testing.T) {
	model := requireLiveXAI(t)
	streamertest.RunLiveCapabilityTests(t, xaiSharedLiveHarness{
		streamer: NewResponsesStreamer(model),
	})
}

type xaiSharedLiveHarness struct {
	streamer *ResponsesStreamer
}

func (h xaiSharedLiveHarness) Capabilities() streamertest.Capabilities {
	// Public xAI docs say streaming function calls may arrive as a single frame;
	// streamertest SKIPs (does not fail) if multi-chunk tool args never appear.
	// Default streamer omits stream_tool_calls when previous_response_id is on
	// (xAI stores broken tool args otherwise), so multi-chunk tool args are
	// usually absent here; keep the capability flag so a future fix is probed.
	return streamertest.Capabilities{
		ToolCallChunks:      true,
		AssistantTextChunks: true,
		ParallelToolCalls:   true,
		AllowedTools:        false,
	}
}

func (h xaiSharedLiveHarness) Stream(t testing.TB, req threads.Req, emit func(threads.Item) error) error {
	t.Helper()
	return h.streamer.StreamReq(req, emit)
}

func TestLiveResponsesWithoutPreviousResponseID(t *testing.T) {
	model := requireLiveXAI(t)
	streamer := NewResponsesStreamer(model)
	streamer.DisablePreviousResponseID = true
	store := false
	streamer.Store = &store

	tools := echoToolOffer()
	items := []threads.Item{threads.UserText("Call echo exactly once with message hi, then stop. No other text.")}
	var got []threads.Item
	if err := streamer.StreamReq(threads.Req{
		Instruction: "You must use the echo tool.",
		Items:       items,
		Tools:       tools,
	}, func(item threads.Item) error {
		got = append(got, item)
		return nil
	}); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if streamer.LastUsedPreviousResponseID() {
		t.Fatal("turn 1 used previous_response_id")
	}
	call := firstToolCall(t, got)

	items = append(items, got...)
	items = append(items, threads.ToolCallResult{CallID: call.CallID, Output: "hi"})
	items = append(items, threads.UserText("After the tool result, reply with only: done"))

	got = nil
	if err := streamer.StreamReq(threads.Req{
		Instruction: "After tool results, answer briefly.",
		Items:       items,
		Tools:       tools,
	}, func(item threads.Item) error {
		got = append(got, item)
		return nil
	}); err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if streamer.LastUsedPreviousResponseID() {
		t.Fatal("turn 2 used previous_response_id; expected full replay")
	}
	if text := assistantText(got); !strings.Contains(strings.ToLower(text), "done") {
		t.Fatalf("turn 2 text = %q, want to contain done", text)
	}
}

func echoToolOffer() threads.ToolOfferSnapshot {
	return threads.ToolOfferSnapshot{
		Offered: []threads.ToolSpec{{
			Name:        "echo",
			Description: "Echo a message back.",
			Payload: threads.ToolPayloadJSONSchema(gschema.Schema{
				Type: "object",
				Properties: map[string]*gschema.Schema{
					"message": {Type: "string"},
				},
				Required: []string{"message"},
			}),
		}},
	}
}

func firstToolCall(t *testing.T, items []threads.Item) threads.ToolCall {
	t.Helper()
	for _, item := range items {
		if tc, ok := item.(threads.ToolCall); ok {
			return tc
		}
	}
	t.Fatalf("no tool call in %d items", len(items))
	return threads.ToolCall{}
}

func assistantText(items []threads.Item) string {
	var b strings.Builder
	for _, item := range items {
		if text, ok := item.(threads.AssistantText); ok {
			b.WriteString(string(text))
		}
	}
	return b.String()
}


