package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	gschema "github.com/google/jsonschema-go/jsonschema"

	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/streamertest"
)

func TestChatStreamerContract(t *testing.T) {
	streamertest.Run(t, ollamaContractHarness{})
}

func TestChatStreamerConstructorAndCapabilities(t *testing.T) {
	t.Setenv("OLLAMA_HOST", "127.0.0.1:11435")
	streamer := NewChatStreamer("")
	if got := streamer.baseURL.String(); got != "http://127.0.0.1:11435" {
		t.Fatalf("base URL = %q, want http://127.0.0.1:11435", got)
	}
	if streamer.model != DefaultModel {
		t.Fatalf("model = %q, want %q", streamer.model, DefaultModel)
	}
	caps := streamer.Capabilities()
	if !caps.AssistantPrefix {
		t.Fatal("expected assistant-prefix capability")
	}
	if caps.Reasoning != threads.ReasoningForCurrentTurn(reasoningProvider) {
		t.Fatalf("reasoning capability = %#v", caps.Reasoning)
	}
	if first, second := streamer.SyntheticToolCallID(), streamer.SyntheticToolCallID(); first == "" || first == second {
		t.Fatalf("synthetic tool IDs are not distinct: %q %q", first, second)
	}
}

func TestChatURLPreservesEscapedPathPrefix(t *testing.T) {
	streamer := NewChatStreamerWithClient(nil, mustURL(t, "https://example.com/tenant%2Fone/api/"), "model")
	got := streamer.chatURL()
	if got.Path != "/tenant/one/api/chat" || got.EscapedPath() != "/tenant%2Fone/api/chat" {
		t.Fatalf("chat URL = %q (path %q), want escaped tenant prefix", got, got.Path)
	}
}

func TestChatStreamerSendsNativeOptionsHeadersAndAPIPath(t *testing.T) {
	var observed map[string]any
	var observedHeader http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/prefix/api/chat" {
			t.Errorf("path = %q, want /prefix/api/chat", r.URL.Path)
		}
		if got := r.Header.Get("Accept"); got != "application/x-ndjson" {
			t.Errorf("Accept = %q", got)
		}
		observedHeader = r.Header.Clone()
		if err := json.NewDecoder(r.Body).Decode(&observed); err != nil {
			t.Errorf("decode request: %v", err)
		}
		writeJSON(t, w, map[string]any{"message": map[string]any{"role": "assistant"}, "done": true})
	}))
	defer server.Close()

	base := mustURL(t, server.URL+"/prefix/api")
	streamer := NewChatStreamerWithClient(server.Client(), base, "test-model")
	truncate, shift := true, false
	streamer.Options = map[string]any{"num_ctx": 65536, "temperature": 0.2}
	streamer.Think = "high"
	streamer.KeepAlive = "10m"
	streamer.Format = json.RawMessage(`{"type":"object"}`)
	streamer.Truncate = &truncate
	streamer.Shift = &shift
	streamer.Headers = http.Header{"Authorization": []string{"Bearer test"}}

	if err := streamer.StreamReq(threads.Req{Items: []threads.Item{threads.UserText("hello")}}, func(threads.Item) error { return nil }); err != nil {
		t.Fatalf("stream request: %v", err)
	}
	if observedHeader.Get("Authorization") != "Bearer test" {
		t.Fatalf("authorization header = %q", observedHeader.Get("Authorization"))
	}
	if observed["model"] != "test-model" || observed["stream"] != true || observed["think"] != "high" || observed["keep_alive"] != "10m" {
		t.Fatalf("unexpected request controls: %#v", observed)
	}
	options, _ := observed["options"].(map[string]any)
	if options["num_ctx"] != float64(65536) || options["temperature"] != 0.2 {
		t.Fatalf("unexpected options: %#v", options)
	}
	format, _ := observed["format"].(map[string]any)
	if format["type"] != "object" || observed["truncate"] != true || observed["shift"] != false {
		t.Fatalf("unexpected format/truncation controls: %#v", observed)
	}
}

