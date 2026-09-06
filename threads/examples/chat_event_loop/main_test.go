package main

import (
	"context"
	"testing"

	"github.com/mackross/agentloom/harness"
	"github.com/mackross/agentloom/threads"
)

func TestSwitchModelIfIdleUsesHarnessSession(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	t.Setenv("GEMINI_API_KEY", "test-key")
	h, err := harness.Open(context.Background(), harness.Options{Store: harness.Memory(harness.Settings{})})
	if err != nil {
		t.Fatal(err)
	}
	session, err := h.Session(context.Background(), "gemini")
	if err != nil {
		t.Fatal(err)
	}
	thread := threads.New()
	if _, err := switchModelIfIdle(thread, session, "sonnet"); err != nil {
		t.Fatal(err)
	}
	if got := session.Model().ID; got != "claude-sonnet-4-6" {
		t.Fatalf("model = %q", got)
	}
}
