package cerebras

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	gschema "github.com/google/jsonschema-go/jsonschema"
	openaiapi "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/vmihailenco/msgpack/v5"

	cachecerebras "github.com/mackross/agentloom/llms/cache/cerebras"
	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/streamertest"
)

func TestChatCompletionsStreamerContract(t *testing.T) {
	streamertest.RunContractTests(t, cerebrasContractHarness{})
}

func TestChatCompletionsStreamerConstructorDefaults(t *testing.T) {
	streamer := NewChatCompletionsStreamerWithClient(openaiapi.Client{}, "")
	if streamer.model != DefaultModel {
		t.Fatalf("default model = %q, want %q", streamer.model, DefaultModel)
	}
	streamer = NewChatCompletionsStreamerWithClient(openaiapi.Client{}, "  "+GLM47Model+"  ")
	if streamer.model != GLM47Model {
		t.Fatalf("explicit model = %q, want %q", streamer.model, GLM47Model)
	}
}

func TestChatCompletionsStreamerReportsAssistantPrefixCapability(t *testing.T) {
	streamer := NewChatCompletionsStreamerWithClient(openaiapi.Client{}, "")
	if got := streamer.Capabilities(); !got.AssistantPrefix || got.ToolResultSendPolicy != threads.ToolResultSendPermissive {
		t.Fatalf("expected assistant-prefix capability, got %#v", got)
	}
}

func TestChatCompletionsStreamerOptionsAndOptimizedPayload(t *testing.T) {
	clearThinking := true
	parallel := true
	temperature := 0.0
	var predictionCalls int
	streamer := newTestStreamer(t, "", []streamertest.Event{streamertest.Emit(threads.AssistantText("ok"))}, func(*http.Request, map[string]any) {
		// msgpack+gzip middleware sees the final SDK JSON body, including stream:true
		// and option.WithJSONSet fields, before rewriting the payload.
	})
	streamer.ReasoningEffort = "high"
	streamer.ReasoningFormat = "hidden"
	streamer.Temperature = &temperature
	streamer.ClearThinking = &clearThinking
	streamer.ServiceTier = "auto"
	streamer.QueueThreshold = "1000"
	streamer.Prediction = func(req threads.Req) (string, bool, error) {
		predictionCalls++
		if len(req.Items) != 1 || req.Items[0] != threads.UserText("hello") {
			t.Fatalf("prediction saw unexpected normalized request: %#v", req)
		}
		return "expected output", true, nil
	}

	captured := streamer.capture
	err := streamer.StreamReq(threads.Req{
		Items: []threads.Item{threads.UserText("hello")},
		ItemMeta: []map[string]any{
			cachecerebras.PromptCacheKey("old"),
			cachecerebras.ClearPromptCacheKey(),
			cachecerebras.PromptCacheKey("new"),
		},
		Tools: threads.ToolOfferSnapshot{Parallel: &parallel},
	}, func(threads.Item) error { return nil })
	if err != nil {
		t.Fatalf("stream req: %v", err)
	}
	if predictionCalls != 1 {
		t.Fatalf("prediction calls = %d, want 1", predictionCalls)
	}

	req, raw := captured.request(t)
	if req.Header.Get("Content-Type") != "application/vnd.msgpack" {
		t.Fatalf("content-type = %q", req.Header.Get("Content-Type"))
	}
	if req.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("content-encoding = %q", req.Header.Get("Content-Encoding"))
	}
	if req.Header.Get("queue_threshold") != "1000" {
		t.Fatalf("queue_threshold header = %q", req.Header.Get("queue_threshold"))
	}
	wantStrings := map[string]string{
		"model":            GLM47Model,
		"reasoning_effort": "high",
		"reasoning_format": "hidden",
		"service_tier":     "auto",
		"prompt_cache_key": "new",
	}
	for key, want := range wantStrings {
		if got := stringValue(raw[key]); got != want {
			t.Fatalf("%s = %q, want %q in %#v", key, got, want, raw)
		}
	}
	if got, ok := raw["clear_thinking"].(bool); !ok || !got {
		t.Fatalf("clear_thinking = %#v, want true", raw["clear_thinking"])
	}
	if got, ok := raw["stream"].(bool); !ok || !got {
		t.Fatalf("stream = %#v, want true", raw["stream"])
	}
	if got, ok := raw["parallel_tool_calls"].(bool); !ok || !got {
		t.Fatalf("parallel_tool_calls = %#v, want true", raw["parallel_tool_calls"])
	}
	if got := numericFloat(raw["temperature"]); got != 0 {
		t.Fatalf("temperature = %#v, want 0", raw["temperature"])
	}
	prediction, ok := raw["prediction"].(map[string]any)
	if !ok {
		t.Fatalf("prediction missing: %#v", raw["prediction"])
	}
	if stringValue(prediction["type"]) != "content" || stringValue(prediction["content"]) != "expected output" {
		t.Fatalf("unexpected prediction payload: %#v", prediction)
	}
}

