package openai

import (
	"testing"

	cacheopenai "github.com/mackross/agentloom/llms/cache/openai"
	"github.com/mackross/agentloom/threads"
	"github.com/openai/openai-go/v3/responses"
)

func TestResponseParamsUsesLatestOpenAICacheMetadata(t *testing.T) {
	s := NewResponsesStreamer("gpt-test")
	params, err := s.responseParams(threads.Req{
		Items: []threads.Item{threads.UserText("a"), threads.UserText("b")},
		ItemMeta: []map[string]any{
			cacheopenai.PromptCacheKey("old"),
			mergeMaps(cacheopenai.PromptCacheKey("new"), cacheopenai.PromptCacheRetention(cacheopenai.Retention24h)),
		},
	})
	if err != nil {
		t.Fatalf("responseParams: %v", err)
	}
	if params.PromptCacheKey.Value != "new" {
		t.Fatalf("PromptCacheKey = %q, want new", params.PromptCacheKey.Value)
	}
	if params.PromptCacheRetention != responses.ResponseNewParamsPromptCacheRetention24h {
		t.Fatalf("PromptCacheRetention = %q, want 24h", params.PromptCacheRetention)
	}
}

func mergeMaps(a, b map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}
