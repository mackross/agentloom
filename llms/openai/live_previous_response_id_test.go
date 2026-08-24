//go:build live

package openai

import (
	"os"
	"strings"
	"testing"

	gschema "github.com/google/jsonschema-go/jsonschema"
	"github.com/openai/openai-go/v3/shared"

	"github.com/mackross/agentloom/threads"
)

func TestLiveResponsesStreamerUsesPreviousResponseIDAcrossThreeToolTurns(t *testing.T) {
	if strings.TrimSpace(os.Getenv("OPENAI_API_KEY")) == "" {
		t.Fatal("OPENAI_API_KEY is not set")
	}

	model := strings.TrimSpace(os.Getenv("OPENAI_MODEL"))
	if model == "" {
		model = "gpt-5.2"
	}

	streamer := NewResponsesStreamer(model)
	streamer.Reasoning = shared.ReasoningParam{Effort: shared.ReasoningEffortMedium, Summary: shared.ReasoningSummaryAuto}
	tools := threads.ToolOfferSnapshot{
		Offered: []threads.ToolSpec{{
			Name:        "number_lookup",
			Description: "Returns the requested number token.",
			Payload: threads.ToolPayloadJSONSchema(gschema.Schema{
				Type: "object",
				Properties: map[string]*gschema.Schema{
					"step": {Type: "string", Pattern: "^(one|two|three)$"},
				},
				Required: []string{"step"},
			}),
		}},
	}

	items := []threads.Item{threads.UserText("Use number_lookup exactly three times, one call per response, for steps one, two, and three in order. Do not answer until all three tool results are provided.")}
	sawReasoning := false
	for turn, output := range []string{"1", "2", "3"} {
		var got []threads.Item
		err := streamer.StreamReq(threads.Req{
			Instruction: "You are testing tool continuation. If fewer than three number_lookup tool results are present, call number_lookup for the next missing step and output no text. If three results are present, answer with only: done 1 2 3",
			Items:       items,
			Tools:       tools,
		}, func(item threads.Item) error {
			got = append(got, item)
			return nil
		})
		if err != nil {
			t.Fatalf("turn %d failed: %v", turn+1, err)
		}
		if turn > 0 && !streamer.LastUsedPreviousResponseID() {
			t.Fatalf("turn %d did not use previous_response_id", turn+1)
		}

		items = append(items, got...)
		var call threads.ToolCall
		for _, item := range got {
			if _, ok := item.(threads.ReasoningItem); ok {
				sawReasoning = true
			}
			if tc, ok := item.(threads.ToolCall); ok {
				call = tc
				break
			}
		}
		if call.CallID == "" {
			t.Fatalf("turn %d did not produce a tool call; got %d items", turn+1, len(got))
		}
		items = append(items, threads.ToolCallResult{CallID: call.CallID, Output: output})
	}

	var final []threads.Item
	if err := streamer.StreamReq(threads.Req{Instruction: "Answer only: done 1 2 3", Items: items, Tools: tools}, func(item threads.Item) error {
		final = append(final, item)
		return nil
	}); err != nil {
		t.Fatalf("final turn failed: %v", err)
	}
	if !sawReasoning {
		t.Fatal("tool loop emitted no reasoning item")
	}
	items = append(items, final...)
	items = append(items, threads.UserText("Reply only: acknowledged"))
	if err := streamer.StreamReq(threads.Req{Items: items}, func(threads.Item) error { return nil }); err != nil {
		t.Fatalf("new user turn after reasoning replay failed: %v", err)
	}
}