func TestChatCompletionsStreamerOnOutputTextDelta(t *testing.T) {
	streamer := newTestStreamer(t, "", []streamertest.Event{streamertest.Emit(threads.AssistantText("hel")), streamertest.Emit(threads.AssistantText("lo"))}, nil)
	var callback strings.Builder
	streamer.OnOutputTextDelta = func(delta string) { callback.WriteString(delta) }

	var emitted []threads.Item
	if err := streamer.StreamReq(threads.Req{Items: []threads.Item{threads.UserText("hello")}}, func(item threads.Item) error {
		emitted = append(emitted, item)
		return nil
	}); err != nil {
		t.Fatalf("stream req: %v", err)
	}
	if callback.String() != "hello" {
		t.Fatalf("OnOutputTextDelta saw %q, want hello", callback.String())
	}
	if !reflect.DeepEqual(emitted, []threads.Item{threads.AssistantText("hel"), threads.AssistantText("lo")}) {
		t.Fatalf("unexpected emitted items: %#v", emitted)
	}
}

func TestChatCompletionsStreamerToolNormalizers(t *testing.T) {
	t.Run("normalizes_request_and_response_tool_calls", func(t *testing.T) {
		streamer := newTestStreamer(t, "", []streamertest.Event{streamertest.Emit(threads.ToolCall{CallID: "c2", Name: "provider_tool", Payload: `{"provider":true}`})}, nil)
		streamer.RegisterToolNormalizer("canonical_tool", threads.ToolNormalizer{
			NormalizeSpec: func(spec threads.ToolSpec) (threads.ToolSpec, error) {
				spec.Name = "provider_tool"
				spec.Description = "provider description"
				return spec, nil
			},
			NormalizeRequestToolCall: func(call threads.ToolCall) (threads.ToolCall, error) {
				call.Name = "provider_tool"
				call.Payload = `{"provider_request":true}`
				return call, nil
			},
		})
		streamer.RegisterToolNormalizer("provider_tool", threads.ToolNormalizer{
			NormalizeResponseToolCall: func(call threads.ToolCall) (threads.ToolCall, error) {
				call.Name = "canonical_tool"
				call.Payload = `{"canonical":true}`
				return call, nil
			},
		})

		var emitted []threads.Item
		err := streamer.StreamReq(threads.Req{
			Items: []threads.Item{
				threads.ToolCall{CallID: "c1", Name: "canonical_tool", Payload: `{"canonical_request":true}`},
			},
			Tools: threads.ToolOfferSnapshot{Offered: []threads.ToolSpec{{
				Name:        "canonical_tool",
				Description: "canonical description",
				Payload:     threads.ToolPayloadJSONSchema(gschema.Schema{Type: "object"}),
			}}},
		}, func(item threads.Item) error {
			emitted = append(emitted, item)
			return nil
		})
		if err != nil {
			t.Fatalf("stream req: %v", err)
		}
		_, raw := streamer.capture.request(t)
		gotTools := parseObservedTools(t, raw["tools"])
		if !reflect.DeepEqual(gotTools, []streamertest.ObservedTool{{
			Kind:        "function",
			Name:        "provider_tool",
			Description: "provider description",
			SchemaType:  "object",
		}}) {
			t.Fatalf("unexpected normalized tools: %#v", gotTools)
		}
		gotItems := parseObservedMessages(t, raw["messages"])
		if !reflect.DeepEqual(gotItems, []streamertest.ObservedInputItem{{
			Kind:    "tool_call",
			CallID:  "c1",
			Name:    "provider_tool",
			Payload: `{"provider_request":true}`,
		}}) {
			t.Fatalf("unexpected normalized request items: %#v", gotItems)
		}
		if !reflect.DeepEqual(emitted, []threads.Item{threads.ToolCall{CallID: "c2", Name: "canonical_tool", Payload: `{"canonical":true}`}}) {
			t.Fatalf("unexpected normalized response items: %#v", emitted)
		}
	})

	t.Run("unregister_stops_normalizing_specs", func(t *testing.T) {
		streamer := newTestStreamer(t, "", nil, nil)
		streamer.RegisterToolNormalizer("canonical_tool", threads.ToolNormalizer{
			NormalizeSpec: func(spec threads.ToolSpec) (threads.ToolSpec, error) {
				spec.Name = "provider_tool"
				return spec, nil
			},
		})
		streamer.UnregisterToolNormalizer("canonical_tool")

		if err := streamer.StreamReq(threads.Req{Tools: threads.ToolOfferSnapshot{Offered: []threads.ToolSpec{{
			Name:    "canonical_tool",
			Payload: threads.ToolPayloadJSONSchema(gschema.Schema{Type: "object"}),
		}}}}, func(threads.Item) error { return nil }); err != nil {
			t.Fatalf("stream req: %v", err)
		}
		_, raw := streamer.capture.request(t)
		gotTools := parseObservedTools(t, raw["tools"])
		if !reflect.DeepEqual(gotTools, []streamertest.ObservedTool{{
			Kind:       "function",
			Name:       "canonical_tool",
			SchemaType: "object",
		}}) {
			t.Fatalf("unexpected tools after unregister: %#v", gotTools)
		}
	})
}

