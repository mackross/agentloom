package googlegenai

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/streamertest"
)

type signedGeminiPart struct {
	item      threads.Item
	thought   bool
	signature []byte
}

func (signedGeminiPart) Emit() bool { return false }

func TestGenerateContentStreamerPreservesThoughtSignatures(t *testing.T) {
	streamer := &GenerateContentStreamer{}
	projected := threads.DefaultRequestBuilder.Build([]threads.Item{
		threads.ReasoningItem{Provider: "google.gemini", ID: "old"},
		threads.UserText("latest"),
		threads.ReasoningItem{Provider: "anthropic.messages", ID: "foreign"},
		threads.ReasoningItem{Provider: "google.gemini", ID: "current"},
	}, streamer.Capabilities())
	if want := []threads.Item{threads.UserText("latest"), threads.ReasoningItem{Provider: "google.gemini", ID: "current"}}; !reflect.DeepEqual(projected.Items, want) {
		t.Fatalf("reasoning projection\n got: %#v\nwant: %#v", projected.Items, want)
	}

	thoughtSig, firstSig, secondSig, textSig := []byte("thought"), []byte("first"), []byte("second"), []byte("text")
	var got []threads.Item
	_, err := googlegenaiContractHarness{}.Stream(t, threads.Req{Items: []threads.Item{threads.UserText("question")}}, []streamertest.Event{
		{Item: signedGeminiPart{item: threads.AssistantText("private thought"), thought: true, signature: thoughtSig}},
		{Item: signedGeminiPart{item: threads.ToolCall{CallID: "c1", Name: "first", Payload: `{}`}, signature: firstSig}},
		{Item: signedGeminiPart{item: threads.ToolCall{CallID: "c2", Name: "second", Payload: `{}`}, signature: secondSig}},
		{Item: signedGeminiPart{item: threads.AssistantText("answer"), signature: textSig}},
	}, func(item threads.Item) error {
		got = append(got, item)
		return nil
	})
	if err != nil {
		t.Fatalf("stream signed parts: %v", err)
	}
	want := []threads.Item{
		threads.ReasoningItem{Provider: "google.gemini", Visibility: threads.ReasoningVisibilitySummary, Summary: "private thought", Opaque: thoughtSig},
		threads.ReasoningItem{Provider: "google.gemini", Visibility: threads.ReasoningVisibilityHidden, Opaque: firstSig},
		threads.ToolCall{CallID: "c1", Name: "first", Payload: `{}`},
		threads.ReasoningItem{Provider: "google.gemini", Visibility: threads.ReasoningVisibilityHidden, Opaque: secondSig},
		threads.ToolCall{CallID: "c2", Name: "second", Payload: `{}`},
		threads.ReasoningItem{Provider: "google.gemini", Visibility: threads.ReasoningVisibilityHidden, Opaque: textSig},
		threads.AssistantText("answer"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("emitted items\n got: %#v\nwant: %#v", got, want)
	}

	contents, err := conversationContents(threads.Req{Items: got})
	if err != nil {
		t.Fatalf("request contents: %v", err)
	}
	if len(contents) != 1 || len(contents[0].Parts) != 4 {
		t.Fatalf("request contents = %#v", contents)
	}
	parts := contents[0].Parts
	if !parts[0].Thought || parts[0].Text != "private thought" || !bytes.Equal(parts[0].ThoughtSignature, thoughtSig) {
		t.Fatalf("thought part = %#v", parts[0])
	}
	for i, signature := range [][]byte{firstSig, secondSig} {
		if parts[i+1].FunctionCall == nil || !bytes.Equal(parts[i+1].ThoughtSignature, signature) {
			t.Fatalf("signed call part %d = %#v", i, parts[i+1])
		}
	}
	if parts[3].Text != "answer" || !bytes.Equal(parts[3].ThoughtSignature, textSig) {
		t.Fatalf("signed text part = %#v", parts[3])
	}
}

func TestGenerateContentStreamerPreservesTrailingTextSignature(t *testing.T) {
	signature := []byte("trailing")
	var got []threads.Item
	_, err := googlegenaiContractHarness{}.Stream(t, threads.Req{Items: []threads.Item{threads.UserText("question")}}, []streamertest.Event{
		{Item: threads.AssistantText("answer ")},
		{Item: threads.AssistantText("complete")},
		{Item: signedGeminiPart{signature: signature}},
	}, func(item threads.Item) error { got = append(got, item); return nil })
	if err != nil {
		t.Fatalf("stream trailing signature: %v", err)
	}
	want := []threads.Item{
		threads.AssistantText("answer "), threads.AssistantText("complete"),
		threads.ReasoningItem{Provider: "google.gemini", Visibility: threads.ReasoningVisibilityHidden, Opaque: signature},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("emitted items\n got: %#v\nwant: %#v", got, want)
	}
	contents, err := conversationContents(threads.Req{Items: got})
	if err != nil {
		t.Fatalf("request contents: %v", err)
	}
	parts := contents[0].Parts
	if len(parts) != 2 || len(parts[0].ThoughtSignature) != 0 || !bytes.Equal(parts[1].ThoughtSignature, signature) {
		t.Fatalf("trailing signature replay = %#v", parts)
	}
	if _, err := (googlegenaiContractHarness{}).Stream(t, threads.Req{}, []streamertest.Event{{Item: signedGeminiPart{signature: signature}}}, func(threads.Item) error { return nil }); err == nil {
		t.Fatal("standalone signature-only part unexpectedly accepted")
	}
}
