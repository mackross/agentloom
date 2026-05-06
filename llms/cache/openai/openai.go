// Package openai adds metadata that changes OpenAI Responses request params.
// Queue metadata after any request item; the streamer uses the last non-cleared
// value to set prompt_cache_key and prompt_cache_retention on the API request.
package openai

import "github.com/mackross/agentloom/threads"

const (
	PromptCacheKeyKey       = "cache/openai/prompt_cache_key"
	PromptCacheRetentionKey = "cache/openai/prompt_cache_retention"

	RetentionInMemory = "in-memory"
	Retention24h      = "24h"
)

// PromptCacheKey sets prompt_cache_key, improving routing/cache locality.
func PromptCacheKey(key string) threads.PreviousItemMetadata {
	return threads.PreviousItemMetadata{PromptCacheKeyKey: key}
}

// ClearPromptCacheKey clears a previously set prompt_cache_key.
func ClearPromptCacheKey() threads.PreviousItemMetadata {
	return threads.PreviousItemMetadata{PromptCacheKeyKey: false}
}

// PromptCacheRetention sets prompt_cache_retention, e.g. Retention24h.
func PromptCacheRetention(retention string) threads.PreviousItemMetadata {
	return threads.PreviousItemMetadata{PromptCacheRetentionKey: retention}
}

// ClearPromptCacheRetention clears a previously set prompt_cache_retention.
func ClearPromptCacheRetention() threads.PreviousItemMetadata {
	return threads.PreviousItemMetadata{PromptCacheRetentionKey: false}
}
