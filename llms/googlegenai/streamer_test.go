package googlegenai

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/streamertest"
	"google.golang.org/genai"
)

func TestGenerateContentStreamerContract(t *testing.T) {
	streamertest.Run(t, googlegenaiContractHarness{})
}

func TestGenerateContentStreamerConstructorDefaults(t *testing.T) {
	streamer := NewGenerateContentStreamerWithClient(nil, " ")
	if streamer.model != DefaultModel {
		t.Fatalf("model = %q, want %q", streamer.model, DefaultModel)
	}
	if got := streamer.Capabilities().Reasoning; got != threads.ReasoningForProvider("google.gemini") {
		t.Fatalf("reasoning strategy = %#v", got)
	}

	legacy := NewGenerateContentStreamerWithClient(nil, " gemini-3.1-flash-lite ")
	if legacy.model != "gemini-3.1-flash-lite" {
		t.Fatalf("legacy model = %q", legacy.model)
	}
	if got := legacy.Capabilities().Reasoning; got != threads.ReasoningForCurrentTurn("google.gemini") {
		t.Fatalf("legacy reasoning strategy = %#v", got)
	}
}

func TestGemini38RequestConfigValidation(t *testing.T) {
	float := float32(0.5)
	budget := int32(100)
	tests := []struct {
		name   string
		config genai.GenerateContentConfig
		want   string
	}{
		{name: "negative candidate count", config: genai.GenerateContentConfig{CandidateCount: -1}, want: "non-negative"},
		{name: "multiple candidates", config: genai.GenerateContentConfig{CandidateCount: 2}, want: "multiple candidates"},
		{name: "minimal thinking", config: genai.GenerateContentConfig{ThinkingConfig: &genai.ThinkingConfig{ThinkingLevel: genai.ThinkingLevelMinimal}}, want: "minimal"},
		{name: "unknown thinking", config: genai.GenerateContentConfig{ThinkingConfig: &genai.ThinkingConfig{ThinkingLevel: "EXTREME"}}, want: "invalid thinking level"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			streamer := &GenerateContentStreamer{model: DefaultModel, Config: tt.config}
			_, err := streamer.geminiRequestConfig(threads.Req{})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want text %q", err, tt.want)
			}
		})
	}

	acceptedConfigs := []struct {
		name   string
		config genai.GenerateContentConfig
	}{
		{name: "temperature", config: genai.GenerateContentConfig{Temperature: &float}},
		{name: "top p", config: genai.GenerateContentConfig{TopP: &float}},
		{name: "top k", config: genai.GenerateContentConfig{TopK: &float}},
		{name: "one candidate", config: genai.GenerateContentConfig{CandidateCount: 1}},
		{name: "thinking budget", config: genai.GenerateContentConfig{ThinkingConfig: &genai.ThinkingConfig{ThinkingBudget: &budget}}},
	}
	for _, tt := range acceptedConfigs {
		t.Run("accepts "+tt.name, func(t *testing.T) {
			streamer := &GenerateContentStreamer{model: DefaultModel, Config: tt.config}
			got, err := streamer.geminiRequestConfig(threads.Req{})
			if err != nil {
				t.Fatalf("request config: %v", err)
			}
			if !reflect.DeepEqual(got, tt.config) {
				t.Fatalf("request config\n got: %#v\nwant: %#v", got, tt.config)
			}
		})
	}

	for _, level := range []genai.ThinkingLevel{
		genai.ThinkingLevelUnspecified,
		genai.ThinkingLevelLow,
		genai.ThinkingLevelMedium,
		genai.ThinkingLevelHigh,
	} {
		t.Run("accepts "+string(level), func(t *testing.T) {
			streamer := &GenerateContentStreamer{
				model: DefaultModel,
				Config: genai.GenerateContentConfig{ThinkingConfig: &genai.ThinkingConfig{
					IncludeThoughts: true,
					ThinkingLevel:   level,
				}},
			}
			got, err := streamer.geminiRequestConfig(threads.Req{Instruction: "test"})
			if err != nil {
				t.Fatalf("request config: %v", err)
			}
			if got.ThinkingConfig == nil || got.ThinkingConfig.ThinkingLevel != level || !got.ThinkingConfig.IncludeThoughts {
				t.Fatalf("thinking config = %#v", got.ThinkingConfig)
			}
			if streamer.Config.SystemInstruction != nil {
				t.Fatal("building a request mutated the configured system instruction")
			}
		})
	}

	legacy := &GenerateContentStreamer{
		model:  "gemini-2.5-flash",
		Config: genai.GenerateContentConfig{Temperature: &float},
	}
	if _, err := legacy.geminiRequestConfig(threads.Req{}); err != nil {
		t.Fatalf("legacy model configuration unexpectedly rejected: %v", err)
	}
}

