package anthropic

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	anthropicapi "github.com/anthropics/anthropic-sdk-go"

	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/streamertest"
)

func TestMessagesStreamerPreservesReasoningBlocks(t *testing.T) {
	streamer := NewMessagesStreamerWithClient(anthropicapi.Client{}, "")
	projected := threads.DefaultRequestBuilder.Build([]threads.Item{
		threads.ReasoningItem{Provider: "anthropic.messages", ID: "old"},
		threads.UserText("latest"),
		threads.ReasoningItem{Provider: "openai.responses", ID: "foreign"},
		threads.ReasoningItem{Provider: "anthropic.messages", ID: "current"},
	}, streamer.Capabilities())
	if want := []threads.Item{threads.UserText("latest"), threads.ReasoningItem{Provider: "anthropic.messages", ID: "current"}}; !reflect.DeepEqual(projected.Items, want) {
		t.Fatalf("reasoning projection\n got: %#v\nwant: %#v", projected.Items, want)
	}

	thinkingRaw := []byte(`{"signature":"signed","thinking":"brief rationale","type":"thinking"}`)
	redactedRaw := []byte(`{"data":"cipher","type":"redacted_thinking"}`)
	var got []threads.Item
	_, err := anthropicContractHarness{}.Stream(t, threads.Req{Items: []threads.Item{threads.UserText("question")}}, []streamertest.Event{
		{Item: threads.ReasoningItem{Opaque: thinkingRaw}},
		{Item: threads.ReasoningItem{Opaque: redactedRaw}},
		{Item: threads.ToolCall{CallID: "c1", Name: "lookup", Payload: `{}`}},
	}, func(item threads.Item) error {
		got = append(got, item)
		return nil
	})
	if err != nil {
		t.Fatalf("stream reasoning: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("emitted items = %#v", got)
	}
	thinking, ok := got[0].(threads.ReasoningItem)
	if !ok || thinking.Provider != "anthropic.messages" || thinking.Visibility != threads.ReasoningVisibilitySummary || thinking.Summary != "brief rationale" || !bytes.Equal(thinking.Opaque, []byte("signed")) {
		t.Fatalf("thinking item = %#v", got[0])
	}
	redacted, ok := got[1].(threads.ReasoningItem)
	if !ok || redacted.Provider != "anthropic.messages" || redacted.Visibility != threads.ReasoningVisibilityHidden || !bytes.Equal(redacted.Opaque, []byte("cipher")) {
		t.Fatalf("redacted item = %#v", got[1])
	}
	if _, ok := got[2].(threads.ToolCall); !ok {
		t.Fatalf("third item = %#v, want tool call", got[2])
	}

	messages, err := conversationMessages(threads.Req{Items: got})
	if err != nil {
		t.Fatalf("request messages: %v", err)
	}
	if len(messages) != 1 || len(messages[0].Content) != 3 {
		t.Fatalf("request messages = %#v", messages)
	}
	for i, want := range [][]byte{thinkingRaw, redactedRaw} {
		replayed, err := json.Marshal(messages[0].Content[i])
		if err != nil || !bytes.Equal(replayed, want) {
			t.Fatalf("replayed block %d = %s, %v; want %s", i, replayed, err, want)
		}
	}
	if messages[0].Content[2].OfToolUse == nil {
		t.Fatalf("reasoning was not replayed before tool call: %#v", messages[0].Content)
	}
	thread := threads.New()
	thread.QueueItem(thinking)
	snapshot, err := thread.Snapshot()
	if err != nil || len(snapshot.Items) != 1 || snapshot.Items[0].Summary != thinking.Summary || !bytes.Equal(snapshot.Items[0].Opaque, []byte("signed")) || bytes.Contains(snapshot.Items[0].Opaque, []byte(thinking.Summary)) {
		t.Fatalf("compacted thinking snapshot = %#v, err=%v", snapshot.Items, err)
	}
}

func TestRequestMessagesRejectsMalformedReasoning(t *testing.T) {
	for name, reasoning := range map[string]threads.ReasoningItem{
		"thinking without summary":   {Visibility: threads.ReasoningVisibilitySummary, Opaque: []byte("signature")},
		"thinking without signature": {Visibility: threads.ReasoningVisibilitySummary, Summary: "thinking"},
		"redacted without data":      {Visibility: threads.ReasoningVisibilityHidden},
		"unsupported visibility":     {Visibility: threads.ReasoningVisibilityText, Text: "thinking"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := conversationMessages(threads.Req{Items: []threads.Item{reasoning}}); err == nil {
				t.Fatal("malformed reasoning unexpectedly accepted")
			}
		})
	}
}
