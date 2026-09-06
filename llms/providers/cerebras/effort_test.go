package cerebras

import (
	"context"
	"testing"

	"github.com/mackross/agentloom/llms"
)

func TestSetEffortAndOpen(t *testing.T) {
	s, err := Provider.Open(context.Background(), llms.Model{ID: GPTOSS120BModel}, llms.Options{APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	cs := s.(*ChatCompletionsStreamer)
	if cs.ReasoningFormat != "hidden" {
		t.Fatalf("ReasoningFormat = %q", cs.ReasoningFormat)
	}
	if err := cs.SetEffort(llms.EffortHigh); err != nil || cs.ReasoningEffort != "high" {
		t.Fatalf("SetEffort: %v %q", err, cs.ReasoningEffort)
	}
	if err := cs.SetEffort(llms.EffortDefault); err != nil || cs.ReasoningEffort != "" {
		t.Fatalf("SetEffort(default): %v %q", err, cs.ReasoningEffort)
	}
	if err := cs.SetFast(true); err != nil || cs.ServiceTier != "priority" {
		t.Fatalf("SetFast: %v %q", err, cs.ServiceTier)
	}
	s, _ = Provider.Open(context.Background(), llms.Model{ID: Gemma4_31BModel}, llms.Options{APIKey: "k"})
	if s.(*ChatCompletionsStreamer).ReasoningFormat != "" {
		t.Fatal("gemma should not set ReasoningFormat")
	}
}
