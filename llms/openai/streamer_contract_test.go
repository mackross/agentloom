package openai

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"

	openaiapi "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"

	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/streamertest"
)

func TestResponsesStreamerContract(t *testing.T) {
	streamertest.RunContractTests(t, openAIContractHarness{})
}

func TestResponsesStreamerSSEContract(t *testing.T) {
	streamertest.RunContractTests(t, openAISSEContractHarness{})
}

func TestResponsesStreamerSendsReasoningEffort(t *testing.T) {
	bodyCh := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("websocket accept: %v", err)
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")

		_, body, err := conn.Read(r.Context())
		if err != nil {
			t.Errorf("read websocket message: %v", err)
			return
		}
		bodyCh <- append([]byte(nil), body...)
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(`[DONE]`))
	}))
	defer server.Close()

	client := openaiapi.NewClient(
		option.WithAPIKey("test"),
		option.WithBaseURL(server.URL),
		option.WithHTTPClient(server.Client()),
		option.WithMaxRetries(0),
	)
	streamer := NewResponsesStreamerWithClient(client, "test-model")
	streamer.Reasoning = shared.ReasoningParam{Effort: shared.ReasoningEffortHigh}
	if err := streamer.StreamReq(threads.Req{Items: []threads.Item{threads.UserText("hello")}}, func(threads.Item) error { return nil }); err != nil {
		t.Fatalf("stream request: %v", err)
	}

	select {
	case body := <-bodyCh:
		var raw map[string]any
		if err := json.Unmarshal(body, &raw); err != nil {
			t.Fatalf("unmarshal request body: %v", err)
		}
		reasoning, ok := raw["reasoning"].(map[string]any)
		if !ok || reasoning["effort"] != "high" {
			t.Fatalf("unexpected reasoning body: %#v", raw["reasoning"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for outbound request")
	}
}

type openAIContractHarness struct{}

func (openAIContractHarness) Capabilities() streamertest.Capabilities {
	return streamertest.Capabilities{ToolCallChunks: true}
}

func (openAIContractHarness) Stream(t testing.TB, req threads.Req, events []streamertest.Event, emit func(threads.Item) error) (streamertest.ObservedRequest, error) {
	t.Helper()

	bodyCh := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected method: %s", r.Method)
		}
		if r.URL.Path != "/responses" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}

		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("websocket accept: %v", err)
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")

		_, body, err := conn.Read(r.Context())
		if err != nil {
			t.Errorf("read websocket message: %v", err)
			return
		}
		bodyCh <- append([]byte(nil), body...)

		for _, msg := range encodeResponseWebSocketEvents(t, events) {
			if err := conn.Write(r.Context(), websocket.MessageText, msg); err != nil {
				t.Errorf("write websocket message: %v", err)
				return
			}
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
	err := streamer.StreamReq(req, emit)

	select {
	case body := <-bodyCh:
		return parseObservedRequest(t, body), err
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for outbound request")
		return streamertest.ObservedRequest{}, err
	}
}

type openAISSEContractHarness struct{}

func (openAISSEContractHarness) Capabilities() streamertest.Capabilities {
	return streamertest.Capabilities{ToolCallChunks: true}
}

func (openAISSEContractHarness) Stream(t testing.TB, req threads.Req, events []streamertest.Event, emit func(threads.Item) error) (streamertest.ObservedRequest, error) {
	t.Helper()

	bodyCh := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method: %s", r.Method)
		}
		if r.URL.Path != "/responses" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		bodyCh <- append([]byte(nil), body...)

		w.Header().Set("Content-Type", "text/event-stream")
		if _, err := io.WriteString(w, encodeResponseStreamEvents(t, events)); err != nil {
			t.Errorf("write response stream: %v", err)
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
	streamer.Transport = ResponsesTransportSSE
	err := streamer.StreamReq(req, emit)

	select {
	case body := <-bodyCh:
		return parseObservedRequest(t, body), err
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for outbound request")
		return streamertest.ObservedRequest{}, err
	}
}

func encodeResponseWebSocketEvents(t testing.TB, events []streamertest.Event) [][]byte {
	t.Helper()

	var messages [][]byte
	appendMessage := func(payload map[string]any) {
		t.Helper()
		msg, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal stream event: %v", err)
		}
		messages = append(messages, msg)
	}
	encodeResponseEvents(t, events, appendMessage)
	messages = append(messages, []byte(`[DONE]`))
	return messages
}