func TestValidateGemini38Contents(t *testing.T) {
	validToolRound := []*genai.Content{
		genai.NewContentFromText("run it", genai.RoleUser),
		genai.NewContentFromParts([]*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "c1", Name: "lookup"}}}, genai.RoleModel),
		genai.NewContentFromParts([]*genai.Part{{FunctionResponse: &genai.FunctionResponse{
			ID: "c1", Name: "lookup", Response: map[string]any{"output": "done"},
		}}}, genai.RoleUser),
	}
	if err := validateGemini38Contents(validToolRound); err != nil {
		t.Fatalf("valid tool round: %v", err)
	}

	tests := []struct {
		name     string
		contents []*genai.Content
		want     string
	}{
		{name: "empty", want: "requires request content"},
		{name: "empty user", contents: []*genai.Content{genai.NewContentFromText("", genai.RoleUser)}, want: "non-empty text"},
		{name: "prefilled model", contents: []*genai.Content{
			genai.NewContentFromText("question", genai.RoleUser),
			genai.NewContentFromText("prefix", genai.RoleModel),
		}, want: "prefilled model turn"},
		{name: "missing call id", contents: []*genai.Content{
			genai.NewContentFromText("question", genai.RoleUser),
			genai.NewContentFromParts([]*genai.Part{{FunctionCall: &genai.FunctionCall{Name: "lookup"}}}, genai.RoleModel),
		}, want: "require id and name"},
		{name: "mismatched response", contents: []*genai.Content{
			genai.NewContentFromText("question", genai.RoleUser),
			genai.NewContentFromParts([]*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "c1", Name: "lookup"}}}, genai.RoleModel),
			genai.NewContentFromParts([]*genai.Part{{FunctionResponse: &genai.FunctionResponse{
				ID: "c1", Name: "other", Response: map[string]any{"output": "done"},
			}}}, genai.RoleUser),
		}, want: "does not match"},
		{name: "missing response", contents: []*genai.Content{
			genai.NewContentFromText("question", genai.RoleUser),
			genai.NewContentFromParts([]*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "c1", Name: "lookup"}}}, genai.RoleModel),
			genai.NewContentFromText("continue", genai.RoleUser),
		}, want: "missing responses"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateGemini38Contents(tt.contents)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want text %q", err, tt.want)
			}
		})
	}
}

func TestRequestToolsPreservesJSONSchemaCustomEncoding(t *testing.T) {
	type args struct {
		Location string   `json:"location"`
		Tags     []string `json:"tags"`
	}
	tools, err := requestTools(threads.ToolOfferSnapshot{Offered: []threads.ToolSpec{{
		Name:    "lookup",
		Payload: threads.ToolPayloadFor[args](),
	}}})
	if err != nil {
		t.Fatalf("request tools: %v", err)
	}

	raw, err := json.Marshal(tools)
	if err != nil {
		t.Fatalf("marshal tools: %v", err)
	}
	var encoded []struct {
		FunctionDeclarations []struct {
			Parameters map[string]any `json:"parametersJsonSchema"`
		} `json:"functionDeclarations"`
	}
	if err := json.Unmarshal(raw, &encoded); err != nil {
		t.Fatalf("unmarshal tools: %v", err)
	}
	if len(encoded) != 1 || len(encoded[0].FunctionDeclarations) != 1 {
		t.Fatalf("encoded tools = %s", raw)
	}
	params := encoded[0].FunctionDeclarations[0].Parameters
	if params["type"] != "object" {
		t.Fatalf("root schema type = %#v, want object; schema=%s", params["type"], raw)
	}
	properties, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema properties = %#v; schema=%s", params["properties"], raw)
	}
	tags, ok := properties["tags"].(map[string]any)
	if !ok || tags["items"] == nil {
		t.Fatalf("array item schema was not preserved: %#v; schema=%s", properties["tags"], raw)
	}
}

