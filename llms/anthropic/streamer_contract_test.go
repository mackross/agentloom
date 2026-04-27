package anthropic

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	anthropicapi "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/streamertest"
)

func TestMessagesStreamerContract(t *testing.T) {
	streamertest.RunContractTests(t, anthropicContractHarness{})
}

func TestMessagesStreamerReportsAssistantPrefixCapability(t *testing.T) {
	tests := []struct {
		model string
		want  bool
	}{
		{model: string(anthropicapi.ModelClaudeSonnet4_6), want: false},
		{model: string(anthropicapi.ModelClaudeSonnet4_5), want: true},
		{model: "claude-haiku-4-7", want: false},
		{model: "claude-sonnet-4-10", want: false},
	}

	for _, tt := range tests {
		streamer := NewMessagesStreamerWithClient(anthropicapi.Client{}, tt.model)
		if got := streamer.Capabilities(); got.AssistantPrefix != tt.want || got.ToolResultSendPolicy != threads.ToolResultSendRequiresComplete {
			t.Fatalf("assistant-prefix capability for %q = %#v, want %v", tt.model, got, tt.want)
		}
	}
}

func TestMessagesStreamerOrdersToolResultsBeforeUserText(t *testing.T) {
	got, err := anthropicContractHarness{}.Stream(t, threads.Req{Items: []threads.Item{
		threads.ToolCall{CallID: "c1", Name: "alpha", Payload: `{"a":1}`},
		threads.AssistantText("between"),
		threads.ToolCall{CallID: "c2", Name: "beta", Payload: `{"b":2}`},
		threads.AssistantText("after"),
		threads.UserText("continue after tools"),
		threads.ToolCallResult{CallID: "c2", Output: "2"},
		threads.ToolCallResult{CallID: "c1", Output: "1"},
	}}, nil, func(threads.Item) error { return nil })
	if err != nil {
		t.Fatalf("stream req: %v", err)
	}
	want := []streamertest.ObservedInputItem{
		{Kind: "tool_call", CallID: "c1", Name: "alpha", Payload: `{"a":1}`},
		{Kind: "assistant_text", Text: "between"},
		{Kind: "tool_call", CallID: "c2", Name: "beta", Payload: `{"b":2}`},
		{Kind: "assistant_text", Text: "after"},
		{Kind: "tool_result", CallID: "c1", Output: "1"},
		{Kind: "tool_result", CallID: "c2", Output: "2"},
		{Kind: "user_text", Text: "continue after tools"},
	}
	if !reflect.DeepEqual(got.Items, want) {
		t.Fatalf("unexpected ordered items:\ngot:  %#v\nwant: %#v", got.Items, want)
	}
}

type anthropicContractHarness struct{}

func (anthropicContractHarness) Capabilities() streamertest.Capabilities {
	return streamertest.Capabilities{ToolCallChunks: true}
}

func (anthropicContractHarness) Stream(t testing.TB, req threads.Req, events []streamertest.Event, emit func(threads.Item) error) (streamertest.ObservedRequest, error) {
	t.Helper()

	bodyCh := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method: %s", r.Method)
		}
		if r.URL.Path != "/v1/messages" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		bodyCh <- append([]byte(nil), body...)

		w.Header().Set("Content-Type", "text/event-stream")
		if _, err := io.WriteString(w, encodeMessageStreamEvents(t, events)); err != nil {
			t.Errorf("write response stream: %v", err)
		}
	}))
	defer server.Close()

	client := anthropicapi.NewClient(
		option.WithAPIKey("test"),
		option.WithBaseURL(server.URL),
		option.WithHTTPClient(server.Client()),
		option.WithMaxRetries(0),
	)
	streamer := NewMessagesStreamerWithClient(client, "test-model")
	err := streamer.StreamReq(req, emit)

	select {
	case body := <-bodyCh:
		return parseObservedRequest(t, req, body), err
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for outbound request")
		return streamertest.ObservedRequest{}, err
	}
}

