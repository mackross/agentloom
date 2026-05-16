package fireworks

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	gschema "github.com/google/jsonschema-go/jsonschema"
	openaiapi "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/streamertest"
)

func TestChatCompletionsStreamerContract(t *testing.T) {
	streamertest.RunContractTests(t, fireworksContractHarness{})
}

func TestChatCompletionsStreamerReportsAssistantPrefixCapability(t *testing.T) {
	streamer := NewChatCompletionsStreamerWithClient(openaiapi.Client{}, "")

	if got := streamer.Capabilities(); !got.AssistantPrefix || got.ToolResultSendPolicy != threads.ToolResultSendPermissive {
		t.Fatalf("expected assistant-prefix capability, got %#v", got)
	}
}

func TestChatCompletionsStreamerSetsContextLengthExceededBehaviorToError(t *testing.T) {
	bodyCh := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		bodyCh <- append([]byte(nil), body...)
		w.Header().Set("Content-Type", "text/event-stream")
		if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
			t.Fatalf("write stream: %v", err)
		}
	}))
	defer server.Close()

	client := openaiapi.NewClient(
		option.WithAPIKey("test"),
		option.WithBaseURL(server.URL),
		option.WithHTTPClient(server.Client()),
		option.WithMaxRetries(0),
	)
	streamer := NewChatCompletionsStreamerWithClient(client, "")
	if err := streamer.StreamReq(threads.Req{
		Items: []threads.Item{threads.UserText("hello")},
		Tools: threads.ToolOfferSnapshot{Offered: []threads.ToolSpec{{
			Name:    "sum",
			Payload: threads.ToolPayloadJSONSchema(gschema.Schema{Type: "object"}),
		}}},
	}, func(threads.Item) error { return nil }); err != nil {
		t.Fatalf("stream req: %v", err)
	}

	select {
	case body := <-bodyCh:
		var raw map[string]any
		if err := json.Unmarshal(body, &raw); err != nil {
			t.Fatalf("unmarshal request body: %v", err)
		}
		if got := stringValue(raw["context_length_exceeded_behavior"]); got != DefaultContextLengthExceededBehavior {
			t.Fatalf("unexpected context overflow behavior: %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for outbound request")
	}
}

type fireworksContractHarness struct{}

func (fireworksContractHarness) Capabilities() streamertest.Capabilities {
	return streamertest.Capabilities{ToolCallChunks: true}
}

func (fireworksContractHarness) Stream(t testing.TB, req threads.Req, events []streamertest.Event, emit func(threads.Item) error) (streamertest.ObservedRequest, error) {
	t.Helper()

	bodyCh := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method: %s", r.Method)
		}
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		bodyCh <- append([]byte(nil), body...)

		w.Header().Set("Content-Type", "text/event-stream")
		if _, err := io.WriteString(w, encodeChatCompletionStreamEvents(t, events)); err != nil {
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
	streamer := NewChatCompletionsStreamerWithClient(client, "test-model")
	err := streamer.StreamReq(req, emit)

	select {
	case body := <-bodyCh:
		return parseObservedRequest(t, req, body), err
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for outbound request")
		return streamertest.ObservedRequest{}, err
	}
}

func encodeChatCompletionStreamEvents(t testing.TB, events []streamertest.Event) string {
	t.Helper()

	type toolMeta struct {
		index        int
		name         string
		hasChunks    bool
		nameStreamed bool
	}

	var out []byte
	appendChunk := func(payload map[string]any) {
		t.Helper()
		line, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal stream chunk: %v", err)
		}
		out = append(out, []byte("data: ")...)
		out = append(out, line...)
		out = append(out, '\n', '\n')
	}

	toolCalls := map[string]*toolMeta{}
	nextToolIndex := 0

	ensureToolMeta := func(callID, name string) *toolMeta {
		if meta, ok := toolCalls[callID]; ok {
			if meta.name == "" && name != "" {
				meta.name = name
			}
			return meta
		}
		meta := &toolMeta{index: nextToolIndex, name: name}
		nextToolIndex++
		toolCalls[callID] = meta
		return meta
	}

	for _, ev := range events {
		if ev.Err != "" {
			appendChunk(map[string]any{
				"error": map[string]any{"message": ev.Err},
			})
			continue
		}

		switch v := ev.Item.(type) {
		case threads.AssistantText:
			appendChunk(map[string]any{
				"id":      "chatcmpl_test",
				"object":  "chat.completion.chunk",
				"created": 0,
				"model":   "test-model",
				"choices": []any{map[string]any{
					"index":         0,
					"finish_reason": nil,
					"delta": map[string]any{
						"content": string(v),
					},
				}},
			})
		case threads.ToolCallChunk:
			meta := ensureToolMeta(v.CallID, v.Name)
			meta.hasChunks = true
			name := ""
			if !meta.nameStreamed {
				name = v.Name
				meta.nameStreamed = true
			}
			appendChunk(map[string]any{
				"id":      "chatcmpl_test",
				"object":  "chat.completion.chunk",
				"created": 0,
				"model":   "test-model",
				"choices": []any{map[string]any{
					"index":         0,
					"finish_reason": nil,
					"delta": map[string]any{
						"tool_calls": []any{map[string]any{
							"index": meta.index,
							"id":    v.CallID,
							"type":  "function",
							"function": map[string]any{
								"name":      name,
								"arguments": v.PayloadDelta,
							},
						}},
					},
				}},
			})
		case threads.ToolCall:
			meta := ensureToolMeta(v.CallID, v.Name)
			finishReason := "tool_calls"
			delta := map[string]any{}
			if meta.hasChunks {
				delta = map[string]any{}
			} else {
				delta = map[string]any{
					"tool_calls": []any{map[string]any{
						"index": meta.index,
						"id":    v.CallID,
						"type":  "function",
						"function": map[string]any{
							"name":      v.Name,
							"arguments": v.Payload,
						},
					}},
				}
			}
			appendChunk(map[string]any{
				"id":      "chatcmpl_test",
				"object":  "chat.completion.chunk",
				"created": 0,
				"model":   "test-model",
				"choices": []any{map[string]any{
					"index":         0,
					"finish_reason": finishReason,
					"delta":         delta,
				}},
			})
		case nil:
		default:
			t.Fatalf("unsupported contract event item: %T", ev.Item)
		}
	}

	out = append(out, []byte("data: [DONE]\n\n")...)
	return string(out)
}

