package main

import (
	"reflect"
	"strings"
	"testing"

	anthropicwrap "github.com/mackross/agentloom/llms/anthropic"
	fireworkswrap "github.com/mackross/agentloom/llms/fireworks"
	openaiwrap "github.com/mackross/agentloom/llms/openai"
	"github.com/mackross/agentloom/threads"
)

func TestEvalJavaScriptReturnsResultAndLogs(t *testing.T) {
	got := evalJavaScript(`console.log("hello", 7); ({sum: 2 + 3})`)

	want := map[string]any{
		"logs":   []string{"hello 7"},
		"result": map[string]any{"sum": int64(5)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected eval output\nwant: %#v\ngot:  %#v", want, got)
	}
}

func TestEvalJavaScriptReturnsErrors(t *testing.T) {
	got := evalJavaScript(`throw new Error("boom")`)
	if got["error"] == nil {
		t.Fatalf("expected error output, got %#v", got)
	}
}

func TestConfiguredModelPrefersGenericModelEnv(t *testing.T) {
	t.Setenv("MODEL", "claude-sonnet-4-6")
	t.Setenv("OPENAI_MODEL", "gpt-5.2")
	t.Setenv("ANTHROPIC_MODEL", "claude-opus-4-6")

	if got := configuredModel(); got != "claude-sonnet-4-6" {
		t.Fatalf("unexpected configured model: %q", got)
	}
}

func TestNewStreamerForModelDefaultsToOpenAI(t *testing.T) {
	streamer, model := newStreamerForModel("")

	if _, ok := streamer.(*openaiwrap.ResponsesStreamer); !ok {
		t.Fatalf("expected openai streamer, got %T", streamer)
	}
	if model != openaiwrap.DefaultModel {
		t.Fatalf("unexpected default model: %q", model)
	}
	if got := requiredAPIKeyLabel(""); got != "OPENAI_API_KEY" {
		t.Fatalf("unexpected api key env: %q", got)
	}
}

func TestNewStreamerForModelUsesAnthropicForClaudeModels(t *testing.T) {
	const model = "claude-sonnet-4-6"

	streamer, gotModel := newStreamerForModel(model)

	if _, ok := streamer.(*anthropicwrap.MessagesStreamer); !ok {
		t.Fatalf("expected anthropic streamer, got %T", streamer)
	}
	if gotModel != model {
		t.Fatalf("unexpected resolved model: %q", gotModel)
	}
	if got := requiredAPIKeyLabel(model); got != "ANTHROPIC_API_KEY" {
		t.Fatalf("unexpected api key env: %q", got)
	}
}

func TestNewStreamerForModelUsesFireworksForFireworksModels(t *testing.T) {
	const model = fireworkswrap.Kimi25Model

	streamer, gotModel := newStreamerForModel(model)

	if _, ok := streamer.(*fireworkswrap.ChatCompletionsStreamer); !ok {
		t.Fatalf("expected fireworks streamer, got %T", streamer)
	}
	if gotModel != model {
		t.Fatalf("unexpected resolved model: %q", gotModel)
	}
	if got := requiredAPIKeyLabel(model); got != "FIREWORKS_API_KEY" {
		t.Fatalf("unexpected api key env: %q", got)
	}
}

func TestSwitchModelIfIdleSwapsExecutor(t *testing.T) {
	thread := threads.New()
	t.Setenv("ANTHROPIC_API_KEY", "test-key")

	executor, resolvedModel, err := switchModelIfIdle(thread, "claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("switch model: %v", err)
	}
	if executor == nil {
		t.Fatal("expected executor")
	}
	if resolvedModel != "claude-sonnet-4-6" {
		t.Fatalf("unexpected resolved model: %q", resolvedModel)
	}
	if got := thread.State(); got != threads.StateIdle {
		t.Fatalf("expected idle state after switch, got %q", got)
	}
}

func TestSwitchModelIfIdleRejectsNonIdleThread(t *testing.T) {
	thread := threads.New()
	thread.QueueItem(threads.SendItem{})
	t.Setenv("ANTHROPIC_API_KEY", "test-key")

	_, _, err := switchModelIfIdle(thread, "claude-sonnet-4-6")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), string(threads.StateConstructLLMRequest)) {
		t.Fatalf("expected non-idle state in error, got %v", err)
	}
}

func TestSwitchModelIfIdleRequiresProviderKey(t *testing.T) {
	thread := threads.New()
	t.Setenv("ANTHROPIC_API_KEY", "")

	_, _, err := switchModelIfIdle(thread, "claude-sonnet-4-6")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Fatalf("expected missing key in error, got %v", err)
	}
}

func TestHasProviderAPIKeyAcceptsEitherFireworksEnv(t *testing.T) {
	model := fireworkswrap.Kimi25Model
	t.Setenv("FIREWORKS_API_KEY", "")
	t.Setenv("FIREWORKS_AI_API_KEY", "legacy-key")

	if !hasProviderAPIKey(model) {
		t.Fatal("expected fireworks key to be detected")
	}
}
