package xai

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	openaiapi "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/mackross/agentloom/threads"
)

func TestNewResponsesStreamerDefaults(t *testing.T) {
	s := NewResponsesStreamerWithClient(openaiapi.Client{}, "")
	if s.Store == nil || !*s.Store {
		t.Fatalf("Store = %v, want true", s.Store)
	}
	if s.StreamToolCalls == nil || !*s.StreamToolCalls {
		t.Fatalf("StreamToolCalls = %v, want true", s.StreamToolCalls)
	}
	if s.DisablePreviousResponseID {
		t.Fatal("expected previous_response_id enabled by default")
	}
}

func TestResponsesStreamerSendsStoreTrueAndSSE(t *testing.T) {
	bodyCh := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/responses" {
			t.Errorf("path = %s, want /responses", r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		bodyCh <- append([]byte(nil), body...)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, ""+
			"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_test\",\"status\":\"in_progress\"}}\n\n"+
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\",\"item_id\":\"msg_1\",\"output_index\":0}\n\n"+
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"status\":\"completed\"}}\n\n"+
			"data: [DONE]\n\n")
	}))
	defer server.Close()

	client := openaiapi.NewClient(
		option.WithAPIKey("test"),
		option.WithBaseURL(server.URL),
		option.WithHTTPClient(server.Client()),
		option.WithMaxRetries(0),
	)
	streamer := NewResponsesStreamerWithClient(client, "grok-4.5")
	var got string
	if err := streamer.StreamReq(threads.Req{
		Items: []threads.Item{threads.UserText("hello")},
	}, func(item threads.Item) error {
		if text, ok := item.(threads.AssistantText); ok {
			got += string(text)
		}
		return nil
	}); err != nil {
		t.Fatalf("StreamReq: %v", err)
	}
	if got != "hi" {
		t.Fatalf("text = %q, want hi", got)
	}

	select {
	case body := <-bodyCh:
		var raw map[string]any
		if err := json.Unmarshal(body, &raw); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if raw["model"] != "grok-4.5" {
			t.Fatalf("model = %#v", raw["model"])
		}
		if raw["store"] != true {
			t.Fatalf("store = %#v, want true", raw["store"])
		}
		// previous_response_id is enabled by default, so stream_tool_calls must be
		// omitted to keep stored tool calls continuable on xAI.
		if _, ok := raw["stream_tool_calls"]; ok {
			t.Fatalf("stream_tool_calls = %#v, want omitted when previous_response_id enabled", raw["stream_tool_calls"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for request body")
	}
}

func TestResponsesStreamerStreamToolCallsOnlyWhenPreviousResponseIDDisabled(t *testing.T) {
	bodyCh := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodyCh <- append([]byte(nil), body...)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, ""+
			"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_stc\",\"status\":\"in_progress\"}}\n\n"+
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_stc\",\"status\":\"completed\"}}\n\n"+
			"data: [DONE]\n\n")
	}))
	defer server.Close()

	client := openaiapi.NewClient(
		option.WithAPIKey("test"),
		option.WithBaseURL(server.URL),
		option.WithHTTPClient(server.Client()),
		option.WithMaxRetries(0),
	)
	streamer := NewResponsesStreamerWithClient(client, "grok-4.5")
	streamer.DisablePreviousResponseID = true
	if err := streamer.StreamReq(threads.Req{
		Items: []threads.Item{threads.UserText("hello")},
	}, func(threads.Item) error { return nil }); err != nil {
		t.Fatalf("StreamReq: %v", err)
	}
	select {
	case body := <-bodyCh:
		var raw map[string]any
		if err := json.Unmarshal(body, &raw); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if raw["stream_tool_calls"] != true {
			t.Fatalf("stream_tool_calls = %#v, want true when previous_response_id disabled", raw["stream_tool_calls"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for request body")
	}
}

func TestResponsesStreamerDisablePreviousResponseIDSendsStoreTrueStill(t *testing.T) {
	// Default store stays true; callers can set Store=false when disabling continuation.
	bodyCh := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodyCh <- append([]byte(nil), body...)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\"}}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()

	client := openaiapi.NewClient(
		option.WithAPIKey("test"),
		option.WithBaseURL(server.URL),
		option.WithHTTPClient(server.Client()),
		option.WithMaxRetries(0),
	)
	streamer := NewResponsesStreamerWithClient(client, DefaultModel)
	streamer.DisablePreviousResponseID = true
	store := false
	streamer.Store = &store
	if err := streamer.StreamReq(threads.Req{Items: []threads.Item{threads.UserText("x")}}, func(threads.Item) error { return nil }); err != nil {
		t.Fatalf("StreamReq: %v", err)
	}
	select {
	case body := <-bodyCh:
		var raw map[string]any
		if err := json.Unmarshal(body, &raw); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if raw["store"] != false {
			t.Fatalf("store = %#v, want false", raw["store"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout")
	}
}