func TestConversationMessagesGroupsAssistantStateAndNamesToolResults(t *testing.T) {
	messages, err := conversationMessages(threads.Req{
		Instruction: "be concise",
		Items: []threads.Item{
			threads.UserText("calculate"),
			threads.ReasoningItem{Provider: reasoningProvider, Text: "need tools"},
			threads.AssistantText("working"),
			threads.ToolCall{CallID: "a", Name: "sum", Payload: `{"left":1,"right":2}`},
			threads.ToolCall{CallID: "b", Name: "echo", Payload: `{"value":"x"}`},
			threads.ToolCallResult{CallID: "a", Output: "3"},
			threads.ToolCallResult{CallID: "b", Output: "x"},
		},
	})
	if err != nil {
		t.Fatalf("conversation messages: %v", err)
	}
	if len(messages) != 5 {
		t.Fatalf("message count = %d, want 5: %#v", len(messages), messages)
	}
	assistant := messages[2]
	if assistant.Role != "assistant" || assistant.Thinking != "need tools" || assistant.Content != "working" || len(assistant.ToolCalls) != 2 {
		t.Fatalf("unexpected assistant message: %#v", assistant)
	}
	if got := string(assistant.ToolCalls[0].Function.Arguments); got != `{"left":1,"right":2}` {
		t.Fatalf("tool arguments changed: %s", got)
	}
	if messages[3].ToolName != "sum" || messages[3].ToolCallID != "a" ||
		messages[4].ToolName != "echo" || messages[4].ToolCallID != "b" {
		t.Fatalf("unexpected tool result messages: %#v", messages[3:5])
	}
}

func TestChatStreamerStreamsReasoningTextAndOrderedToolCalls(t *testing.T) {
	server := newNDJSONServer(t,
		map[string]any{"message": map[string]any{"role": "assistant", "thinking": "first "}, "done": false},
		map[string]any{"message": map[string]any{"role": "assistant", "thinking": "second"}, "done": false},
		map[string]any{"message": map[string]any{"role": "assistant", "content": "answer"}, "done": false},
		map[string]any{"message": map[string]any{"role": "assistant", "tool_calls": []any{
			map[string]any{"id": "c2", "function": map[string]any{"index": 1, "name": "beta", "arguments": map[string]any{"b": 2}}},
			map[string]any{"id": "c1", "function": map[string]any{"index": 0, "name": "alpha", "arguments": map[string]any{"a": 1}}},
		}}, "done": false},
		map[string]any{"message": map[string]any{"role": "assistant"}, "done": true},
	)
	defer server.Close()

	streamer := NewChatStreamerWithClient(server.Client(), mustURL(t, server.URL), "model")
	var callback strings.Builder
	streamer.OnOutputTextDelta = func(delta string) { callback.WriteString(delta) }
	var items []threads.Item
	if err := streamer.StreamReq(threads.Req{}, func(item threads.Item) error {
		items = append(items, item)
		return nil
	}); err != nil {
		t.Fatalf("stream request: %v", err)
	}
	want := []threads.Item{
		threads.ReasoningItem{Provider: reasoningProvider, Visibility: threads.ReasoningVisibilityText, Text: "first second"},
		threads.AssistantText("answer"),
		threads.ToolCall{CallID: "c1", Name: "alpha", Payload: `{"a":1}`},
		threads.ToolCall{CallID: "c2", Name: "beta", Payload: `{"b":2}`},
	}
	if !reflect.DeepEqual(items, want) {
		t.Fatalf("unexpected stream items:\n got: %#v\nwant: %#v", items, want)
	}
	if callback.String() != "answer" {
		t.Fatalf("output callback = %q", callback.String())
	}
}

