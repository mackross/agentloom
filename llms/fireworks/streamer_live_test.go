//go:build live

package fireworks

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	gschema "github.com/google/jsonschema-go/jsonschema"

	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/streamerlivetest"
)

func TestLiveCapabilities(t *testing.T) {
	if fireworksAPIKey() == "" {
		t.Fatal("FIREWORKS_API_KEY is not set")
	}

	reasoningModel := strings.TrimSpace(os.Getenv("FIREWORKS_MODEL"))
	if reasoningModel == "" {
		reasoningModel = DeepSeekV4FlashModel
	}
	h := fireworksLiveHarness{
		SupportsReasoningToolLoop: streamerlivetest.SupportsReasoningToolLoop{
			Streamer: NewChatCompletionsStreamer(reasoningModel),
		},
		streamer: NewChatCompletionsStreamer(Kimi3Model),
	}
	streamerlivetest.Run(t, h)
}

func TestLiveServerlessFunctionCallingModels(t *testing.T) {
	if fireworksAPIKey() == "" {
		t.Fatal("FIREWORKS_API_KEY is not set")
	}

	tests := []struct {
		name  string
		model string
	}{
		{name: "deepseek-v4-pro", model: DeepSeekV4ProModel},
		{name: "deepseek-v4-flash", model: DeepSeekV4FlashModel},
		{name: "kimi-k3", model: Kimi3Model},
		{name: "minimax-m2p7", model: MiniMaxM27Model},
		{name: "gpt-oss-120b", model: GPTOSS120BModel},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			token := any("fireworks-function-calling-live-test")
			streamer := NewChatCompletionsStreamer(tt.model)
			var calls []threads.ToolCall
			err := streamer.StreamReq(threads.Req{
				Instruction: "You must call the selected tool exactly once. Do not write any normal text.",
				Items: []threads.Item{threads.UserText(
					`Call the tool named record_probe with payload {"token":"fireworks-function-calling-live-test"}.`,
				)},
				Tools: threads.ToolOfferSnapshot{
					Offered: []threads.ToolSpec{{
						Name:        "record_probe",
						Description: "Records the provided probe token.",
						Payload: threads.ToolPayloadJSONSchema(gschema.Schema{
							Type: "object",
							Properties: map[string]*gschema.Schema{
								"token": {Type: "string", Const: &token},
							},
							Required: []string{"token"},
						}),
					}},
					Allowed:  []string{"record_probe"},
					Required: true,
				},
			}, func(item threads.Item) error {
				if call, ok := item.(threads.ToolCall); ok {
					calls = append(calls, call)
				}
				return nil
			})
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			if len(calls) != 1 {
				t.Fatalf("got %d tool calls, want 1", len(calls))
			}
			if calls[0].Name != "record_probe" {
				t.Fatalf("tool name = %q, want record_probe", calls[0].Name)
			}
			var payload struct {
				Token string `json:"token"`
			}
			if err := json.Unmarshal([]byte(calls[0].Payload), &payload); err != nil {
				t.Fatalf("tool payload is not JSON: %v; payload=%q", err, calls[0].Payload)
			}
			if payload.Token != "fireworks-function-calling-live-test" {
				t.Fatalf("token = %q, want fireworks-function-calling-live-test; payload=%q", payload.Token, calls[0].Payload)
			}
		})
	}
}

type fireworksLiveHarness struct {
	streamerlivetest.SupportsToolCallChunking
	streamerlivetest.SupportsAssistantTextChunking
	streamerlivetest.SupportsParallelToolCalls
	streamerlivetest.SupportsReasoningToolLoop
	streamer *ChatCompletionsStreamer
}

func (h fireworksLiveHarness) Stream(t testing.TB, req threads.Req, emit func(threads.Item) error) error {
	t.Helper()
	return h.streamer.StreamReq(req, emit)
}
