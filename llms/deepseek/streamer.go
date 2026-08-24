package deepseek

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	openaiapi "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	openaiadapter "github.com/mackross/agentloom/llms/openai"
	"github.com/mackross/agentloom/threads"
)

const BaseURL = "https://api.deepseek.com"
const DefaultModel = "deepseek-v4-flash"
const reasoningProvider = "deepseek.responses"

type ResponsesStreamer struct {
	*openaiadapter.ResponsesStreamer
}

func NewResponsesStreamer(model string) *ResponsesStreamer {
	client := openaiapi.NewClient(option.WithBaseURL(BaseURL), option.WithAPIKey(strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY"))))
	return NewResponsesStreamerWithClient(client, model)
}

func NewResponsesStreamerWithClient(client openaiapi.Client, model string) *ResponsesStreamer {
	if strings.TrimSpace(model) == "" {
		model = DefaultModel
	}
	streamer := openaiadapter.NewResponsesStreamerWithClient(client, model)
	streamer.ReasoningProvider = reasoningProvider
	streamer.Transport = openaiadapter.ResponsesTransportSSE
	streamer.DisablePreviousResponseID = true
	return &ResponsesStreamer{ResponsesStreamer: streamer}
}

func (*ResponsesStreamer) Capabilities() threads.StreamerCapabilities {
	return threads.StreamerCapabilities{AssistantPrefix: true, Reasoning: threads.ReasoningForCurrentTurn(reasoningProvider)}
}

func (s *ResponsesStreamer) StreamReq(req threads.Req, emit func(threads.Item) error) error {
	return s.StreamReqContext(context.Background(), req, emit)
}

func (s *ResponsesStreamer) StreamReqContext(ctx context.Context, req threads.Req, emit func(threads.Item) error) error {
	req.Items = append([]threads.Item(nil), req.Items...)
	for i, item := range req.Items {
		reasoning, ok := item.(threads.ReasoningItem)
		if !ok || reasoning.Provider != reasoningProvider {
			continue
		}
		if reasoning.ID == "" || reasoning.Text == "" {
			return fmt.Errorf("deepseek reasoning input requires id and text")
		}
		reasoning.Opaque, _ = json.Marshal(map[string]any{
			"id": reasoning.ID, "type": "reasoning",
			"content": []any{map[string]any{"type": "reasoning_text", "text": reasoning.Text}},
		})
		req.Items[i] = reasoning
	}
	return s.ResponsesStreamer.StreamReqContext(ctx, req, func(item threads.Item) error {
		reasoning, ok := item.(threads.ReasoningItem)
		if !ok || reasoning.Provider != reasoningProvider {
			return emit(item)
		}
		if reasoning.ID == "" || reasoning.Text == "" {
			return fmt.Errorf("deepseek reasoning output requires id and text")
		}
		reasoning.Opaque = nil
		return emit(reasoning)
	})
}
