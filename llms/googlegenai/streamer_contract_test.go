package googlegenai

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/streamertest"
	"google.golang.org/genai"
)

func TestGenerateContentStreamerContract(t *testing.T) {
	streamertest.RunContractTests(t, googlegenaiContractHarness{})
}

type googlegenaiContractHarness struct{}

func (googlegenaiContractHarness) Capabilities() streamertest.Capabilities {
	return streamertest.Capabilities{}
}

func (googlegenaiContractHarness) Stream(t testing.TB, req threads.Req, events []streamertest.Event, emit func(threads.Item) error) (streamertest.ObservedRequest, error) {
	t.Helper()

	bodyCh := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method: %s", r.Method)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		bodyCh <- append([]byte(nil), body...)

		if len(events) > 0 && events[0].Err != "" {
			http.Error(w, events[0].Err, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, encodeGenerateContentStreamEvents(t, events))
	}))
	defer server.Close()

	client, err := genai.NewClient(t.Context(), &genai.ClientConfig{
		APIKey:      "test",
		Backend:     genai.BackendGeminiAPI,
		HTTPClient:  server.Client(),
		HTTPOptions: genai.HTTPOptions{BaseURL: server.URL, APIVersion: "v1beta"},
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	streamer := NewGenerateContentStreamerWithClient(client, "test-model")
	err = streamer.StreamReq(req, emit)

	select {
	case body := <-bodyCh:
		return parseObservedRequest(t, req, body), err
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for outbound request")
		return streamertest.ObservedRequest{}, err
	}
}

func encodeGenerateContentStreamEvents(t testing.TB, events []streamertest.Event) string {
	t.Helper()
	var out []byte
	for _, event := range events {
		if event.Err != "" {
			payload, _ := json.Marshal(map[string]any{"error": map[string]any{"message": event.Err}})
			out = append(out, []byte("data: ")...)
			out = append(out, payload...)
			out = append(out, '\n', '\n')
			continue
		}
		var part map[string]any
		switch v := event.Item.(type) {
		case threads.AssistantText:
			part = map[string]any{"text": string(v)}
		case threads.ToolCall:
			var args map[string]any
			if err := json.Unmarshal([]byte(v.Payload), &args); err != nil {
				t.Fatalf("unmarshal tool payload: %v", err)
			}
			part = map[string]any{"functionCall": map[string]any{"id": v.CallID, "name": v.Name, "args": args}}
		case threads.ToolCallChunk:
			continue
		default:
			t.Fatalf("unsupported contract event item: %T", event.Item)
		}
		payload, err := json.Marshal(map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"role": "model", "parts": []any{part}}}}})
		if err != nil {
			t.Fatalf("marshal stream event: %v", err)
		}
		out = append(out, []byte("data: ")...)
		out = append(out, payload...)
		out = append(out, '\n', '\n')
	}
	return string(out)
}

func parseObservedRequest(t testing.TB, req threads.Req, body []byte) streamertest.ObservedRequest {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("unmarshal request body: %v\n%s", err, body)
	}
	out := streamertest.ObservedRequest{
		Instruction: parseSystemInstruction(raw["systemInstruction"]),
		Items:       parseObservedContents(t, raw["contents"]),
		Tools:       parseObservedTools(t, raw["tools"]),
		ToolChoice:  parseObservedToolChoice(raw["toolConfig"]),
	}
	out.Parallel = req.Tools.Parallel
	return out
}

func parseSystemInstruction(raw any) string {
	obj, _ := raw.(map[string]any)
	return partsText(obj["parts"])
}

func parseObservedContents(t testing.TB, raw any) []streamertest.ObservedInputItem {
	t.Helper()
	contents := objectSlice(t, raw)
	out := make([]streamertest.ObservedInputItem, 0)
	for _, content := range contents {
		role := stringValue(content["role"])
		for _, part := range objectSlice(t, content["parts"]) {
			if text := stringValue(part["text"]); text != "" {
				kind := "user_text"
				if role == "model" {
					kind = "assistant_text"
				}
				out = append(out, streamertest.ObservedInputItem{Kind: kind, Text: text})
				continue
			}
			if fc, ok := part["functionCall"].(map[string]any); ok {
				payload, _ := json.Marshal(fc["args"])
				out = append(out, streamertest.ObservedInputItem{Kind: "tool_call", CallID: stringValue(fc["id"]), Name: stringValue(fc["name"]), Payload: string(payload)})
				continue
			}
			if fr, ok := part["functionResponse"].(map[string]any); ok {
				resp, _ := fr["response"].(map[string]any)
				out = append(out, streamertest.ObservedInputItem{Kind: "tool_result", CallID: stringValue(fr["id"]), Output: stringValue(resp["output"])})
			}
		}
	}
	return out
}

func parseObservedTools(t testing.TB, raw any) []streamertest.ObservedTool {
	t.Helper()
	tools := objectSlice(t, raw)
	var out []streamertest.ObservedTool
	for _, tool := range tools {
		for _, decl := range objectSlice(t, tool["functionDeclarations"]) {
			obs := streamertest.ObservedTool{Kind: "function", Name: stringValue(decl["name"]), Description: stringValue(decl["description"]), SchemaType: "object"}
			if params, ok := decl["parametersJsonSchema"].(map[string]any); ok {
				if typ := stringValue(params["type"]); typ != "" {
					obs.SchemaType = typ
				}
			}
			if params, ok := decl["parameters"].(map[string]any); ok {
				if typ := stringValue(params["type"]); typ != "" {
					obs.SchemaType = typ
				}
			}
			out = append(out, obs)
		}
	}
	return out
}

func parseObservedToolChoice(raw any) streamertest.ObservedToolChoice {
	obj, _ := raw.(map[string]any)
	fc, _ := obj["functionCallingConfig"].(map[string]any)
	mode := stringValue(fc["mode"])
	if mode == "" {
		return streamertest.ObservedToolChoice{}
	}
	outMode := "auto"
	switch mode {
	case "ANY":
		outMode = "required"
	case "VALIDATED":
		outMode = "allowed"
	case "NONE":
		outMode = "none"
	}
	choice := streamertest.ObservedToolChoice{Mode: outMode}
	for _, name := range stringSlice(fc["allowedFunctionNames"]) {
		choice.Allowed = append(choice.Allowed, streamertest.ObservedAllowedTool{Kind: "function", Name: name})
	}
	return choice
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

func partsText(raw any) string {
	text := ""
	if items, ok := raw.([]any); ok {
		for _, item := range items {
			if obj, ok := item.(map[string]any); ok {
				text += stringValue(obj["text"])
			}
		}
	}
	return text
}
func stringValue(v any) string { s, _ := v.(string); return s }
func stringSlice(v any) []string {
	items, _ := v.([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s := stringValue(item); s != "" {
			out = append(out, s)
		}
	}
	return out
}
