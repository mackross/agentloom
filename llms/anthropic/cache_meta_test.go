package anthropic

import (
	"testing"

	cacheanthropic "github.com/mackross/agentloom/llms/cache/anthropic"
	"github.com/mackross/agentloom/threads"
)

func TestRequestMessagesUsesAnthropicCacheControlMetadata(t *testing.T) {
	msgs, err := requestMessages(threads.Req{
		Items:    []threads.Item{threads.UserText("cached")},
		ItemMeta: []map[string]any{cacheanthropic.Ephemeral1h()},
	})
	if err != nil {
		t.Fatalf("requestMessages: %v", err)
	}
	block := msgs[0].Content[0].OfText
	if block == nil {
		t.Fatalf("first content block is not text: %#v", msgs[0].Content[0])
	}
	if block.CacheControl.Type != "ephemeral" {
		t.Fatalf("cache_control type = %q, want ephemeral", block.CacheControl.Type)
	}
	if block.CacheControl.TTL != "1h" {
		t.Fatalf("cache_control ttl = %q, want 1h", block.CacheControl.TTL)
	}
}
