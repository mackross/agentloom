// Package anthropic adds metadata that changes Anthropic message serialization.
//
// Anthropic caches complete prompt prefixes: tools, system, and messages up to
// a cache breakpoint. This package is for explicit breakpoints: queue a text
// item, then Ephemeral5m or Ephemeral1h, and the streamer writes cache_control
// on that text block. Anthropic also has request-level automatic caching; use
// MessagesStreamer.UseAutoCache when you want Claude to place the breakpoint at
// the last cacheable block instead of marking individual items here.
package anthropic

import "github.com/mackross/agentloom/threads"

// CacheControlKey is read by the Anthropic streamer and emitted as cache_control.
const CacheControlKey = "cache/anthropic/cache_control"

const (
	TTL5m = "5m"
	TTL1h = "1h"
)

// Ephemeral5m marks the previous text item as a 5-minute cache breakpoint.
func Ephemeral5m() threads.PreviousItemMetadata {
	return threads.PreviousItemMetadata{CacheControlKey: map[string]any{"type": "ephemeral", "ttl": TTL5m}}
}

// Ephemeral1h marks the previous text item as a 1-hour cache breakpoint.
func Ephemeral1h() threads.PreviousItemMetadata {
	return threads.PreviousItemMetadata{CacheControlKey: map[string]any{"type": "ephemeral", "ttl": TTL1h}}
}

// Clear removes Anthropic cache_control for later latest-wins metadata scans.
func Clear() threads.PreviousItemMetadata {
	return threads.PreviousItemMetadata{CacheControlKey: false}
}
