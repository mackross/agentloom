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
func SessionAffinity(key string) threads.PreviousItemMetadata {
	return threads.PreviousItemMetadata{SessionAffinityKey: key}
}

// ClearSessionAffinity clears a previously set session-affinity value.
func ClearSessionAffinity() threads.PreviousItemMetadata {
	return threads.PreviousItemMetadata{SessionAffinityKey: false}
}

// PromptCacheIsolationKey sets prompt_cache_isolation_key to separate caches.
func PromptCacheIsolationKey(key string) threads.PreviousItemMetadata {
	return threads.PreviousItemMetadata{PromptCacheIsolationKeyKey: key}
}

// ClearPromptCacheIsolationKey clears a previously set isolation key.
func ClearPromptCacheIsolationKey() threads.PreviousItemMetadata {
	return threads.PreviousItemMetadata{PromptCacheIsolationKeyKey: false}
}