func TestChatCompletionsStreamerClearThinkingRejectsNonGLM(t *testing.T) {
	clearThinking := true
	streamer := newTestStreamer(t, GPTOSS120BModel, nil, nil)
	streamer.ClearThinking = &clearThinking
	if err := streamer.StreamReq(threads.Req{Items: []threads.Item{threads.UserText("hello")}}, func(threads.Item) error { return nil }); err == nil || !strings.Contains(err.Error(), "clear_thinking") {
		t.Fatalf("expected clear_thinking local error, got %v", err)
	}
	if streamer.capture.count() != 0 {
		t.Fatal("request was sent despite local clear_thinking validation error")
	}
}

func TestChatCompletionsStreamerAcceptsAllCerebrasServiceTiers(t *testing.T) {
	for _, tier := range []string{"priority", "default", "auto", "flex"} {
		t.Run(tier, func(t *testing.T) {
			streamer := newTestStreamer(t, "", nil, nil)
			streamer.ServiceTier = tier
			if err := streamer.StreamReq(threads.Req{Items: []threads.Item{threads.UserText("hello")}}, func(threads.Item) error { return nil }); err != nil {
				t.Fatalf("stream req: %v", err)
			}
			_, raw := streamer.capture.request(t)
			if got := stringValue(raw["service_tier"]); got != tier {
				t.Fatalf("service_tier = %q, want %q", got, tier)
			}
		})
	}
}

func TestChatCompletionsStreamerInvalidServiceTierRejected(t *testing.T) {
	streamer := newTestStreamer(t, "", nil, nil)
	streamer.ServiceTier = "scale"
	if err := streamer.StreamReq(threads.Req{Items: []threads.Item{threads.UserText("hello")}}, func(threads.Item) error { return nil }); err == nil || !strings.Contains(err.Error(), "service tier") {
		t.Fatalf("expected service-tier local error, got %v", err)
	}
	if streamer.capture.count() != 0 {
		t.Fatal("request was sent despite local service-tier validation error")
	}
}

