package main

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/mackross/agentloom/harness"
	googlegenaiwrap "github.com/mackross/agentloom/llms/providers/googlegenai"
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

func TestConfiguredModelReadsGoogleModelEnv(t *testing.T) {
	t.Setenv("MODEL", "")
	t.Setenv("OPENAI_MODEL", "")
	t.Setenv("ANTHROPIC_MODEL", "")
	t.Setenv("FIREWORKS_MODEL", "")
	t.Setenv("GOOGLE_GENAI_MODEL", googlegenaiwrap.DefaultModel)

	if got := configuredModel(); got != googlegenaiwrap.DefaultModel {
		t.Fatalf("configured model = %q, want %q", got, googlegenaiwrap.DefaultModel)
	}
}

func testSession(t *testing.T, name string) *harness.Session {
	t.Helper()
	h, err := harness.Open(context.Background(), harness.Options{Store: harness.Memory(harness.Settings{})})
	if err != nil {
		t.Fatal(err)
	}
	session, err := h.Session(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func TestSwitchModelIfIdleSwapsExecutor(t *testing.T) {
	thread := threads.New()
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	session := testSession(t, "")

	executor, err := switchModelIfIdle(thread, session, "claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("switch model: %v", err)
	}
	if executor == nil {
		t.Fatal("expected executor")
	}
	if session.Model().ID != "claude-sonnet-4-6" || session.Credential() != "ANTHROPIC_API_KEY" {
		t.Fatalf("unexpected session: %+v %s", session.Model(), session.Credential())
	}
	if got := thread.State(); got != threads.StateIdle {
		t.Fatalf("expected idle state after switch, got %q", got)
	}
}

func TestSwitchModelIfIdleRejectsNonIdleThread(t *testing.T) {
	thread := threads.New()
	thread.QueueItem(threads.SendItem{})
	t.Setenv("OPENAI_API_KEY", "test-key")
	session := testSession(t, "")

	_, err := switchModelIfIdle(thread, session, "claude-sonnet-4-6")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), string(threads.StateConstructLLMRequest)) {
		t.Fatalf("expected non-idle state in error, got %v", err)
	}
}

func TestSwitchModelIfIdleRequiresProviderKey(t *testing.T) {
	thread := threads.New()
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("ANTHROPIC_API_KEY", "")
	session := testSession(t, "")

	_, err := switchModelIfIdle(thread, session, "claude-sonnet-4-6")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Fatalf("expected missing key in error, got %v", err)
	}
}
