// Package fireworks adds metadata that changes Fireworks request routing.
// Fireworks caching is automatic on exact prefixes; these latest-wins metadata
// values set API routing/isolation hints rather than per-block cache controls.
package fireworks

import "github.com/mackross/agentloom/threads"

const (
	SessionAffinityKey         = "cache/fireworks/session_affinity"
	PromptCacheIsolationKeyKey = "cache/fireworks/prompt_cache_isolation_key"
)

// SessionAffinity sets x-session-affinity to improve same-session cache hits.
func SessionAffinity(key string) threads.PatchItemMetadata {
	return threads.PatchItemMetadata{Metadata: map[string]any{SessionAffinityKey: key}}
}

// ClearSessionAffinity clears a previously set session-affinity value.
func ClearSessionAffinity() threads.PatchItemMetadata {
	return threads.PatchItemMetadata{Metadata: map[string]any{SessionAffinityKey: false}}
}

// PromptCacheIsolationKey sets prompt_cache_isolation_key to separate caches.
func PromptCacheIsolationKey(key string) threads.PatchItemMetadata {
	return threads.PatchItemMetadata{Metadata: map[string]any{PromptCacheIsolationKeyKey: key}}
}

// ClearPromptCacheIsolationKey clears a previously set isolation key.
func ClearPromptCacheIsolationKey() threads.PatchItemMetadata {
	return threads.PatchItemMetadata{Metadata: map[string]any{PromptCacheIsolationKeyKey: false}}
}