func TestChatCompletionsStreamerPredictionCallbackBehavior(t *testing.T) {
	t.Run("ok_false_omits_prediction", func(t *testing.T) {
		var calls int
		streamer := newTestStreamer(t, "", nil, nil)
		streamer.Prediction = func(req threads.Req) (string, bool, error) {
			calls++
			return "", false, nil
		}
		if err := streamer.StreamReq(threads.Req{Items: []threads.Item{threads.UserText("hello")}}, func(threads.Item) error { return nil }); err != nil {
			t.Fatalf("stream req: %v", err)
		}
		_, raw := streamer.capture.request(t)
		if _, ok := raw["prediction"]; ok {
			t.Fatalf("prediction should be omitted: %#v", raw["prediction"])
		}
		if calls != 1 {
			t.Fatalf("prediction calls = %d, want 1", calls)
		}
	})

	t.Run("callback_error_aborts_before_request", func(t *testing.T) {
		streamer := newTestStreamer(t, "", nil, nil)
		sentinel := errors.New("prediction failed")
		streamer.Prediction = func(req threads.Req) (string, bool, error) { return "", false, sentinel }
		if err := streamer.StreamReq(threads.Req{Items: []threads.Item{threads.UserText("hello")}}, func(threads.Item) error { return nil }); !errors.Is(err, sentinel) {
			t.Fatalf("expected prediction error, got %v", err)
		}
		if streamer.capture.count() != 0 {
			t.Fatal("request was sent despite prediction error")
		}
	})

	t.Run("prediction_rejects_tools", func(t *testing.T) {
		streamer := newTestStreamer(t, "", nil, nil)
		streamer.Prediction = func(req threads.Req) (string, bool, error) { return "x", true, nil }
		err := streamer.StreamReq(threads.Req{
			Items: []threads.Item{threads.UserText("hello")},
			Tools: threads.ToolOfferSnapshot{Offered: []threads.ToolSpec{{
				Name: "sum", Payload: threads.ToolPayloadJSONSchema(gschema.Schema{Type: "object"}),
			}}},
		}, func(threads.Item) error { return nil })
		if err == nil || !strings.Contains(err.Error(), "prediction") || !strings.Contains(err.Error(), "tools") {
			t.Fatalf("expected prediction/tools local error, got %v", err)
		}
		if streamer.capture.count() != 0 {
			t.Fatal("request was sent despite prediction/tools validation error")
		}
	})
}

func TestChatCompletionsStreamerRejectsUnsupportedRequestInputs(t *testing.T) {
	t.Run("item", func(t *testing.T) {
		if _, err := requestMessages(threads.Req{Items: []threads.Item{unsupportedItem{}}}); err == nil || !strings.Contains(err.Error(), "cerebras request item not supported") {
			t.Fatalf("expected unsupported-item error, got %v", err)
		}
	})

	t.Run("tool_payload", func(t *testing.T) {
		_, err := requestTools(threads.ToolOfferSnapshot{Offered: []threads.ToolSpec{{Name: "bad"}}})
		if err == nil || !strings.Contains(err.Error(), `cerebras tool "bad" payload not supported`) {
			t.Fatalf("expected unsupported-tool-payload error, got %v", err)
		}
	})
}

