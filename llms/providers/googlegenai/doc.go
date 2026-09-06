// Package googlegenai adapts the Google GenAI generateContent API to
// threads.LLMStreamer.
//
// An empty model name selects Gemini 3.8 Flash. The default client reads
// GEMINI_API_KEY or GOOGLE_API_KEY. Gemini 3.8 Flash uses the service's medium
// thinking level by default; callers may explicitly select low, medium, or high
// with GenerateContentStreamer.Config. Minimal thinking and multiple candidates
// are not supported by Gemini 3.8 Flash and are rejected before a request is
// sent.
//
// Gemini 3.8 requests retain Google thought signatures across turns. This
// improves long-running tool workflows but can increase input token usage.
package googlegenai