func encodeResponseStreamEvents(t testing.TB, events []streamertest.Event) string {
	t.Helper()

	var out []byte
	appendEvent := func(payload map[string]any) {
		t.Helper()
		line, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal stream event: %v", err)
		}
		out = append(out, []byte("data: ")...)
		out = append(out, line...)
		out = append(out, '\n', '\n')
	}
	encodeResponseEvents(t, events, appendEvent)

	out = append(out, []byte("data: [DONE]\n\n")...)
	return string(out)
}

func encodeResponseEvents(t testing.TB, events []streamertest.Event, appendEvent func(map[string]any)) {
	t.Helper()

	type toolMeta struct {
		ItemID      string
		OutputIndex int
		Name        string
	}

	toolCalls := map[string]toolMeta{}
	nextToolIndex := 0
	sequence := 1
	outputTextItemID := "msg_1"

	ensureToolMeta := func(callID, name string) toolMeta {
		meta, ok := toolCalls[callID]
		if ok {
			if meta.Name == "" && name != "" {
				meta.Name = name
				toolCalls[callID] = meta
			}
			return meta
		}
		meta = toolMeta{
			ItemID:      "fc_" + callID,
			OutputIndex: nextToolIndex,
			Name:        name,
		}
		nextToolIndex++
		toolCalls[callID] = meta
		appendEvent(map[string]any{
			"type":            "response.output_item.added",
			"output_index":    meta.OutputIndex,
			"sequence_number": sequence,
			"item": map[string]any{
				"type":      "function_call",
				"id":        meta.ItemID,
				"call_id":   callID,
				"name":      name,
				"arguments": "",
				"status":    "in_progress",
			},
		})
		sequence++
		return meta
	}

	for _, ev := range events {
		if ev.Err != "" {
			appendEvent(map[string]any{
				"type":            "error",
				"code":            "test_error",
				"message":         ev.Err,
				"param":           "",
				"sequence_number": sequence,
			})
			sequence++
			continue
		}

		switch v := ev.Item.(type) {
		case threads.AssistantText:
			appendEvent(map[string]any{
				"type":            "response.output_text.delta",
				"content_index":   0,
				"delta":           string(v),
				"item_id":         outputTextItemID,
				"logprobs":        []any{},
				"output_index":    0,
				"sequence_number": sequence,
			})
			sequence++
		case threads.ToolCallChunk:
			meta := ensureToolMeta(v.CallID, v.Name)
			appendEvent(map[string]any{
				"type":            "response.function_call_arguments.delta",
				"delta":           v.PayloadDelta,
				"item_id":         meta.ItemID,
				"output_index":    meta.OutputIndex,
				"sequence_number": sequence,
			})
			sequence++
		case threads.ToolCall:
			meta := ensureToolMeta(v.CallID, v.Name)
			name := v.Name
			if name == "" {
				name = meta.Name
			}
			appendEvent(map[string]any{
				"type":            "response.function_call_arguments.done",
				"arguments":       v.Payload,
				"item_id":         meta.ItemID,
				"name":            name,
				"output_index":    meta.OutputIndex,
				"sequence_number": sequence,
			})
			sequence++
		case nil:
		default:
			t.Fatalf("unsupported contract event item: %T", ev.Item)
		}
	}
}

func parseObservedRequest(t testing.TB, body []byte) streamertest.ObservedRequest {
	t.Helper()

	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("unmarshal outbound request: %v", err)
	}

	out := streamertest.ObservedRequest{
		Instruction: stringValue(raw["instructions"]),
		Items:       parseObservedInputItems(t, raw["input"]),
		Tools:       parseObservedTools(t, raw["tools"]),
		ToolChoice:  parseObservedToolChoice(t, raw["tool_choice"]),
	}
	if v, ok := raw["parallel_tool_calls"].(bool); ok {
		out.Parallel = boolRef(v)
	}
	return out
}