func encodeMessageStreamEvents(t testing.TB, events []streamertest.Event) string {
	t.Helper()

	type toolState struct {
		index  int
		name   string
		chunks int
	}

	var out []byte
	appendEvent := func(eventType string, payload map[string]any) {
		t.Helper()
		line, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal stream event: %v", err)
		}
		out = append(out, []byte("event: ")...)
		out = append(out, eventType...)
		out = append(out, '\n')
		out = append(out, []byte("data: ")...)
		out = append(out, line...)
		out = append(out, '\n', '\n')
	}

	appendEvent("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            "msg_1",
			"type":          "message",
			"role":          "assistant",
			"model":         "test-model",
			"content":       []any{},
			"stop_reason":   "",
			"stop_sequence": "",
			"container": map[string]any{
				"id":         "",
				"expires_at": "0001-01-01T00:00:00Z",
			},
			"usage": map[string]any{
				"cache_creation": map[string]any{
					"ephemeral_1h_input_tokens": 0,
					"ephemeral_5m_input_tokens": 0,
				},
				"cache_creation_input_tokens": 0,
				"cache_read_input_tokens":     0,
				"inference_geo":               "",
				"input_tokens":                0,
				"output_tokens":               0,
				"server_tool_use": map[string]any{
					"web_fetch_requests":  0,
					"web_search_requests": 0,
				},
				"service_tier": "standard",
			},
		},
	})

	toolCalls := map[string]*toolState{}
	nextIndex := 0

	startToolState := func(callID, name string, input any) *toolState {
		if state, ok := toolCalls[callID]; ok {
			if state.name == "" && name != "" {
				state.name = name
			}
			return state
		}
		state := &toolState{index: nextIndex, name: name}
		nextIndex++
		toolCalls[callID] = state
		appendEvent("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": state.index,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    callID,
				"name":  name,
				"input": input,
			},
		})
		return state
	}

	for _, ev := range events {
		if ev.Err != "" {
			appendEvent("error", map[string]any{
				"type":    "api_error",
				"message": ev.Err,
			})
			continue
		}

		switch v := ev.Item.(type) {
		case threads.AssistantText:
			index := nextIndex
			nextIndex++
			appendEvent("content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": index,
				"content_block": map[string]any{
					"type": "text",
					"text": "",
				},
			})
			appendEvent("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": index,
				"delta": map[string]any{
					"type": "text_delta",
					"text": string(v),
				},
			})
			appendEvent("content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": index,
			})
		case threads.ToolCallChunk:
			state := startToolState(v.CallID, v.Name, map[string]any{})
			state.chunks++
			appendEvent("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": state.index,
				"delta": map[string]any{
					"type":         "input_json_delta",
					"partial_json": v.PayloadDelta,
				},
			})
		case threads.ToolCall:
			input := map[string]any{}
			if state, ok := toolCalls[v.CallID]; ok {
				appendEvent("content_block_stop", map[string]any{
					"type":  "content_block_stop",
					"index": state.index,
				})
				continue
			}
			if v.Payload != "" {
				input = mustJSONObject(t, v.Payload)
			}
			state := startToolState(v.CallID, v.Name, input)
			appendEvent("content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": state.index,
			})
		case nil:
		default:
			t.Fatalf("unsupported contract event item: %T", ev.Item)
		}
	}

	appendEvent("message_stop", map[string]any{"type": "message_stop"})
	return string(out)
}

func parseObservedRequest(t testing.TB, req threads.Req, body []byte) streamertest.ObservedRequest {
	t.Helper()

	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("unmarshal outbound request: %v", err)
	}

	choice, parallel := parseObservedToolChoice(t, req, raw["tool_choice"])
	return streamertest.ObservedRequest{
		Instruction: extractSystemText(t, raw["system"]),
		Items:       parseObservedMessages(t, raw["messages"]),
		Tools:       parseObservedTools(t, raw["tools"]),
		ToolChoice:  choice,
		Parallel:    parallel,
	}
}

func extractSystemText(t testing.TB, raw any) string {
	t.Helper()
	if raw == nil {
		return ""
	}
	blocks := objectSlice(t, raw)
	text := ""
	for _, block := range blocks {
		if stringValue(block["type"]) != "text" {
			t.Fatalf("unsupported system block: %#v", block)
		}
		text += stringValue(block["text"])
	}
	return text
}

