package xai

import (
	"bytes"
	"encoding/json"
	"testing"

	openaiapi "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"

	"github.com/mackross/agentloom/threads"
)

func TestResponsesStreamerPreservesReasoningItems(t *testing.T) {
	streamer := NewResponsesStreamerWithClient(openaiapi.Client{}, "")
	caps := streamer.Capabilities()
	projected := threads.DefaultRequestBuilder.Build([]threads.Item{
		threads.ReasoningItem{Provider: "xai.responses"},
		threads.ReasoningItem{Provider: "openai.responses"},
	}, caps)
	if len(projected.Items) != 1 || projected.Items[0].(threads.ReasoningItem).Provider != "xai.responses" {
		t.Fatalf("reasoning projection = %#v", projected.Items)
	}

	raw := []byte(`{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"brief rationale"}],"encrypted_content":"cipher","status":"completed"}`)
	events := reasoningEvents(t,
		`{"type":"response.output_item.done","item":`+string(raw)+`}`,
		`{"type":"response.output_text.delta","delta":"answer","item_id":"msg_1"}`,
	)
	var got []threads.Item
	if _, _, err := streamer.streamResponseItems(&reasoningResponseStream{events: events, at: -1}, func(item threads.Item) error {
		got = append(got, item)
		return nil
	}); err != nil {
		t.Fatalf("consume response: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("emitted items = %#v, want reasoning then text", got)
	}
	reasoning, ok := got[0].(threads.ReasoningItem)
	if !ok || reasoning.Provider != "xai.responses" || reasoning.ID != "rs_1" || reasoning.Visibility != threads.ReasoningVisibilitySummary || reasoning.Summary != "brief rationale" || !bytes.Equal(reasoning.Opaque, raw) {
		t.Fatalf("reasoning item = %#v", got[0])
	}
	if got[1] != threads.AssistantText("answer") {
		t.Fatalf("second item = %#v, want assistant answer", got[1])
	}

	input, err := conversationInput(threads.Req{Items: []threads.Item{reasoning}})
	if err != nil {
		t.Fatalf("request reasoning: %v", err)
	}
	replayed, err := json.Marshal(input[0])
	if err != nil || !bytes.Equal(replayed, raw) {
		t.Fatalf("replayed reasoning = %s, %v; want %s", replayed, err, raw)
	}
}

type reasoningResponseStream struct {
	events []responses.ResponseStreamEventUnion
	at     int
}

func (s *reasoningResponseStream) Next() bool                                  { s.at++; return s.at < len(s.events) }
func (s *reasoningResponseStream) Current() responses.ResponseStreamEventUnion { return s.events[s.at] }
func (*reasoningResponseStream) Err() error                                    { return nil }
func (*reasoningResponseStream) Close() error                                  { return nil }

func reasoningEvents(t *testing.T, raws ...string) []responses.ResponseStreamEventUnion {
	t.Helper()
	events := make([]responses.ResponseStreamEventUnion, len(raws))
	for i, raw := range raws {
		if err := json.Unmarshal([]byte(raw), &events[i]); err != nil {
			t.Fatalf("decode event: %v", err)
		}
	}
	return events
}
