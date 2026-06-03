//go:build live

package fireworks

import (
	"encoding/json"
	"os"
	"testing"

	gschema "github.com/google/jsonschema-go/jsonschema"

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

func TestLivePartialToolResultWithInterveningUserText(t *testing.T) {
	if os.Getenv("RUN_LIVE_API_TESTS") != "1" {
		t.Skip("set RUN_LIVE_API_TESTS=1 to run live API tests")
	}
	if fireworksAPIKey() == "" {
		t.Skip("FIREWORKS_API_KEY is not set")
	}

	parallel := true
	streamer := NewChatCompletionsStreamer(Kimi25Model)
	tokenTool := func(name, token string) threads.ToolSpec {
		return threads.ToolSpec{
			Name:        name,
			Description: "Records the " + token + " token.",
			Payload: threads.ToolPayloadJSONSchema(gschema.Schema{
				Type: "object",
				Properties: map[string]*gschema.Schema{
					"token": {Type: "string", Pattern: "^" + token + "$"},
				},
				Required: []string{"token"},
			}),
		}
	}
	tools := threads.ToolOfferSnapshot{
		Offered: []threads.ToolSpec{
			tokenTool("alpha_once", "alpha"),
			tokenTool("beta_once", "beta"),
		},
		Allowed:  []string{"alpha_once", "beta_once"},
		Parallel: &parallel,
	}

	var firstItems []threads.Item
	err := streamer.StreamReq(threads.Req{
		Instruction: "Call both tools exactly once in the same response. Do not output any text. Do not wait for tool results.",
		Items: []threads.Item{threads.UserText(
			"Call tool alpha_once with token alpha and tool beta_once with token beta.",
		)},
		Tools: tools,
	}, func(item threads.Item) error {
		firstItems = append(firstItems, item)
		return nil
	})
	if err != nil {
		t.Fatalf("first request failed: %v", err)
	}

	var calls []threads.ToolCall
	for _, item := range firstItems {
		if call, ok := item.(threads.ToolCall); ok {
			calls = append(calls, call)
		}
	}
	if len(calls) != 2 {
		t.Skipf("model did not make exactly two tool calls; got %d", len(calls))
	}

	t.Logf("first request got tool calls: %s(%s), %s(%s)", calls[0].Name, calls[0].CallID, calls[1].Name, calls[1].CallID)

	partialReq := threads.Req{
		Items: []threads.Item{
			calls[0],
			calls[1],
			threads.UserText("While the second tool is still pending, can you continue with the information you have?"),
			threads.ToolCallResult{CallID: calls[0].CallID, Output: `{"ok":true}`},
		},
		Tools: tools,
	}
	err = streamer.StreamReq(partialReq, func(item threads.Item) error {
		t.Logf("partial follow-up received item: %T", item)
		return nil
	})
	if err != nil {
		t.Fatalf("partial follow-up with intervening user text failed: %v", err)
	}
	t.Logf("partial follow-up accepted as expected")

	completeButInterveningReq := threads.Req{
		Items: []threads.Item{
			calls[0],
			calls[1],
			threads.UserText("While the second tool is still pending, can you continue with the information you have?"),
			threads.ToolCallResult{CallID: calls[0].CallID, Output: `{"ok":true}`},
			threads.ToolCallResult{CallID: calls[1].CallID, Output: `{"ok":true}`},
		},
		Tools: tools,
	}
	err = streamer.StreamReq(completeButInterveningReq, func(item threads.Item) error {
		t.Logf("complete-but-intervening follow-up received item: %T", item)
		return nil
	})
	if err != nil {
		t.Fatalf("complete follow-up with intervening user text failed: %v", err)
	}
	t.Logf("complete follow-up with intervening user text accepted as expected")

	var completeItems []threads.Item
	completeReq := threads.Req{
		Items: []threads.Item{
			calls[0],
			calls[1],
			threads.ToolCallResult{CallID: calls[0].CallID, Output: `{"ok":true}`},
			threads.ToolCallResult{CallID: calls[1].CallID, Output: `{"ok":true}`},
			threads.UserText("Now that both tools are complete, can you continue?"),
		},
		Tools: tools,
	}
	err = streamer.StreamReq(completeReq, func(item threads.Item) error {
		completeItems = append(completeItems, item)
		return nil
	})
	if err != nil {
		t.Fatalf("complete follow-up with both tool results before user text failed: %v", err)
	}

	t.Logf("complete follow-up with both tool results before user text got %d items", len(completeItems))
	for i, item := range completeItems {
		t.Logf("  item[%d]: %T", i, item)
	}
}

func TestLiveServerlessFunctionCallingModels(t *testing.T) {
	if os.Getenv("RUN_LIVE_API_TESTS") != "1" {
		t.Skip("set RUN_LIVE_API_TESTS=1 to run live API tests")
	}
	if fireworksAPIKey() == "" {
		t.Skip("FIREWORKS_API_KEY is not set")
	}

	tests := []struct {
		name  string
		model string
	}{
		{name: "deepseek-v4-pro", model: DeepSeekV4ProModel},
		{name: "deepseek-v4-flash", model: DeepSeekV4FlashModel},
		{name: "kimi-k2p6", model: Kimi26Model},
		{name: "kimi-k2p5", model: Kimi25Model},
		{name: "minimax-m2p7", model: MiniMaxM27Model},
		{name: "minimax-m2p5", model: MiniMaxM25Model},
		{name: "qwen3p6-plus", model: Qwen36PlusModel},
		{name: "glm-5p1", model: GLM51Model},
		{name: "gpt-oss-120b", model: GPTOSS120BModel},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
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
								"token": {Type: "string", Const: "fireworks-function-calling-live-test"},
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
