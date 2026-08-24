//go:build live

package openai

import (
	"os"
	"strings"
	"testing"

	gschema "github.com/google/jsonschema-go/jsonschema"

	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/streamerlivetest"
)

func TestLiveCapabilities(t *testing.T) {
	if strings.TrimSpace(os.Getenv("OPENAI_API_KEY")) == "" {
		t.Fatal("OPENAI_API_KEY is not set")
	}

	model := strings.TrimSpace(os.Getenv("OPENAI_MODEL"))
	if model == "" {
		model = "gpt-5.2"
	}

	h := openAILiveHarness{
		streamer: NewResponsesStreamer(model),
	}
	streamerlivetest.Run(t, h)
}

func TestLiveSendBeforeToolResolution(t *testing.T) {
	if strings.TrimSpace(os.Getenv("OPENAI_API_KEY")) == "" {
		t.Fatal("OPENAI_API_KEY is not set")
	}

	model := strings.TrimSpace(os.Getenv("OPENAI_MODEL"))
	if model == "" {
		model = "gpt-5.2"
	}

	streamer := NewResponsesStreamer(model)

	req1 := threads.Req{
		Instruction: "Call the calculator tool exactly once. Do not output any text.",
		Items:       []threads.Item{threads.UserText("Compute 2 + 2 using the calculator tool.")},
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

type openAILiveHarness struct {
	streamerlivetest.SupportsToolCallChunking
	streamerlivetest.SupportsAssistantTextChunking
	streamerlivetest.SupportsParallelToolCalls
	streamerlivetest.SupportsAllowedTools // OpenAI Responses tool_choice.allowed_tools
	streamer                              *ResponsesStreamer
}

func (h openAILiveHarness) Stream(t testing.TB, req threads.Req, emit func(threads.Item) error) error {
	t.Helper()
	return h.streamer.StreamReq(req, emit)
}