func TestChatCompletionsStreamerToolChoiceEdgeCases(t *testing.T) {
	tool := func(name string) threads.ToolSpec {
		return threads.ToolSpec{Name: name, Payload: threads.ToolPayloadJSONSchema(gschema.Schema{Type: "object"})}
	}

	t.Run("required_with_allowed_unset", func(t *testing.T) {
		streamer := newTestStreamer(t, "", nil, nil)
		err := streamer.StreamReq(threads.Req{Tools: threads.ToolOfferSnapshot{
			Offered:  []threads.ToolSpec{tool("sum")},
			Required: true,
		}}, func(threads.Item) error { return nil })
		if err != nil {
			t.Fatalf("stream req: %v", err)
		}
		_, raw := streamer.capture.request(t)
		if got := stringValue(raw["tool_choice"]); got != "required" {
			t.Fatalf("tool_choice = %q, want required", got)
		}
	})

	t.Run("multiple_allowed_auto_filters_tools", func(t *testing.T) {
		streamer := newTestStreamer(t, "", nil, nil)
		err := streamer.StreamReq(threads.Req{Tools: threads.ToolOfferSnapshot{
			Offered: []threads.ToolSpec{tool("sum"), tool("echo"), tool("drop")},
			Allowed: []string{"sum", "echo"},
		}}, func(threads.Item) error { return nil })
		if err != nil {
			t.Fatalf("stream req: %v", err)
		}
		_, raw := streamer.capture.request(t)
		if got := stringValue(raw["tool_choice"]); got != "auto" {
			t.Fatalf("tool_choice = %q, want auto", got)
		}
		gotTools := parseObservedTools(t, raw["tools"])
		if !reflect.DeepEqual(gotTools, []streamertest.ObservedTool{
			{Kind: "function", Name: "sum", SchemaType: "object"},
			{Kind: "function", Name: "echo", SchemaType: "object"},
		}) {
			t.Fatalf("unexpected filtered tools: %#v", gotTools)
		}
	})

	t.Run("allowed_tool_not_offered", func(t *testing.T) {
		streamer := newTestStreamer(t, "", nil, nil)
		err := streamer.StreamReq(threads.Req{Tools: threads.ToolOfferSnapshot{
			Offered: []threads.ToolSpec{tool("sum")},
			Allowed: []string{"missing"},
		}}, func(threads.Item) error { return nil })
		if err == nil || !strings.Contains(err.Error(), `cerebras tool "missing" not offered`) {
			t.Fatalf("expected missing-tool error, got %v", err)
		}
		if streamer.capture.count() != 0 {
			t.Fatal("request was sent despite missing allowed tool")
		}
	})

	t.Run("required_empty_allowed_set", func(t *testing.T) {
		streamer := newTestStreamer(t, "", nil, nil)
		err := streamer.StreamReq(threads.Req{Tools: threads.ToolOfferSnapshot{
			Offered:  []threads.ToolSpec{tool("sum")},
			Allowed:  []string{},
			Required: true,
		}}, func(threads.Item) error { return nil })
		if err == nil || !strings.Contains(err.Error(), "cannot require an empty allowed tool set") {
			t.Fatalf("expected empty-required error, got %v", err)
		}
		if streamer.capture.count() != 0 {
			t.Fatal("request was sent despite invalid tool choice")
		}
	})
}

func TestRequestFunctionParametersClosesObjectSchemasRecursively(t *testing.T) {
	params, err := requestFunctionParameters("nested", threads.ToolPayloadJSONSchema(gschema.Schema{
		Type: "object",
		Properties: map[string]*gschema.Schema{
			"config": {
				Type: "object",
				Properties: map[string]*gschema.Schema{
					"labels": {Type: "array", Items: &gschema.Schema{Type: "object"}},
				},
			},
			"open": {Type: "object", AdditionalProperties: &gschema.Schema{Type: "string"}},
		},
	}))
	if err != nil {
		t.Fatalf("requestFunctionParameters: %v", err)
	}
	if got := params["additionalProperties"]; got != false {
		t.Fatalf("root additionalProperties = %#v, want false", got)
	}
	config := params["properties"].(map[string]any)["config"].(map[string]any)
	if got := config["additionalProperties"]; got != false {
		t.Fatalf("nested additionalProperties = %#v, want false", got)
	}
	labelsItems := config["properties"].(map[string]any)["labels"].(map[string]any)["items"].(map[string]any)
	if got := labelsItems["additionalProperties"]; got != false {
		t.Fatalf("array object additionalProperties = %#v, want false", got)
	}
	open := params["properties"].(map[string]any)["open"].(map[string]any)
	openProps := open["properties"].(map[string]any)
	if _, ok := openProps["_"].(map[string]any); !ok {
		t.Fatalf("additionalProperties-only object was not converted to a concrete property: %#v", open)
	}
	if got := open["additionalProperties"]; got != false {
		t.Fatalf("converted additionalProperties = %#v, want false", got)
	}
}

