package xai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mackross/agentloom/threads"
	openaiapi "github.com/mackross/openai-go/v3"
	"github.com/mackross/openai-go/v3/option"
)

func TestResponsesStreamIgnoresDataLessSSEKeepalives(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		_, _ = w.Write([]byte(
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"before\"}\n\n" +
				": keep-alive\n\n" + "\n" + "event: ping\r\n\r\n" +
				"data: {\"type\":\"response.output_text.delta\",\"delta\":\"after\"}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"status\":\"completed\",\"output\":[]}}\n\n" +
				"data: [DONE]\n\n"))
	}))
	defer server.Close()
	client := openaiapi.NewClient(option.WithBaseURL(server.URL+"/"), option.WithAPIKey("test"), option.WithMaxRetries(0))
	streamer := NewResponsesStreamerWithClient(client, "grok-4.7")
	var text strings.Builder
	err := streamer.StreamReqContext(context.Background(), threads.Req{Items: []threads.Item{threads.UserText("hello")}}, func(item threads.Item) error {
		if delta, ok := item.(threads.AssistantText); ok {
			text.WriteString(string(delta))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("keep-alive interrupted valid response: %v", err)
	}
	if text.String() != "beforeafter" {
		t.Fatalf("text = %q, want beforeafter", text.String())
	}
}

func TestResponsesStreamCancellationWhileWaitingAfterKeepalive(t *testing.T) {
	keepaliveSent := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(": ping\n\n"))
		w.(http.Flusher).Flush()
		close(keepaliveSent)
		<-req.Context().Done()
	}))
	defer server.Close()
	client := openaiapi.NewClient(option.WithBaseURL(server.URL+"/"), option.WithAPIKey("test"), option.WithMaxRetries(0))
	streamer := NewResponsesStreamerWithClient(client, "grok-4.7")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- streamer.StreamReqContext(ctx, threads.Req{}, func(threads.Item) error { return nil }) }()
	select {
	case <-keepaliveSent:
	case <-ctx.Done():
		t.Fatal("server never sent keepalive")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not stop after cancellation")
	}
}

func TestResponsesStreamStillReportsMalformedJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(": ping\n\ndata: {\n\n"))
	}))
	defer server.Close()
	client := openaiapi.NewClient(option.WithBaseURL(server.URL+"/"), option.WithAPIKey("test"), option.WithMaxRetries(0))
	streamer := NewResponsesStreamerWithClient(client, "grok-4.7")
	err := streamer.StreamReqContext(context.Background(), threads.Req{}, func(threads.Item) error { return nil })
	var syntax *json.SyntaxError
	if !errors.As(err, &syntax) || !strings.Contains(err.Error(), "xai Responses stream:") {
		t.Fatalf("malformed JSON was hidden or lost context: %v", err)
	}
}
