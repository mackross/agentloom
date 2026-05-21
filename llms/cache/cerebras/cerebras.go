// Package cerebras adds metadata that changes Cerebras prompt-cache routing.
// Cerebras prompt caching is automatic; prompt_cache_key is a routing hint.
package cerebras

import "github.com/mackross/agentloom/threads"

const PromptCacheKeyKey = "cache/cerebras/prompt_cache_key"

// PromptCacheKey sets prompt_cache_key, improving Cerebras prompt-cache routing locality.
func PromptCacheKey(key string) threads.PreviousItemMetadata {
	return threads.PreviousItemMetadata{PromptCacheKeyKey: key}
}

// ClearPromptCacheKey clears a previously set prompt_cache_key.
func ClearPromptCacheKey() threads.PreviousItemMetadata {
	return threads.PreviousItemMetadata{PromptCacheKeyKey: false}
}
