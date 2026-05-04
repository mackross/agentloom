package openai

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	openaiapi "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/mackross/agentloom/threads"
)

func TestResponsesStreamerPreviousResponseIDRetriesFullRequestOnContinuationError(t *testing.T) {
	requests := make(chan []byte, 3)
	requestCount := 0
	var requestMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("websocket accept: %v", err)
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")

		for {
			_, body, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			requests <- append([]byte(nil), body...)
			requestMu.Lock()
			requestCount++
			count := requestCount
			requestMu.Unlock()
			raw := parseObservedRawRequest(t, body)

			if count == 1 {
				_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.created","response":{"id":"resp_1"}}`))
				_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.output_item.added","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"","status":"in_progress"}}`))
				_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.function_call_arguments.done","item_id":"fc_1","name":"lookup","arguments":"{}"}`))
				_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.output_item.done","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{}","status":"completed"}}`))
				_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_1"}}`))
				continue
			}

			if stringValue(raw["previous_response_id"]) != "" {
				_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"error","message":"bad previous_response_id"}`))
				continue
			}

			_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.created","response":{"id":"resp_2"}}`))
			_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.output_text.delta","delta":"ok","item_id":"msg_2"}`))
			_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_2"}}`))
		}
	}))
	defer server.Close()

	client := openaiapi.NewClient(
		option.WithAPIKey("test"),
		option.WithBaseURL(server.URL),
		option.WithHTTPClient(server.Client()),
		option.WithMaxRetries(0),
	)
	streamer := NewResponsesStreamerWithClient(client, "test-model")

	var first []threads.Item
	if err := streamer.StreamReq(threads.Req{Items: []threads.Item{threads.UserText("start")}}, func(item threads.Item) error {
		first = append(first, item)
		return nil
	}); err != nil {
		t.Fatalf("first request failed: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("first request emitted %d items, want 1", len(first))
	}
	call, ok := first[0].(threads.ToolCall)
	if !ok {
		t.Fatalf("first item = %T, want ToolCall", first[0])
	}

	var second []threads.Item
	err := streamer.StreamReq(threads.Req{Items: []threads.Item{
		threads.UserText("start"),
		call,
		threads.ToolCallResult{CallID: call.CallID, Output: "result"},
	}}, func(item threads.Item) error {
		second = append(second, item)
		return nil
	})
	if err != nil {
		t.Fatalf("second request failed after fallback: %v", err)
	}
	if len(second) != 1 || second[0] != threads.AssistantText("ok") {
		t.Fatalf("second emitted %#v, want assistant ok", second)
	}

	var bodies [][]byte
	for len(bodies) < 3 {
		select {
		case body := <-requests:
			bodies = append(bodies, body)
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for request %d", len(bodies)+1)
		}
	}
	firstReq := parseObservedRawRequest(t, bodies[0])
	if _, ok := firstReq["previous_response_id"]; ok {
		t.Fatalf("first request unexpectedly had previous_response_id: %s", bodies[0])
	}
	contReq := parseObservedRawRequest(t, bodies[1])
	if got := stringValue(contReq["previous_response_id"]); got != "resp_1" {
		t.Fatalf("continuation previous_response_id = %q, want resp_1; body=%s", got, bodies[1])
	}
	if got := len(objectSlice(t, contReq["input"])); got != 2 {
		t.Fatalf("continuation input len = %d, want delta len 2; body=%s", got, bodies[1])
	}
	fallbackReq := parseObservedRawRequest(t, bodies[2])
	if _, ok := fallbackReq["previous_response_id"]; ok {
		t.Fatalf("fallback request had previous_response_id: %s", bodies[2])
	}
	if got := len(objectSlice(t, fallbackReq["input"])); got != 3 {
		t.Fatalf("fallback input len = %d, want full len 3; body=%s", got, bodies[2])
	}
}

func parseObservedRawRequest(t testing.TB, body []byte) map[string]any {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	return raw
}