func parseObservedInputItems(t testing.TB, raw any) []streamertest.ObservedInputItem {
	t.Helper()
	items := objectSlice(t, raw)
	out := make([]streamertest.ObservedInputItem, 0, len(items))
	for _, item := range items {
		kind := stringValue(item["type"])
		if kind == "" && stringValue(item["role"]) != "" {
			kind = "message"
		}
		switch kind {
		case "message":
			role := stringValue(item["role"])
			kind := ""
			switch role {
			case "user":
				kind = "user_text"
			case "assistant":
				kind = "assistant_text"
			default:
				t.Fatalf("unsupported message role: %#v", item)
			}
			out = append(out, streamertest.ObservedInputItem{
				Kind: kind,
				Text: extractMessageText(t, item["content"]),
			})
		case "function_call":
			out = append(out, streamertest.ObservedInputItem{
				Kind:    "tool_call",
				CallID:  stringValue(item["call_id"]),
				Name:    stringValue(item["name"]),
				Payload: stringValue(item["arguments"]),
			})
		case "function_call_output":
			out = append(out, streamertest.ObservedInputItem{
				Kind:   "tool_result",
				CallID: stringValue(item["call_id"]),
				Output: extractToolOutput(t, item["output"]),
			})
		default:
			t.Fatalf("unsupported input item: %#v", item)
		}
	}
	return out
}

func parseObservedTools(t testing.TB, raw any) []streamertest.ObservedTool {
	t.Helper()
	items := objectSlice(t, raw)
	if items == nil {
		return nil
	}
	out := make([]streamertest.ObservedTool, 0, len(items))
	for _, item := range items {
		tool := streamertest.ObservedTool{
			Kind:        stringValue(item["type"]),
			Name:        stringValue(item["name"]),
			Description: stringValue(item["description"]),
		}
		if params, ok := item["parameters"].(map[string]any); ok {
			tool.SchemaType = stringValue(params["type"])
		}
		out = append(out, tool)
	}
	return out
}

func parseObservedToolChoice(t testing.TB, raw any) streamertest.ObservedToolChoice {
	t.Helper()
	switch v := raw.(type) {
	case nil:
		return streamertest.ObservedToolChoice{}
	case string:
		return streamertest.ObservedToolChoice{Mode: v}
	case map[string]any:
		out := streamertest.ObservedToolChoice{}
		if tools := objectSlice(t, v["tools"]); tools != nil {
			out.Allowed = make([]streamertest.ObservedAllowedTool, 0, len(tools))
			for _, tool := range tools {
				out.Allowed = append(out.Allowed, streamertest.ObservedAllowedTool{
					Kind: stringValue(tool["type"]),
					Name: stringValue(tool["name"]),
				})
			}
			if len(out.Allowed) > 0 {
				out.Mode = "allowed"
			}
		}
		if out.Mode == "" {
			if mode := stringValue(v["mode"]); mode != "" {
				if mode == "none" {
					out.Mode = "none"
				} else {
					out.Mode = mode
				}
			}
		}
		if out.Mode == "" {
			switch stringValue(v["type"]) {
			case "none":
				out.Mode = "none"
			case "allowed_tools":
				out.Mode = "allowed"
			}
		}
		return out
	default:
		t.Fatalf("unsupported tool choice payload: %#v", raw)
		return streamertest.ObservedToolChoice{}
	}
}

func extractMessageText(t testing.TB, raw any) string {
	t.Helper()
	if text, ok := raw.(string); ok {
		return text
	}
	parts := objectSlice(t, raw)
	text := ""
	for _, part := range parts {
		switch stringValue(part["type"]) {
		case "input_text", "output_text":
			text += stringValue(part["text"])
		default:
			t.Fatalf("unsupported message content: %#v", part)
		}
	}
	return text
}

func extractToolOutput(t testing.TB, raw any) string {
	t.Helper()
	switch v := raw.(type) {
	case nil:
		return ""
	case string:
		return v
	case []any:
		text := ""
		for _, item := range v {
			part, ok := item.(map[string]any)
			if !ok {
				t.Fatalf("unsupported tool output part: %#v", item)
			}
			text += stringValue(part["text"])
		}
		return text
	default:
		t.Fatalf("unsupported tool output payload: %#v", raw)
		return ""
	}
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

func boolRef(v bool) *bool {
	return &v
}
