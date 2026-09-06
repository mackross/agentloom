// Package ollama adapts Ollama's native chat API to threads.Streamer.
//
// The adapter uses POST /api/chat rather than Ollama's OpenAI compatibility
// layer. It supports streamed assistant text, thinking, JSON-schema function
// tools, and Ollama runtime options without importing the Ollama server module.
package ollama