func TestChatStreamerHandlesLargeToolPayloadWithoutScannerLimit(t *testing.T) {
	// Ollama's own Go client currently caps scanned NDJSON records at 8 MiB.
	// Decode directly so a valid, unusually large tool call is not truncated.
	large := strings.Repeat("abcdefgh", 1152*1024)
	server := newNDJSONServer(t,
		map[string]any{"message": map[string]any{"role": "assistant", "tool_calls": []any{
			map[string]any{"function": map[string]any{"index": 0, "name": "large", "arguments": map[string]any{"payload": large}}},
		}}, "done": false},
		map[string]any{"message": map[string]any{"role": "assistant"}, "done": true},
	)
	defer server.Close()

	streamer := NewChatStreamerWithClient(server.Client(), mustURL(t, server.URL), "model")
	var call threads.ToolCall
	if err := streamer.StreamReq(threads.Req{}, func(item threads.Item) error {
		if value, ok := item.(threads.ToolCall); ok {
			call = value
		}
		return nil
	}); err != nil {
		t.Fatalf("stream large tool payload: %v", err)
	}
	if call.CallID == "" || call.Name != "large" {
		t.Fatalf("unexpected tool call: %#v", call)
	}
	var payload struct {
		Payload string `json:"payload"`
	}
	if err := json.Unmarshal([]byte(call.Payload), &payload); err != nil {
		t.Fatalf("decode tool payload: %v", err)
	}
	if payload.Payload != large {
		t.Fatalf("large payload length = %d, want %d", len(payload.Payload), len(large))
	}
}

func TestChatStreamerToolNormalizers(t *testing.T) {
	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
			t.Errorf("decode request: %v", err)
		}
		writeJSON(t, w, map[string]any{"message": map[string]any{
			"role": "assistant",
			"tool_calls": []any{map[string]any{
				"id":       "call",
				"function": map[string]any{"name": "canonical", "arguments": map[string]any{"wire": "out"}},
			}},
		}, "done": true})
	}))
	defer server.Close()

	streamer := NewChatStreamerWithClient(server.Client(), mustURL(t, server.URL), "model")
	streamer.RegisterToolNormalizer("canonical", threads.ToolNormalizer{
		NormalizeSpec: func(spec threads.ToolSpec) (threads.ToolSpec, error) {
			spec.Description = "wire description"
			return spec, nil
		},
		NormalizeRequestToolCall: func(call threads.ToolCall) (threads.ToolCall, error) {
			call.Payload = `{"wire":"in"}`
			return call, nil
		},
		NormalizeResponseToolCall: func(call threads.ToolCall) (threads.ToolCall, error) {
			call.Payload = `{"canonical":"out"}`
			return call, nil
		},
	})
	req := threads.Req{
		Items: []threads.Item{threads.ToolCall{CallID: "old", Name: "canonical", Payload: `{"canonical":"in"}`}},
		Tools: threads.ToolOfferSnapshot{Offered: []threads.ToolSpec{{
			Name: "canonical", Payload: threads.ToolPayloadJSONSchema(gschema.Schema{Type: "object"}),
		}}},
	}
	var got threads.ToolCall
	if err := streamer.StreamReq(req, func(item threads.Item) error {
		got, _ = item.(threads.ToolCall)
		return nil
	}); err != nil {
		t.Fatalf("stream request: %v", err)
	}
	messages := requestBody["messages"].([]any)
	wireCall := messages[0].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	wireArgs := wireCall["function"].(map[string]any)["arguments"].(map[string]any)
	if wireArgs["wire"] != "in" {
		t.Fatalf("request call was not normalized: %#v", wireArgs)
	}
	tools := requestBody["tools"].([]any)
	if gotDescription := tools[0].(map[string]any)["function"].(map[string]any)["description"]; gotDescription != "wire description" {
		t.Fatalf("request spec was not normalized: %#v", gotDescription)
	}
	if got.Payload != `{"canonical":"out"}` {
		t.Fatalf("response call was not normalized: %#v", got)
	}
}

