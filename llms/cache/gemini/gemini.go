// Package gemini adds metadata that changes Google GenAI request params.
// Gemini context caching uses a cached content resource name; queue metadata
// after any request item and the googlegenai streamer uses the last non-cleared
// value as GenerateContentConfig.CachedContent.
package gemini

import "github.com/mackross/agentloom/threads"

// CachedContentKey is read by the googlegenai streamer and sent as cachedContent.
const CachedContentKey = "cache/gemini/cached_content"

// CachedContent sets the cached content resource name for subsequent requests.
func CachedContent(name string) threads.PreviousItemMetadata {
	return threads.PreviousItemMetadata{CachedContentKey: name}
}

// ClearCachedContent clears a previously set cached content resource name.
func ClearCachedContent() threads.PreviousItemMetadata {
	return threads.PreviousItemMetadata{CachedContentKey: false}
}