func TestMsgpackGzipRewriteSkipsNonCerebrasHosts(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/v1/chat/completions", bytes.NewBufferString(`{"n":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if shouldRewriteMsgpackGzip(req) {
		t.Fatal("non-Cerebras host should not be rewritten")
	}
}

type cerebrasContractHarness struct{}

func (cerebrasContractHarness) Capabilities() streamertest.Capabilities {
	return streamertest.Capabilities{ToolCallChunks: true}
}

func (cerebrasContractHarness) Stream(t testing.TB, req threads.Req, events []streamertest.Event, emit func(threads.Item) error) (streamertest.ObservedRequest, error) {
	t.Helper()
	streamer := newTestStreamer(t, "test-model", events, nil)
	err := streamer.StreamReq(req, emit)
	_, raw := streamer.capture.request(t)
	return parseObservedRequest(t, req, raw), err
}

type capturedRequest struct {
	req  *http.Request
	body []byte
}

type requestCapture struct {
	ch chan capturedRequest
}

func (c *requestCapture) count() int { return len(c.ch) }

func (c *requestCapture) request(t testing.TB) (*http.Request, map[string]any) {
	t.Helper()
	select {
	case got := <-c.ch:
		return got.req, decodeMsgpackGzipBody(t, got.body)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for outbound request")
		return nil, nil
	}
}

type testStreamer struct {
	*ChatCompletionsStreamer
	capture *requestCapture
}

func newTestStreamer(t testing.TB, model string, events []streamertest.Event, observe func(*http.Request, map[string]any)) *testStreamer {
	t.Helper()
	capture := &requestCapture{ch: make(chan capturedRequest, 1)}
	client := openaiapi.NewClient(
		option.WithAPIKey("test"),
		option.WithBaseURL(BaseURL),
		option.WithHTTPClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method != http.MethodPost {
				t.Errorf("unexpected method: %s", r.Method)
			}
			if strings.TrimRight(r.URL.Path, "/") != "/v1/chat/completions" {
				t.Errorf("unexpected path: %s", r.URL.Path)
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read request body: %v", err)
			}
			raw := decodeMsgpackGzipBody(t, body)
			if observe != nil {
				observe(r, raw)
			}
			capture.ch <- capturedRequest{req: r.Clone(r.Context()), body: append([]byte(nil), body...)}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(encodeChatCompletionStreamEvents(t, events))),
				Request:    r,
			}, nil
		})),
		option.WithMaxRetries(0),
	)
	if model == "" {
		model = GLM47Model
	}
	return &testStreamer{ChatCompletionsStreamer: NewChatCompletionsStreamerWithClient(client, model), capture: capture}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

type unsupportedItem struct{}

func (unsupportedItem) Emit() bool { return true }

