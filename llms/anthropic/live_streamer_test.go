//go:build live

package anthropic

import (
	"os"
	"strings"
	"testing"

	gschema "github.com/google/jsonschema-go/jsonschema"

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

func TestLivePartialToolResultWithInterveningUserText(t *testing.T) {
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

	parallel := true
	streamer := NewMessagesStreamer(model)
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
	if err == nil {
		t.Fatalf("partial follow-up unexpectedly succeeded with one result for two outstanding tool calls")
	}
	t.Logf("partial follow-up rejected as expected: %v", err)

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
		t.Fatalf("complete follow-up with normalizable intervening user text failed: %v", err)
	}
	t.Logf("complete follow-up with normalizable intervening user text accepted as expected")

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

func TestLiveHistoricalToolResultsBeforeLaterUserText(t *testing.T) {
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
