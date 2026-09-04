package main

import (
	"testing"

	googlegenaiwrap "github.com/mackross/agentloom/llms/googlegenai"
)

func TestNewStreamerForModelUsesGoogleForGeminiModels(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "test-key")

	streamer, model := newStreamerForModel(googlegenaiwrap.DefaultModel)
	if _, ok := streamer.(*googlegenaiwrap.GenerateContentStreamer); !ok {
		t.Fatalf("expected Google GenAI streamer, got %T", streamer)
	}
	if model != googlegenaiwrap.DefaultModel {
		t.Fatalf("model = %q, want %q", model, googlegenaiwrap.DefaultModel)
	}
	if !hasProviderAPIKey(model) {
		t.Fatal("expected Gemini API key to be detected")
	}
	if got := requiredAPIKeyLabel(model); got != "GEMINI_API_KEY or GOOGLE_API_KEY" {
		t.Fatalf("API key label = %q", got)
	}
}