func TestGeminiFinishError(t *testing.T) {
	for _, reason := range []genai.FinishReason{"", genai.FinishReasonUnspecified, genai.FinishReasonStop} {
		if err := geminiFinishError(&genai.Candidate{FinishReason: reason}); err != nil {
			t.Fatalf("finish reason %q: %v", reason, err)
		}
	}
	err := geminiFinishError(&genai.Candidate{
		FinishReason:  genai.FinishReasonTooManyToolCalls,
		FinishMessage: "tool limit reached",
	})
	if err == nil || !strings.Contains(err.Error(), "TOO_MANY_TOOL_CALLS") || !strings.Contains(err.Error(), "tool limit reached") {
		t.Fatalf("finish error = %v", err)
	}
}

func TestGemini38RejectsInvalidConfigBeforeHTTP(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()

	streamer := NewGenerateContentStreamerWithClient(newGoogleTestClient(t, server), DefaultModel)
	streamer.Config.CandidateCount = 2
	err := streamer.StreamReq(
		threads.Req{Items: []threads.Item{threads.UserText("hello")}},
		func(threads.Item) error { return nil },
	)
	if err == nil || !strings.Contains(err.Error(), "multiple candidates") {
		t.Fatalf("error = %v", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("HTTP requests = %d, want 0", got)
	}
}

func TestGenerateContentStreamerReturnsFinishReason(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"candidates":[{"finishReason":"TOO_MANY_TOOL_CALLS","finishMessage":"tool limit reached"}]}`+"\n\n")
	}))
	defer server.Close()

	streamer := NewGenerateContentStreamerWithClient(newGoogleTestClient(t, server), DefaultModel)
	err := streamer.StreamReq(
		threads.Req{Items: []threads.Item{threads.UserText("hello")}},
		func(threads.Item) error { return nil },
	)
	if err == nil || !strings.Contains(err.Error(), "TOO_MANY_TOOL_CALLS") {
		t.Fatalf("finish error = %v", err)
	}
}

type googlegenaiContractHarness struct {
	model string
}

func (googlegenaiContractHarness) Capabilities() streamertest.Capabilities {
	return streamertest.Capabilities{}
}

func (h googlegenaiContractHarness) Stream(t testing.TB, req threads.Req, events []streamertest.Event, emit func(threads.Item) error) (streamertest.ObservedRequest, error) {
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

	client := newGoogleTestClient(t, server)
	model := h.model
	if model == "" {
		model = "test-model"
	}
	streamer := NewGenerateContentStreamerWithClient(client, model)
	err := streamer.StreamReq(req, emit)

	select {
	case body := <-bodyCh:
		return parseObservedRequest(t, req, body), err
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for outbound request")
		return streamertest.ObservedRequest{}, err
	}
}

func newGoogleTestClient(t testing.TB, server *httptest.Server) *genai.Client {
	t.Helper()
	client, err := genai.NewClient(t.Context(), &genai.ClientConfig{
		APIKey:      "test",
		Backend:     genai.BackendGeminiAPI,
		HTTPClient:  server.Client(),
		HTTPOptions: genai.HTTPOptions{BaseURL: server.URL, APIVersion: "v1beta"},
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return client
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
		case signedGeminiPart:
			switch item := v.item.(type) {
			case nil:
				part = map[string]any{}
			case threads.AssistantText:
				part = map[string]any{"text": string(item)}
			case threads.ToolCall:
				var args map[string]any
				if err := json.Unmarshal([]byte(item.Payload), &args); err != nil {
					t.Fatalf("unmarshal signed tool payload: %v", err)
				}
				part = map[string]any{"functionCall": map[string]any{"id": item.CallID, "name": item.Name, "args": args}}
			default:
				t.Fatalf("unsupported signed part: %T", v.item)
			}
			part["thought"] = v.thought
			part["thoughtSignature"] = v.signature
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
			obs := streamertest.ObservedTool{Kind: "function", Name: stringValue(decl["name"]), Description: stringValue(decl["description"])}
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
