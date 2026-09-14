// Package openai adds metadata that changes OpenAI Responses request params.
// Queue metadata after any request item; the streamer uses the last non-cleared
// value to set prompt_cache_key and prompt_cache_retention on the API request.
package openai

import "github.com/mackross/agentloom/threads"

const (
	PromptCacheKeyKey       = "cache/openai/prompt_cache_key"
	PromptCacheRetentionKey = "cache/openai/prompt_cache_retention"
	PromptCacheTTLKey       = "cache/openai/prompt_cache_ttl"

	RetentionInMemory = "in_memory"
	Retention24h      = "24h"

	// TTL30m is the only prompt_cache_options.ttl value OpenAI supports, and it
	// is also the default. GPT-5.6 and later (including gpt-6-astra) control
	// cache lifetime with ttl; prompt_cache_retention is the pre-5.6 knob.
	TTL30m = "30m"
)

// PromptCacheKey sets prompt_cache_key, improving routing/cache locality.
func PromptCacheKey(key string) threads.PatchItemMetadata {
	return threads.PatchItemMetadata{Metadata: map[string]any{PromptCacheKeyKey: key}}
}

// ClearPromptCacheKey clears a previously set prompt_cache_key.
func ClearPromptCacheKey() threads.PatchItemMetadata {
	return threads.PatchItemMetadata{Metadata: map[string]any{PromptCacheKeyKey: false}}
}

// PromptCacheRetention sets prompt_cache_retention, e.g. Retention24h.
func PromptCacheRetention(retention string) threads.PatchItemMetadata {
	return threads.PatchItemMetadata{Metadata: map[string]any{PromptCacheRetentionKey: retention}}
}

// ClearPromptCacheRetention clears a previously set prompt_cache_retention.
func ClearPromptCacheRetention() threads.PatchItemMetadata {
	return threads.PatchItemMetadata{Metadata: map[string]any{PromptCacheRetentionKey: false}}
}

// PromptCacheTTL sets prompt_cache_options.ttl, e.g. TTL30m. GPT-5.6 and later
// models reject prompt_cache_retention and use ttl instead.
func PromptCacheTTL(ttl string) threads.PatchItemMetadata {
	return threads.PatchItemMetadata{Metadata: map[string]any{PromptCacheTTLKey: ttl}}
}

// ClearPromptCacheTTL clears a previously set prompt_cache_options.ttl.
func ClearPromptCacheTTL() threads.PatchItemMetadata {
	return threads.PatchItemMetadata{Metadata: map[string]any{PromptCacheTTLKey: false}}
}