func parseObservedMessages(t testing.TB, raw any) []streamertest.ObservedInputItem {
	t.Helper()
	messages := objectSlice(t, raw)
	if messages == nil {
		return nil
	}

	out := make([]streamertest.ObservedInputItem, 0)
	for _, msg := range messages {
		role := stringValue(msg["role"])
		for _, block := range objectSlice(t, msg["content"]) {
			switch stringValue(block["type"]) {
			case "text":
				item := streamertest.ObservedInputItem{Text: stringValue(block["text"])}
				switch role {
				case "user":
					item.Kind = "user_text"
				case "assistant":
					item.Kind = "assistant_text"
				default:
					t.Fatalf("unsupported message role: %#v", msg)
				}
				out = append(out, item)
			case "tool_use":
				out = append(out, streamertest.ObservedInputItem{
					Kind:    "tool_call",
					CallID:  stringValue(block["id"]),
					Name:    stringValue(block["name"]),
					Payload: mustJSONString(t, block["input"]),
				})
			case "tool_result":
				out = append(out, streamertest.ObservedInputItem{
					Kind:   "tool_result",
					CallID: stringValue(block["tool_use_id"]),
					Output: extractToolResultText(t, block["content"]),
				})
			default:
				t.Fatalf("unsupported message content block: %#v", block)
			}
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
			Kind:        "function",
			Name:        stringValue(item["name"]),
			Description: stringValue(item["description"]),
		}
		if schema, ok := item["input_schema"].(map[string]any); ok {
			tool.SchemaType = stringValue(schema["type"])
		}
		out = append(out, tool)
	}
	return out
}

func parseObservedToolChoice(t testing.TB, req threads.Req, raw any) (streamertest.ObservedToolChoice, *bool) {
	t.Helper()
	if raw == nil {
		return streamertest.ObservedToolChoice{}, nil
	}

	obj, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("unsupported tool choice payload: %#v", raw)
	}

	var parallel *bool
	if disabled, ok := obj["disable_parallel_tool_use"].(bool); ok {
		parallel = boolRef(!disabled)
	}

	choice := streamertest.ObservedToolChoice{}
	switch stringValue(obj["type"]) {
	case "none":
		choice.Mode = "none"
	case "tool":
		choice.Mode = "allowed"
		choice.Allowed = []streamertest.ObservedAllowedTool{{
			Kind: "function",
			Name: stringValue(obj["name"]),
		}}
	case "auto", "any":
		if req.Tools.Allowed != nil && len(req.Tools.Allowed) > 0 {
			choice.Mode = "allowed"
			choice.Allowed = make([]streamertest.ObservedAllowedTool, 0, len(req.Tools.Allowed))
			for _, name := range req.Tools.Allowed {
				choice.Allowed = append(choice.Allowed, streamertest.ObservedAllowedTool{
					Kind: "function",
					Name: name,
				})
			}
		} else {
			choice.Mode = stringValue(obj["type"])
		}
	default:
		t.Fatalf("unsupported tool choice payload: %#v", raw)
	}
	return choice, parallel
}

func extractToolResultText(t testing.TB, raw any) string {
	t.Helper()
	if text, ok := raw.(string); ok {
		return text
	}
	parts := objectSlice(t, raw)
	text := ""
	for _, part := range parts {
		if stringValue(part["type"]) != "text" {
			t.Fatalf("unsupported tool result content: %#v", part)
		}
		text += stringValue(part["text"])
	}
	return text
}

func mustJSONString(t testing.TB, raw any) string {
	t.Helper()
	buf, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal json payload: %v", err)
	}
	return string(buf)
}

func mustJSONObject(t testing.TB, raw string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("unmarshal json payload: %v", err)
	}
	return out
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
		obj, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("expected object payload, got %#v", item)
		}
		out = append(out, obj)
	}
	return out
}

func boolRef(v bool) *bool {
	return &v
}
