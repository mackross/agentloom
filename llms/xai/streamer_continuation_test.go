package xai

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	gschema "github.com/google/jsonschema-go/jsonschema"
	openaiapi "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/mackross/agentloom/threads"
)

func TestResponsesStreamerPreviousResponseIDRetriesFullRequestOnContinuationError(t *testing.T) {
	// SSE-only: first call succeeds with a tool call; second uses previous_response_id
	// and gets a stream error before emission; third is the full-history fallback.
	requests := make(chan []byte, 3)
	requestCount := 0
	var requestMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := mustReadAll(t, r.Body)
		requests <- append([]byte(nil), body...)

		requestMu.Lock()
		requestCount++
		count := requestCount
		requestMu.Unlock()
		raw := parseObservedRawRequest(t, body)

		w.Header().Set("Content-Type", "text/event-stream")
		if count == 1 {
			if _, ok := raw["previous_response_id"]; ok {
				t.Errorf("first request had previous_response_id: %s", body)
			}
			_, _ = w.Write([]byte("" +
				"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_1\",\"name\":\"lookup\",\"arguments\":\"\",\"status\":\"in_progress\"}}\n\n" +
				"data: {\"type\":\"response.function_call_arguments.done\",\"item_id\":\"fc_1\",\"name\":\"lookup\",\"arguments\":\"{}\"}\n\n" +
				"data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_1\",\"name\":\"lookup\",\"arguments\":\"{}\",\"status\":\"completed\"}}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\"}}\n\n" +
				"data: [DONE]\n\n"))
			return
		}
		if stringValue(raw["previous_response_id"]) != "" {
			// Fail continuation before any output item is emitted so retry kicks in.
			_, _ = w.Write([]byte("" +
				"data: {\"type\":\"error\",\"message\":\"bad previous_response_id\"}\n\n" +
				"data: [DONE]\n\n"))
			return
		}
		_, _ = w.Write([]byte("" +
			"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_2\"}}\n\n" +
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\",\"item_id\":\"msg_2\"}\n\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_2\"}}\n\n" +
			"data: [DONE]\n\n"))
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

func TestResponsesStreamerPreviousResponseIDOmitsInstructionsWithToolsSSE(t *testing.T) {
	// xAI rejects requests that set both instructions and previous_response_id.
	requests := make(chan []byte, 2)
	requestCount := 0
	var requestMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		body := mustReadAll(t, r.Body)
		requests <- append([]byte(nil), body...)

		requestMu.Lock()
		requestCount++
		count := requestCount
		requestMu.Unlock()

		raw := parseObservedRawRequest(t, body)
		w.Header().Set("Content-Type", "text/event-stream")
		if count == 1 {
			if stringValue(raw["instructions"]) == "" {
				t.Errorf("first request missing instructions: %s", body)
			}
			if _, ok := raw["previous_response_id"]; ok {
				t.Errorf("first request had previous_response_id: %s", body)
			}
			// previous_response_id is on; stream_tool_calls must be omitted so xAI
			// stores continuable function-call arguments.
			if _, ok := raw["stream_tool_calls"]; ok {
				t.Errorf("first request stream_tool_calls = %#v, want omitted with previous_response_id", raw["stream_tool_calls"])
			}
			_, _ = w.Write([]byte("" +
				"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_tool_1\"}}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_1\",\"name\":\"echo\",\"arguments\":\"\",\"status\":\"in_progress\"}}\n\n" +
				"data: {\"type\":\"response.function_call_arguments.done\",\"item_id\":\"fc_1\",\"name\":\"echo\",\"arguments\":\"{\\\"message\\\":\\\"hi\\\"}\"}\n\n" +
				"data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_1\",\"name\":\"echo\",\"arguments\":\"{\\\"message\\\":\\\"hi\\\"}\",\"status\":\"completed\"}}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_tool_1\"}}\n\n" +
				"data: [DONE]\n\n"))
			return
		}

		if got := stringValue(raw["previous_response_id"]); got != "resp_tool_1" {
			t.Errorf("continuation previous_response_id = %q, want resp_tool_1; body=%s", got, body)
		}
		if _, ok := raw["instructions"]; ok {
			t.Errorf("continuation must omit instructions when previous_response_id is set; body=%s", body)
		}
		if got := len(objectSlice(t, raw["input"])); got != 2 {
			t.Errorf("continuation input len = %d, want 2 (function_call + function_call_output); body=%s", got, body)
		}
		_, _ = w.Write([]byte("" +
			"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_tool_2\"}}\n\n" +
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"done\",\"item_id\":\"msg_2\"}\n\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_tool_2\"}}\n\n" +
			"data: [DONE]\n\n"))
	}))
	defer server.Close()

	client := openaiapi.NewClient(
		option.WithAPIKey("test"),
		option.WithBaseURL(server.URL),
		option.WithHTTPClient(server.Client()),
		option.WithMaxRetries(0),
	)
	streamer := NewResponsesStreamerWithClient(client, "test-model")

	tools := threads.ToolOfferSnapshot{Offered: []threads.ToolSpec{{
		Name:        "echo",
		Description: "echo",
		Payload: threads.ToolPayloadJSONSchema(gschema.Schema{
			Type: "object",
			Properties: map[string]*gschema.Schema{
				"message": {Type: "string"},
			},
			Required: []string{"message"},
		}),
	}}}

	var first []threads.Item
	if err := streamer.StreamReq(threads.Req{
		Instruction: "use the tool",
		Items:       []threads.Item{threads.UserText("call echo")},
		Tools:       tools,
	}, func(item threads.Item) error {
		first = append(first, item)
		return nil
	}); err != nil {
		t.Fatalf("first request: %v", err)
	}
	var call threads.ToolCall
	for _, item := range first {
		if tc, ok := item.(threads.ToolCall); ok {
			call = tc
			break
		}
	}
	if call.CallID == "" {
		t.Fatalf("first request missing tool call: %#v", first)
	}

	var second []threads.Item
	if err := streamer.StreamReq(threads.Req{
		Instruction: "use the tool",
		Items: []threads.Item{
			threads.UserText("call echo"),
			call,
			threads.ToolCallResult{CallID: call.CallID, Output: "hi"},
		},
		Tools: tools,
	}, func(item threads.Item) error {
		second = append(second, item)
		return nil
	}); err != nil {
		t.Fatalf("second request: %v", err)
	}
	if !streamer.LastUsedPreviousResponseID() {
		t.Fatal("second request did not use previous_response_id")
	}
	if len(second) != 1 || second[0] != threads.AssistantText("done") {
		t.Fatalf("second emitted %#v, want assistant done", second)
	}

	for i := 0; i < 2; i++ {
		select {
		case <-requests:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for request %d", i+1)
		}
	}
}

func mustReadAll(t testing.TB, r io.Reader) []byte {
	t.Helper()
	body, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return body
}

func objectSlice(t testing.TB, raw any) []map[string]any {
	t.Helper()
	if raw == nil {
		return nil
	}
	items, ok := raw.([]any)
	if !ok {
		t.Fatalf("expected array payload, got %#v", raw)
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("expected object payload, got %#v", item)
		}
		out = append(out, m)
	}
	return out
}

func stringValue(v any) string {
	s, _ := v.(string)
	return s
}
