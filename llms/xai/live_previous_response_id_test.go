//go:build live

package xai

import (
	"strings"
	"testing"

	gschema "github.com/google/jsonschema-go/jsonschema"

	"github.com/mackross/agentloom/threads"
)

func TestLiveResponsesWithPreviousResponseID(t *testing.T) {
	model := requireLiveXAI(t)
	streamer := NewResponsesStreamer(model) // default: store=true, previous_response_id enabled

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

	const instruction = "You are testing tool continuation. If fewer than three number_lookup tool results are present, call number_lookup for the next missing step and output no text. If three results are present, answer with only: done 1 2 3"
	items := []threads.Item{threads.UserText("Use number_lookup exactly three times, one call per response, for steps one, two, and three in order. Do not answer until all three tool results are provided.")}

	// Three tool turns: each must produce a tool call; turns 2+ must use previous_response_id.
	for turn, output := range []string{"1", "2", "3"} {
		var got []threads.Item
		err := streamer.StreamReq(threads.Req{
			Instruction: instruction,
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
			if tc, ok := item.(threads.ToolCall); ok {
				call = tc
				break
			}
		}
		if call.CallID == "" {
			t.Fatalf("turn %d did not produce a tool call; got %d items: %#v", turn+1, len(got), got)
		}
		items = append(items, threads.ToolCallResult{CallID: call.CallID, Output: output})
	}

	// Final turn with all three tool results should answer "done" via previous_response_id.
	var got []threads.Item
	if err := streamer.StreamReq(threads.Req{
		Instruction: instruction,
		Items:       items,
		Tools:       tools,
	}, func(item threads.Item) error {
		got = append(got, item)
		return nil
	}); err != nil {
		t.Fatalf("final turn failed: %v", err)
	}
	if !streamer.LastUsedPreviousResponseID() {
		t.Fatal("final turn did not use previous_response_id")
	}
	if text := assistantText(got); !strings.Contains(text, "done") {
		t.Fatalf("final text = %q, want to contain done", text)
	}
}