func parseObservedRequest(t testing.TB, req threads.Req, body []byte) streamertest.ObservedRequest {
	t.Helper()

	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("unmarshal outbound request: %v", err)
	}

	choice := parseObservedToolChoice(t, req, raw["tool_choice"])
	out := streamertest.ObservedRequest{
		Instruction: extractSystemInstruction(t, raw["messages"]),
		Items:       parseObservedMessages(t, raw["messages"]),
		Tools:       parseObservedTools(t, raw["tools"]),
		ToolChoice:  choice,
	}
	if v, ok := raw["parallel_tool_calls"].(bool); ok {
		out.Parallel = boolRef(v)
	}
	return out
}

func extractSystemInstruction(t testing.TB, raw any) string {
	t.Helper()
	messages := objectSlice(t, raw)
	if messages == nil {
		return ""
	}
	text := ""
	for _, msg := range messages {
		if stringValue(msg["role"]) != "system" {
			continue
		}
		text += extractMessageText(t, msg["content"])
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
		switch role {
		case "system":
		case "user":
			out = append(out, streamertest.ObservedInputItem{
				Kind: "user_text",
				Text: extractMessageText(t, msg["content"]),
			})
		case "assistant":
			if toolCalls := objectSlice(t, msg["tool_calls"]); toolCalls != nil {
				for _, tool := range toolCalls {
					function, ok := tool["function"].(map[string]any)
					if !ok {
						t.Fatalf("unsupported assistant tool call: %#v", tool)
					}
					out = append(out, streamertest.ObservedInputItem{
						Kind:    "tool_call",
						CallID:  stringValue(tool["id"]),
						Name:    stringValue(function["name"]),
						Payload: stringValue(function["arguments"]),
					})
				}
				continue
			}
			out = append(out, streamertest.ObservedInputItem{
				Kind: "assistant_text",
				Text: extractMessageText(t, msg["content"]),
			})
		case "tool":
			out = append(out, streamertest.ObservedInputItem{
				Kind:   "tool_result",
				CallID: stringValue(msg["tool_call_id"]),
				Output: extractMessageText(t, msg["content"]),
			})
		default:
			t.Fatalf("unsupported message role: %#v", msg)
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
			Kind: stringValue(item["type"]),
		}
		if function, ok := item["function"].(map[string]any); ok {
			tool.Name = stringValue(function["name"])
			tool.Description = stringValue(function["description"])
			if params, ok := function["parameters"].(map[string]any); ok {
				tool.SchemaType = stringValue(params["type"])
			}
		}
		out = append(out, tool)
	}
	return out
}

func parseObservedToolChoice(t testing.TB, req threads.Req, raw any) streamertest.ObservedToolChoice {
	t.Helper()
	switch v := raw.(type) {
	case nil:
		return streamertest.ObservedToolChoice{}
	case string:
		if v == "none" {
			return streamertest.ObservedToolChoice{Mode: "none"}
		}
		if v == "required" && req.Tools.Allowed != nil && len(req.Tools.Allowed) > 0 {
			choice := allowedToolChoice(req.Tools.Allowed)
			choice.Mode = "required"
			return choice
		}
		if v == "auto" && req.Tools.Allowed != nil && len(req.Tools.Allowed) > 1 {
			return allowedToolChoice(req.Tools.Allowed)
		}
		return streamertest.ObservedToolChoice{Mode: v}
	case map[string]any:
		if stringValue(v["type"]) == "function" {
			function, _ := v["function"].(map[string]any)
			mode := "allowed"
			if req.Tools.Required {
				mode = "required"
			}
			return streamertest.ObservedToolChoice{
				Mode: mode,
				Allowed: []streamertest.ObservedAllowedTool{{
					Kind: "function",
					Name: stringValue(function["name"]),
				}},
			}
		}
		if stringValue(v["type"]) == "allowed_tools" && req.Tools.Allowed != nil {
			return allowedToolChoice(req.Tools.Allowed)
		}
		t.Fatalf("unsupported tool choice payload: %#v", raw)
		return streamertest.ObservedToolChoice{}
	default:
		t.Fatalf("unsupported tool choice payload: %#v", raw)
		return streamertest.ObservedToolChoice{}
	}
}

func allowedToolChoice(names []string) streamertest.ObservedToolChoice {
	allowed := make([]streamertest.ObservedAllowedTool, 0, len(names))
	for _, name := range names {
		allowed = append(allowed, streamertest.ObservedAllowedTool{
			Kind: "function",
			Name: name,
		})
	}
	return streamertest.ObservedToolChoice{Mode: "allowed", Allowed: allowed}
}

func extractMessageText(t testing.TB, raw any) string {
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
				t.Fatalf("unsupported message content part: %#v", item)
			}
			text += stringValue(part["text"])
		}
		return text
	default:
		t.Fatalf("unsupported message content payload: %#v", raw)
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