func decodeMsgpackGzipBody(t testing.TB, body []byte) map[string]any {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	decompressed, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read gzip: %v", err)
	}
	if err := zr.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	var raw map[string]any
	if err := msgpack.Unmarshal(decompressed, &raw); err != nil {
		t.Fatalf("decode msgpack: %v", err)
	}
	return raw
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
			appendChunk(map[string]any{"error": map[string]any{"message": ev.Err}})
			continue
		}
		switch v := ev.Item.(type) {
		case threads.AssistantText:
			appendChunk(map[string]any{"id": "chatcmpl_test", "object": "chat.completion.chunk", "created": 0, "model": "test-model", "choices": []any{map[string]any{"index": 0, "finish_reason": nil, "delta": map[string]any{"content": string(v)}}}})
		case threads.ToolCallChunk:
			meta := ensureToolMeta(v.CallID, v.Name)
			meta.hasChunks = true
			name := ""
			if !meta.nameStreamed {
				name = v.Name
				meta.nameStreamed = true
			}
			appendChunk(map[string]any{"id": "chatcmpl_test", "object": "chat.completion.chunk", "created": 0, "model": "test-model", "choices": []any{map[string]any{"index": 0, "finish_reason": nil, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": meta.index, "id": v.CallID, "type": "function", "function": map[string]any{"name": name, "arguments": v.PayloadDelta}}}}}}})
		case threads.ToolCall:
			meta := ensureToolMeta(v.CallID, v.Name)
			delta := map[string]any{}
			if !meta.hasChunks {
				delta = map[string]any{"tool_calls": []any{map[string]any{"index": meta.index, "id": v.CallID, "type": "function", "function": map[string]any{"name": v.Name, "arguments": v.Payload}}}}
			}
			appendChunk(map[string]any{"id": "chatcmpl_test", "object": "chat.completion.chunk", "created": 0, "model": "test-model", "choices": []any{map[string]any{"index": 0, "finish_reason": "tool_calls", "delta": delta}}})
		case nil:
		default:
			t.Fatalf("unsupported contract event item: %T", ev.Item)
		}
	}
	out = append(out, []byte("data: [DONE]\n\n")...)
	return string(out)
}

func parseObservedRequest(t testing.TB, req threads.Req, raw map[string]any) streamertest.ObservedRequest {
	t.Helper()
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
	text := ""
	for _, msg := range messages {
		if stringValue(msg["role"]) == "system" {
			text += extractMessageText(t, msg["content"])
		}
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
		switch role := stringValue(msg["role"]); role {
		case "system":
		case "user":
			out = append(out, streamertest.ObservedInputItem{Kind: "user_text", Text: extractMessageText(t, msg["content"])})
		case "assistant":
			if toolCalls := objectSlice(t, msg["tool_calls"]); toolCalls != nil {
				for _, tool := range toolCalls {
					function, ok := tool["function"].(map[string]any)
					if !ok {
						t.Fatalf("unsupported assistant tool call: %#v", tool)
					}
					out = append(out, streamertest.ObservedInputItem{Kind: "tool_call", CallID: stringValue(tool["id"]), Name: stringValue(function["name"]), Payload: stringValue(function["arguments"])})
				}
				continue
			}
			out = append(out, streamertest.ObservedInputItem{Kind: "assistant_text", Text: extractMessageText(t, msg["content"])})
		case "tool":
			out = append(out, streamertest.ObservedInputItem{Kind: "tool_result", CallID: stringValue(msg["tool_call_id"]), Output: extractMessageText(t, msg["content"])})
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
		tool := streamertest.ObservedTool{Kind: stringValue(item["type"])}
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
			return streamertest.ObservedToolChoice{Mode: mode, Allowed: []streamertest.ObservedAllowedTool{{Kind: "function", Name: stringValue(function["name"])}}}
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
		allowed = append(allowed, streamertest.ObservedAllowedTool{Kind: "function", Name: name})
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

func boolRef(v bool) *bool { return &v }

func numericFloat(v any) float64 {
	switch n := v.(type) {
	case int:
		return float64(n)
	case int8:
		return float64(n)
	case int16:
		return float64(n)
	case int32:
		return float64(n)
	case int64:
		return float64(n)
	case uint:
		return float64(n)
	case uint8:
		return float64(n)
	case uint16:
		return float64(n)
	case uint32:
		return float64(n)
	case uint64:
		return float64(n)
	case float32:
		return float64(n)
	case float64:
		return n
	default:
		return -1
	}
}

func TestEmitHelpersForCoverage(t *testing.T) {
	states := map[toolKey]*toolState{{choiceIndex: 0, toolIndex: 0}: {callID: "c", name: "n"}}
	var emitted []threads.Item
	if err := emitRemainingToolCalls(states, func(item threads.Item) error { emitted = append(emitted, item); return nil }); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(emitted, []threads.Item{threads.ToolCall{CallID: "c", Name: "n", Payload: ""}}) {
		t.Fatalf("unexpected emitted helper items: %#v", emitted)
	}
}