func TestChatStreamerRejectsUnsupportedAndInvalidInputs(t *testing.T) {
	base := mustURL(t, "http://127.0.0.1:1")
	streamer := NewChatStreamerWithClient(http.DefaultClient, base, "model")
	falseValue := false
	jsonTool := threads.ToolSpec{Name: "sum", Payload: threads.ToolPayloadJSONSchema(gschema.Schema{Type: "object"})}

	tests := []struct {
		name string
		req  threads.Req
		set  func()
		want string
	}{
		{name: "unsupported_item", req: threads.Req{Items: []threads.Item{unsupportedItem{}}}, want: "request item not supported"},
		{name: "reasoning_without_text", req: threads.Req{Items: []threads.Item{threads.ReasoningItem{Summary: "summary"}}}, want: "requires text"},
		{name: "invalid_historical_args", req: threads.Req{Items: []threads.Item{threads.ToolCall{Name: "sum", Payload: `[]`}}}, want: "must be a JSON object"},
		{name: "orphan_result", req: threads.Req{Items: []threads.Item{threads.ToolCallResult{CallID: "missing"}}}, want: "no preceding tool call"},
		{name: "unsupported_payload", req: threads.Req{Tools: threads.ToolOfferSnapshot{Offered: []threads.ToolSpec{{Name: "text", Payload: threads.ToolPayloadText()}}}}, want: "payload not supported"},
		{name: "missing_allowed_tool", req: threads.Req{Tools: threads.ToolOfferSnapshot{Offered: []threads.ToolSpec{jsonTool}, Allowed: []string{"missing"}}}, want: "not offered"},
		{name: "required", req: threads.Req{Tools: threads.ToolOfferSnapshot{Offered: []threads.ToolSpec{jsonTool}, Required: true}}, want: "does not support required"},
		{name: "disable_parallel", req: threads.Req{Tools: threads.ToolOfferSnapshot{Offered: []threads.ToolSpec{jsonTool}, Parallel: &falseValue}}, want: "cannot disable parallel"},
		{name: "invalid_think", req: threads.Req{}, set: func() { streamer.Think = "extreme" }, want: "think level"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			streamer.Think = nil
			if test.set != nil {
				test.set()
			}
			err := streamer.StreamReq(test.req, func(threads.Item) error { return nil })
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestChatStreamerErrorsAndCancellation(t *testing.T) {
	t.Run("http_error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			writeJSON(t, w, map[string]any{"error": "model not found"})
		}))
		defer server.Close()
		streamer := NewChatStreamerWithClient(server.Client(), mustURL(t, server.URL), "missing")
		err := streamer.StreamReq(threads.Req{}, func(threads.Item) error { return nil })
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound || apiErr.Message != "model not found" {
			t.Fatalf("unexpected API error: %#v", err)
		}
	})

	t.Run("stream_error", func(t *testing.T) {
		server := newNDJSONServer(t, map[string]any{"error": "generation failed"})
		defer server.Close()
		streamer := NewChatStreamerWithClient(server.Client(), mustURL(t, server.URL), "model")
		err := streamer.StreamReq(threads.Req{}, func(threads.Item) error { return nil })
		if err == nil || !strings.Contains(err.Error(), "generation failed") {
			t.Fatalf("stream error = %v", err)
		}
	})

	t.Run("truncated_stream", func(t *testing.T) {
		server := newNDJSONServer(t, map[string]any{"message": map[string]any{"content": "partial"}, "done": false})
		defer server.Close()
		streamer := NewChatStreamerWithClient(server.Client(), mustURL(t, server.URL), "model")
		err := streamer.StreamReq(threads.Req{}, func(threads.Item) error { return nil })
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("truncated stream error = %v", err)
		}
	})

	t.Run("progress_timeout", func(t *testing.T) {
		// Handler stalls until the client's request context is canceled; only
		// the watchdog can unblock this stream.
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/x-ndjson")
			writeJSON(t, w, map[string]any{"message": map[string]any{"content": "first"}, "done": false})
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			<-r.Context().Done()
		}))
		defer server.Close()
		streamer := NewChatStreamerWithClient(server.Client(), mustURL(t, server.URL), "model")
		short := 40 * time.Millisecond
		streamer.StreamProgressTimeout = &short
		done := make(chan error, 1)
		go func() {
			_, err := collectOllamaItems(streamer)
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil || !errors.Is(err, ErrStreamProgressTimeout) {
				t.Fatalf("progress timeout error = %v, want ErrStreamProgressTimeout", err)
			}
			if errors.Is(err, context.Canceled) {
				t.Fatalf("progress timeout error must not be a cancellation: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("stream did not fail before watchdog deadline")
		}
	})

	t.Run("progress_timeout_disabled", func(t *testing.T) {
		server := newNDJSONServer(t,
			map[string]any{"message": map[string]any{"content": "first"}, "done": false},
			map[string]any{"done": true},
		)
		defer server.Close()
		streamer := NewChatStreamerWithClient(server.Client(), mustURL(t, server.URL), "model")
		off := -time.Second
		streamer.StreamProgressTimeout = &off
		items, err := collectOllamaItems(streamer)
		if err != nil {
			t.Fatalf("stream with disabled watchdog: %v", err)
		}
		if len(items) == 0 {
			t.Fatal("expected items from stream with disabled watchdog")
		}
	})

	t.Run("progress_timeout_resolution", func(t *testing.T) {
		if d, enabled := (&ChatStreamer{}).streamProgressTimeout(); !enabled || d != DefaultStreamProgressTimeout {
			t.Fatalf("nil field: %v enabled=%v, want default enabled", d, enabled)
		}
		off := -time.Second
		if _, enabled := (&ChatStreamer{StreamProgressTimeout: &off}).streamProgressTimeout(); enabled {
			t.Fatal("negative value should disable the watchdog")
		}
		zero := time.Duration(0)
		if _, enabled := (&ChatStreamer{StreamProgressTimeout: &zero}).streamProgressTimeout(); enabled {
			t.Fatal("zero value should disable the watchdog")
		}
		d2 := 3 * time.Second
		if d, enabled := (&ChatStreamer{StreamProgressTimeout: &d2}).streamProgressTimeout(); !enabled || d != d2 {
			t.Fatalf("custom value: %v enabled=%v, want 3s enabled", d, enabled)
		}
	})

	t.Run("emit_error", func(t *testing.T) {
		server := newNDJSONServer(t, map[string]any{"message": map[string]any{"content": "x"}, "done": false})
		defer server.Close()
		streamer := NewChatStreamerWithClient(server.Client(), mustURL(t, server.URL), "model")
		sentinel := errors.New("emit failed")
		err := streamer.StreamReq(threads.Req{}, func(threads.Item) error { return sentinel })
		if !errors.Is(err, sentinel) {
			t.Fatalf("emit error = %v", err)
		}
	})

	t.Run("context", func(t *testing.T) {
		started := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			close(started)
			<-r.Context().Done()
		}))
		defer server.Close()
		streamer := NewChatStreamerWithClient(server.Client(), mustURL(t, server.URL), "model")
		ctx, cancel := context.WithCancel(context.Background())
		errCh := make(chan error, 1)
		go func() {
			errCh <- streamer.StreamReqContext(ctx, threads.Req{}, func(threads.Item) error { return nil })
		}()
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("request did not start")
		}
		cancel()
		select {
		case err := <-errCh:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation error = %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("canceled request did not return")
		}
	})
}

