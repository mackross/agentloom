package deepseek

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	openaiapi "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	openaiadapter "github.com/mackross/agentloom/llms/openai"
	"github.com/mackross/agentloom/threads"
)

func TestResponsesStreamerReplaysCurrentReasoningAndDropsPriorTurnReasoning(t *testing.T) {
	if BaseURL != "https://api.deepseek.com" || DefaultModel != "deepseek-v4-flash" {
		t.Fatalf("unexpected defaults: base=%q model=%q", BaseURL, DefaultModel)
	}

	requests := make(chan []byte, 2)
	var count atomic.Int32
	toolReasoning := []byte(`{"id":"rs_tool","type":"reasoning","content":[{"type":"reasoning_text","text":"use the tool"}],"summary":[],"status":"completed"}`)
	finalReasoning := []byte(`{"id":"rs_final","type":"reasoning","content":[{"type":"reasoning_text","text":"compose answer"}],"summary":[],"status":"completed"}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- body
		w.Header().Set("Content-Type", "text/event-stream")
		if count.Add(1) == 1 {
			writeSSE(w,
				`{"type":"response.created","response":{"id":"resp_1"}}`,
				`{"type":"response.output_item.done","item":`+string(toolReasoning)+`}`,
				`{"type":"response.output_item.added","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"","status":"in_progress"}}`,
				`{"type":"response.function_call_arguments.done","item_id":"fc_1","name":"lookup","arguments":"{}"}`,
				`{"type":"response.output_item.done","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{}","status":"completed"}}`,
				`{"type":"response.completed","response":{"id":"resp_1"}}`)
			return
		}
		writeSSE(w,
			`{"type":"response.created","response":{"id":"resp_2"}}`,
			`{"type":"response.output_item.done","item":`+string(finalReasoning)+`}`,
			`{"type":"response.output_text.delta","delta":"answer","item_id":"msg_2"}`,
			`{"type":"response.completed","response":{"id":"resp_2"}}`)
	}))
	defer server.Close()

	client := openaiapi.NewClient(option.WithAPIKey("test"), option.WithBaseURL(server.URL), option.WithHTTPClient(server.Client()), option.WithMaxRetries(0))
	streamer := NewResponsesStreamerWithClient(client, "")
	if streamer.Transport != openaiadapter.ResponsesTransportSSE || !streamer.DisablePreviousResponseID {
		t.Fatalf("unexpected transport/continuation: %#v", streamer)
	}

	items := []threads.Item{threads.UserText("question")}
	var first []threads.Item
	if err := streamer.StreamReq(threads.DefaultRequestBuilder.Build(items, streamer.Capabilities()), func(item threads.Item) error { first = append(first, item); return nil }); err != nil {
		t.Fatalf("first request: %v", err)
	}
	if len(first) != 2 {
		t.Fatalf("first items = %#v", first)
	}
	reasoning, ok := first[0].(threads.ReasoningItem)
	call, callOK := first[1].(threads.ToolCall)
	if !ok || reasoning.Provider != "deepseek.responses" || reasoning.ID != "rs_tool" || reasoning.Visibility != threads.ReasoningVisibilityText || reasoning.Text != "use the tool" || len(reasoning.Opaque) != 0 || !callOK {
		t.Fatalf("first items = %#v", first)
	}
	thread := threads.New()
	thread.QueueItem(reasoning)
	snapshot, err := thread.Snapshot()
	if err != nil || len(snapshot.Items) != 1 || len(snapshot.Items[0].Opaque) != 0 {
		t.Fatalf("compacted reasoning snapshot = %#v, err=%v", snapshot.Items, err)
	}
	items = append(items, first...)
	items = append(items, threads.ToolCallResult{CallID: call.CallID, Output: "found"})

	var second []threads.Item
	if err := streamer.StreamReq(threads.DefaultRequestBuilder.Build(items, streamer.Capabilities()), func(item threads.Item) error { second = append(second, item); return nil }); err != nil {
		t.Fatalf("second request: %v", err)
	}
	firstBody, secondBody := <-requests, <-requests
	var firstJSON, secondJSON map[string]any
	_ = json.Unmarshal(firstBody, &firstJSON)
	_ = json.Unmarshal(secondBody, &secondJSON)
	if firstJSON["store"] != false || firstJSON["previous_response_id"] != nil || firstJSON["include"] != nil {
		t.Fatalf("stateful fields in first request: %s", firstBody)
	}
	input := secondJSON["input"].([]any)
	wantReplayed := map[string]any{
		"id": "rs_tool", "type": "reasoning",
		"content": []any{map[string]any{"type": "reasoning_text", "text": "use the tool"}},
	}
	if !reflect.DeepEqual(input[1], wantReplayed) {
		t.Fatalf("replayed reasoning = %#v, want %#v", input[1], wantReplayed)
	}

	items = append(items, second...)
	items = append(items, threads.UserText("next question"))
	projected := threads.DefaultRequestBuilder.Build(items, streamer.Capabilities()).Items
	var reasoningIDs []string
	for _, item := range projected {
		if item, ok := item.(threads.ReasoningItem); ok {
			reasoningIDs = append(reasoningIDs, item.ID)
		}
	}
	if len(reasoningIDs) != 0 {
		t.Fatalf("projected reasoning IDs = %#v, want none from prior turn", reasoningIDs)
	}
}

func TestResponsesStreamerRejectsIncompleteReasoningInput(t *testing.T) {
	streamer := NewResponsesStreamerWithClient(openaiapi.Client{}, "")
	for name, reasoning := range map[string]threads.ReasoningItem{
		"missing id":   {Provider: reasoningProvider, Text: "reason"},
		"missing text": {Provider: reasoningProvider, ID: "rs_1"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := streamer.StreamReq(threads.Req{Items: []threads.Item{reasoning}}, func(threads.Item) error { return nil }); err == nil {
				t.Fatal("incomplete reasoning unexpectedly accepted")
			}
		})
	}
}

func writeSSE(w io.Writer, events ...string) {
	for _, event := range events {
		_, _ = io.WriteString(w, "data: "+event+"\n\n")
	}
}
