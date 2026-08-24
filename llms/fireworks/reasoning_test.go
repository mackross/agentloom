package fireworks

import (
	"encoding/json"
	"reflect"
	"testing"

	openaiapi "github.com/openai/openai-go/v3"

	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/streamertest"
)

func TestChatCompletionsStreamerPreservesReasoning(t *testing.T) {
	streamer := NewChatCompletionsStreamerWithClient(openaiapi.Client{}, "")
	projected := threads.DefaultRequestBuilder.Build([]threads.Item{
		threads.ReasoningItem{Provider: "fireworks.chat", ID: "old"},
		threads.UserText("latest"),
		threads.ReasoningItem{Provider: "cerebras.chat", ID: "foreign"},
		threads.ReasoningItem{Provider: "fireworks.chat", ID: "current"},
	}, streamer.Capabilities())
	if want := []threads.Item{threads.UserText("latest"), threads.ReasoningItem{Provider: "fireworks.chat", ID: "current"}}; !reflect.DeepEqual(projected.Items, want) {
		t.Fatalf("reasoning projection\n got: %#v\nwant: %#v", projected.Items, want)
	}

	for _, terminal := range []threads.Item{
		threads.ToolCall{CallID: "c1", Name: "lookup", Payload: `{}`},
		threads.AssistantText("answer"),
	} {
		var got []threads.Item
		_, err := fireworksContractHarness{}.Stream(t, threads.Req{Items: []threads.Item{threads.UserText("question")}}, []streamertest.Event{
			{Item: threads.ReasoningItem{Text: "think "}},
			{Item: threads.ReasoningItem{Text: "hard"}},
			{Item: terminal},
		}, func(item threads.Item) error { got = append(got, item); return nil })
		if err != nil {
			t.Fatalf("stream reasoning: %v", err)
		}
		want := []threads.Item{threads.ReasoningItem{Provider: "fireworks.chat", Visibility: threads.ReasoningVisibilityText, Text: "think hard"}, terminal}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("emitted items\n got: %#v\nwant: %#v", got, want)
		}
	}

	messages, err := conversationMessages(threads.Req{Items: []threads.Item{
		threads.ReasoningItem{Provider: "fireworks.chat", Visibility: threads.ReasoningVisibilityText, Text: "think hard"},
		threads.ToolCall{CallID: "c1", Name: "lookup", Payload: `{}`},
	}})
	if err != nil {
		t.Fatalf("request messages: %v", err)
	}
	encoded, _ := json.Marshal(messages[0])
	var message map[string]any
	_ = json.Unmarshal(encoded, &message)
	if len(messages) != 1 || message["reasoning_content"] != "think hard" || message["content"] != nil || message["tool_calls"] == nil {
		t.Fatalf("assistant tool-call message = %s", encoded)
	}
	if _, err := conversationMessages(threads.Req{Items: []threads.Item{threads.ReasoningItem{Provider: "fireworks.chat", Text: "orphan"}}}); err == nil {
		t.Fatal("orphan reasoning unexpectedly accepted")
	}
}