type ollamaContractHarness struct{}

func (ollamaContractHarness) Capabilities() streamertest.Capabilities {
	return streamertest.Capabilities{
		AssistantTextChunks: true,
		ParallelToolCalls:   true,
		AllowedTools:        true,
	}
}

func (ollamaContractHarness) Stream(t testing.TB, req threads.Req, events []streamertest.Event, emit func(threads.Item) error) (streamertest.ObservedRequest, error) {
	t.Helper()
	bodyCh := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		}
		bodyCh <- body
		for index, event := range events {
			if event.Err != "" {
				writeJSON(t, w, map[string]any{"error": event.Err})
				return
			}
			switch item := event.Item.(type) {
			case threads.AssistantText:
				writeJSON(t, w, map[string]any{"message": map[string]any{"role": "assistant", "content": string(item)}, "done": false})
			case threads.ToolCall:
				var args map[string]any
				if err := json.Unmarshal([]byte(item.Payload), &args); err != nil {
					t.Fatalf("decode contract tool payload: %v", err)
				}
				writeJSON(t, w, map[string]any{"message": map[string]any{
					"role": "assistant",
					"tool_calls": []any{map[string]any{
						"id":       item.CallID,
						"function": map[string]any{"index": index, "name": item.Name, "arguments": args},
					}},
				}, "done": false})
			case nil:
			default:
				t.Fatalf("unsupported contract event: %T", item)
			}
		}
		writeJSON(t, w, map[string]any{"message": map[string]any{"role": "assistant"}, "done": true})
	}))
	defer server.Close()

	streamer := NewChatStreamerWithClient(server.Client(), mustURL(t, server.URL), "test-model")
	streamer.AllowBestEffortToolControls = true
	err := streamer.StreamReq(req, emit)

	select {
	case body := <-bodyCh:
		return parseObservedRequest(t, req, body), err
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Ollama request")
		return streamertest.ObservedRequest{}, err
	}
}

