package cerebras

import (
	"testing"

	"github.com/mackross/agentloom/llms/internal/streamerutil"
	"github.com/mackross/agentloom/threads"
)

func TestPromptCacheKeyMetadataLatestWins(t *testing.T) {
	req := threads.Req{ItemMeta: []map[string]any{
		PromptCacheKey("old"),
		ClearPromptCacheKey(),
		PromptCacheKey("new"),
	}}
	got, ok := streamerutil.LastStringMetadata(req, PromptCacheKeyKey)
	if !ok || got != "new" {
		t.Fatalf("latest prompt cache key = %q, %v; want new, true", got, ok)
	}
}

func TestPromptCacheKeyMetadataClear(t *testing.T) {
	req := threads.Req{ItemMeta: []map[string]any{
		PromptCacheKey("old"),
		ClearPromptCacheKey(),
	}}
	if got, ok := streamerutil.LastStringMetadata(req, PromptCacheKeyKey); ok {
		t.Fatalf("cleared prompt cache key = %q, true; want false", got)
	}
}
