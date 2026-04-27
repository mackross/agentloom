//go:build live

package openai

import (
	"os"
	"strings"
	"testing"

	gschema "github.com/google/jsonschema-go/jsonschema"

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

func TestLiveSendBeforeToolResolution(t *testing.T) {
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

	streamer := NewResponsesStreamer(model)

	req1 := threads.Req{
		Instruction: "Call the calculator tool exactly once. Do not output any text.",
		Items: []threads.Item{threads.UserText("Compute 2 + 2 using the calculator tool.")},
		Tools: threads.ToolOfferSnapshot{
			Offered: []threads.ToolSpec{{
				Name:        "calculator",
				Description: "Perform basic arithmetic calculations",
				Payload: threads.ToolPayloadJSONSchema(gschema.Schema{
					Type: "object",
					Properties: map[string]*gschema.Schema{
						"expression": {Type: "string", Pattern: "^[0-9+\\-*/ ]+$"},
					},
					Required: []string{"expression"},
				}),
			}},
		},
	}

	var req1Items, req2Items []threads.Item

	err := streamer.StreamReq(req1, func(item threads.Item) error {
		req1Items = append(req1Items, item)
		t.Logf("req1 received: %T", item)
		return nil
	})
	if err != nil {
		t.Fatalf("first request failed: %v", err)
	}

	t.Logf("request 1 got %d items", len(req1Items))
	for i, item := range req1Items {
		t.Logf("  item[%d]: %T", i, item)
	}

	hasToolCall := false
	var finalToolCall threads.ToolCall
	for _, item := range req1Items {
		if tc, ok := item.(threads.ToolCall); ok {
			hasToolCall = true
			finalToolCall = tc
			break
		}
	}
	if !hasToolCall {
		t.Skip("model did not make a tool call")
	}

	t.Logf("First request got tool call: %s(%s)", finalToolCall.Name, finalToolCall.CallID)

	req2WithResult := threads.Req{
		Items: []threads.Item{
			threads.UserText("What's the result?"),
			finalToolCall,
			threads.ToolCallResult{CallID: finalToolCall.CallID, Output: "4"},
		},
		Tools: threads.ToolOfferSnapshot{
			Offered: []threads.ToolSpec{{
				Name:        "calculator",
				Description: "Perform basic arithmetic calculations",
				Payload: threads.ToolPayloadJSONSchema(gschema.Schema{
					Type: "object",
					Properties: map[string]*gschema.Schema{
						"expression": {Type: "string", Pattern: "^[0-9+\\-*/ ]+$"},
					},
					Required: []string{"expression"},
				}),
			}},
		},
	}

	err = streamer.StreamReq(req2WithResult, func(item threads.Item) error {
		req2Items = append(req2Items, item)
		return nil
	})
	if err != nil {
		t.Fatalf("second request failed: %v", err)
	}

	t.Logf("request 2 got %d items", len(req2Items))
	for i, item := range req2Items {
		t.Logf("  item[%d]: %T", i, item)
	}

	var toolCalls, toolResults []threads.Item
	for _, item := range req2Items {
		switch item.(type) {
		case threads.ToolCall:
			toolCalls = append(toolCalls, item)
		case threads.ToolCallResult:
			toolResults = append(toolResults, item)
		}
	}

	t.Logf("tool calls in req2: %d, tool results in req2: %d", len(toolCalls), len(toolResults))

	if len(toolCalls) > 0 && len(toolResults) == 0 {
		t.Logf("BUG: Request sent with tool call but NO tool result!")
	}
}

func TestLivePartialToolResultWithoutUserText(t *testing.T) {
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

	parallel := true
	streamer := NewResponsesStreamer(model)
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
			threads.ToolCallResult{CallID: calls[0].CallID, Output: `{"ok":true}`},
		},
		Tools: tools,
	}
	err = streamer.StreamReq(partialReq, func(item threads.Item) error {
		t.Logf("partial follow-up received item: %T", item)
		return nil
	})
	if err == nil {
		t.Fatalf("partial follow-up unexpectedly succeeded with one result for two outstanding tool calls")
	}
	t.Logf("partial follow-up rejected as expected: %v", err)

	var completeItems []threads.Item
	completeReq := threads.Req{
		Items: []threads.Item{
			calls[0],
			calls[1],
			threads.ToolCallResult{CallID: calls[0].CallID, Output: `{"ok":true}`},
			threads.ToolCallResult{CallID: calls[1].CallID, Output: `{"ok":true}`},
		},
		Tools: tools,
	}
	err = streamer.StreamReq(completeReq, func(item threads.Item) error {
		completeItems = append(completeItems, item)
		return nil
	})
	if err != nil {
		t.Fatalf("complete follow-up with both tool results and no user text failed: %v", err)
	}

	t.Logf("complete follow-up with both tool results and no user text got %d items", len(completeItems))
	for i, item := range completeItems {
		t.Logf("  item[%d]: %T", i, item)
	}
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
