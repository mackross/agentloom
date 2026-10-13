package all_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	anthropicapi "github.com/anthropics/anthropic-sdk-go"
	anthropicoption "github.com/anthropics/anthropic-sdk-go/option"
	"github.com/mackross/agentloom/llms/providers/anthropic"
	"github.com/mackross/agentloom/llms/providers/cerebras"
	"github.com/mackross/agentloom/llms/providers/deepseek"
	"github.com/mackross/agentloom/llms/providers/fireworks"
	"github.com/mackross/agentloom/llms/providers/googlegenai"
	openaiadapter "github.com/mackross/agentloom/llms/providers/openai"
	"github.com/mackross/agentloom/llms/providers/xai"
	"github.com/mackross/agentloom/threads"
	openaiapi "github.com/mackross/openai-go/v3"
	"github.com/mackross/openai-go/v3/option"
	"google.golang.org/genai"
)

var keepaliveProviders = []string{"openai_sse", "deepseek", "cerebras", "fireworks", "xai", "google", "anthropic"}

type keepaliveStreamer interface {
	StreamReqContext(context.Context, threads.Req, func(threads.Item) error) error
}

func newKeepaliveStreamer(t testing.TB, provider string, server *httptest.Server) keepaliveStreamer {
	t.Helper()
	client := openaiapi.NewClient(option.WithAPIKey("test"), option.WithBaseURL(server.URL+"/"), option.WithMaxRetries(0))
	switch provider {
	case "openai_sse":
		s := openaiadapter.NewResponsesStreamerWithClient(client, "test-model")
		s.Transport = openaiadapter.ResponsesTransportSSE
		return s
	case "deepseek":
		return deepseek.NewResponsesStreamerWithClient(client, "deepseek-v4-flash")
	case "cerebras":
		return cerebras.NewChatCompletionsStreamerWithClient(client, "test-model")
	case "fireworks":
		return fireworks.NewChatCompletionsStreamerWithClient(client, "test-model")
	case "xai":
		return xai.NewResponsesStreamerWithClient(client, "grok-4.7")
	case "anthropic":
		c := anthropicapi.NewClient(anthropicoption.WithAPIKey("test"), anthropicoption.WithBaseURL(server.URL), anthropicoption.WithMaxRetries(0))
		return anthropic.NewMessagesStreamerWithClient(c, "test-model")
	case "google":
		c, err := genai.NewClient(t.Context(), &genai.ClientConfig{APIKey: "test", Backend: genai.BackendGeminiAPI, HTTPClient: server.Client(), HTTPOptions: genai.HTTPOptions{BaseURL: server.URL, APIVersion: "v1beta"}})
		if err != nil {
			t.Fatal(err)
		}
		return googlegenai.NewGenerateContentStreamerWithClient(c, "gemini-3.1-flash-lite")
	default:
		t.Fatalf("unknown provider %q", provider)
		return nil
	}
}

func keepaliveTranscript(provider, frame string) string {
	switch provider {
	case "cerebras", "fireworks":
		return "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"before\"}}]}\n\n" + frame +
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"after\"},\"finish_reason\":\"stop\"}]}\n\n" + "data: [DONE]\n\n"
	case "anthropic":
		return "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_test\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"test-model\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n" +
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"before\"}}\n\n" + frame +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"after\"}}\n\n" +
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" + "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	case "google":
		return "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"before\"}]}}]}\n\n" + frame +
			"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"after\"}]},\"finishReason\":\"STOP\"}]}\n\n"
	default:
		return "data: {\"type\":\"response.output_text.delta\",\"delta\":\"before\"}\n\n" + frame +
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"after\"}\n\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"status\":\"completed\",\"output\":[]}}\n\n" + "data: [DONE]\n\n"
	}
}

func TestSSEKeepaliveHandlingAcrossProviders(t *testing.T) {
	for _, provider := range keepaliveProviders {
		t.Run(provider, func(t *testing.T) {
			for _, tc := range []struct{ name, frame string }{
				{"no_keepalive", ""},
				{"blank", "\n"},
				{"comment", ": keepalive\n\n"},
				{"metadata", "event: ping\nid: 1\n\n"},
				{"crlf", ": keepalive\r\n\r\n"},
				{"combined", ": keepalive\n\n\nevent: ping\r\n\r\n"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
						_, _ = w.Write([]byte(keepaliveTranscript(provider, tc.frame)))
					}))
					defer server.Close()
					streamer := newKeepaliveStreamer(t, provider, server)
					var text strings.Builder
					err := streamer.StreamReqContext(t.Context(), threads.Req{Items: []threads.Item{threads.UserText("hello")}}, func(item threads.Item) error {
						if delta, ok := item.(threads.AssistantText); ok {
							text.WriteString(string(delta))
						}
						return nil
					})
					if err != nil || text.String() != "beforeafter" {
						t.Fatalf("keepalive interrupted response: text=%q err=%v", text.String(), err)
					}
				})
			}
		})
	}
}

func TestSSEKeepaliveHandlingPreservesDataErrorsAcrossProviders(t *testing.T) {
	for _, provider := range keepaliveProviders {
		t.Run(provider, func(t *testing.T) {
			for _, payload := range []string{"{", ""} {
				t.Run("data="+payload, func(t *testing.T) {
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						w.Header().Set("Content-Type", "text/event-stream")
						prefix := ": keepalive\n\n"
						if provider == "anthropic" {
							prefix += "event: content_block_delta\n"
						}
						_, _ = w.Write([]byte(prefix + "data: " + payload + "\n\n"))
					}))
					defer server.Close()
					streamer := newKeepaliveStreamer(t, provider, server)
					err := streamer.StreamReqContext(t.Context(), threads.Req{Items: []threads.Item{threads.UserText("hello")}}, func(threads.Item) error { return nil })
					var syntax *json.SyntaxError
					if !errors.As(err, &syntax) {
						t.Fatalf("data error hidden or lost cause: %v", err)
					}
				})
			}
		})
	}
}

func TestSSEKeepaliveHandlingCancellationAcrossProviders(t *testing.T) {
	for _, provider := range keepaliveProviders {
		t.Run(provider, func(t *testing.T) {
			keepaliveSent := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(": keepalive\n\n"))
				w.(http.Flusher).Flush()
				close(keepaliveSent)
				<-req.Context().Done()
			}))
			defer server.Close()
			streamer := newKeepaliveStreamer(t, provider, server)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- streamer.StreamReqContext(ctx, threads.Req{Items: []threads.Item{threads.UserText("hello")}}, func(threads.Item) error { return nil })
			}()
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
				t.Fatal("keepalive filter blocked cancellation")
			}
		})
	}
}