func parseObservedRequest(t testing.TB, req threads.Req, body []byte) streamertest.ObservedRequest {
	t.Helper()
	var raw struct {
		Messages []message `json:"messages"`
		Tools    []tool    `json:"tools"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decode observed request: %v", err)
	}
	out := streamertest.ObservedRequest{Parallel: req.Tools.Parallel}
	for _, msg := range raw.Messages {
		switch msg.Role {
		case "system":
			out.Instruction += msg.Content
		case "user":
			out.Items = append(out.Items, streamertest.ObservedInputItem{Kind: "user_text", Text: msg.Content})
		case "assistant":
			if msg.Content != "" {
				out.Items = append(out.Items, streamertest.ObservedInputItem{Kind: "assistant_text", Text: msg.Content})
			}
			for _, call := range msg.ToolCalls {
				out.Items = append(out.Items, streamertest.ObservedInputItem{
					Kind:    "tool_call",
					CallID:  call.ID,
					Name:    call.Function.Name,
					Payload: string(call.Function.Arguments),
				})
			}
		case "tool":
			out.Items = append(out.Items, streamertest.ObservedInputItem{Kind: "tool_result", CallID: msg.ToolCallID, Output: msg.Content})
		}
	}
	for _, item := range raw.Tools {
		var schema map[string]any
		if err := json.Unmarshal(item.Function.Parameters, &schema); err != nil {
			t.Fatalf("decode observed schema: %v", err)
		}
		out.Tools = append(out.Tools, streamertest.ObservedTool{
			Kind:        item.Type,
			Name:        item.Function.Name,
			Description: item.Function.Description,
			SchemaType:  stringValue(schema["type"]),
		})
	}
	// Disabling all tools is represented by omitting them from Ollama's native
	// request. The contract's observed catalog still describes the input offer;
	// ToolChoice below records that none reached the model.
	if req.Tools.Allowed != nil && len(req.Tools.Allowed) == 0 {
		for _, spec := range req.Tools.Offered {
			schema, _ := spec.Payload.(threads.ToolPayloadJSONSchema)
			out.Tools = append(out.Tools, streamertest.ObservedTool{
				Kind:        "function",
				Name:        spec.Name,
				Description: spec.Description,
				SchemaType:  schema.Type,
			})
		}
	}
	switch {
	case req.Tools.Allowed != nil && len(req.Tools.Allowed) == 0:
		out.ToolChoice.Mode = "none"
	case req.Tools.Allowed != nil:
		out.ToolChoice.Mode = "allowed"
		if req.Tools.Required {
			out.ToolChoice.Mode = "required"
		}
		for _, name := range req.Tools.Allowed {
			out.ToolChoice.Allowed = append(out.ToolChoice.Allowed, streamertest.ObservedAllowedTool{Kind: "function", Name: name})
		}
	case req.Tools.Required:
		out.ToolChoice.Mode = "required"
		for _, spec := range req.Tools.Offered {
			out.ToolChoice.Allowed = append(out.ToolChoice.Allowed, streamertest.ObservedAllowedTool{Kind: "function", Name: spec.Name})
		}
	}
	return out
}

func newNDJSONServer(t testing.TB, chunks ...map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		for _, chunk := range chunks {
			writeJSON(t, w, chunk)
		}
	}))
}

func collectOllamaItems(streamer *ChatStreamer) ([]threads.Item, error) {
	var items []threads.Item
	err := streamer.StreamReq(threads.Req{}, func(item threads.Item) error {
		items = append(items, item)
		return nil
	})
	return items, err
}

func writeJSON(t testing.TB, w io.Writer, value any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encode JSON: %v", err)
	}
}

func mustURL(t testing.TB, raw string) *url.URL {
	t.Helper()
	value, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}
	return value
}

func stringValue(value any) string {
	out, _ := value.(string)
	return out
}

type unsupportedItem struct{}

func (unsupportedItem) Emit() bool { return true }
