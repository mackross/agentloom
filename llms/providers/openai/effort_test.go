package openai

import (
	"testing"

	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"

	"github.com/mackross/agentloom/llms"
)

func TestSetEffortAndFast(t *testing.T) {
	s := NewResponsesStreamer("gpt-5.6-sol")
	if err := s.SetEffort(llms.EffortXHigh); err != nil || s.Reasoning.Effort != shared.ReasoningEffort("xhigh") {
		t.Fatalf("SetEffort: %v %+v", err, s.Reasoning)
	}
	if err := s.SetEffort(llms.EffortDefault); err != nil || s.Reasoning.Effort != "" {
		t.Fatalf("SetEffort(default): %v %+v", err, s.Reasoning)
	}
	if err := s.SetFast(true); err != nil || s.ServiceTier != responses.ResponseNewParamsServiceTierPriority {
		t.Fatalf("SetFast(true): %v %q", err, s.ServiceTier)
	}
	if err := s.SetFast(false); err != nil || s.ServiceTier != "" {
		t.Fatalf("SetFast(false): %v %q", err, s.ServiceTier)
	}
}

func TestMatch(t *testing.T) {
	m, ok := Provider.Match("gpt-5.7-preview")
	if !ok || len(m.Efforts) == 0 || !m.Fast {
		t.Fatalf("gpt-5.7: %+v %v", m, ok)
	}
	m, ok = Provider.Match("gpt-4.1")
	if !ok || len(m.Efforts) != 0 {
		t.Fatalf("gpt-4.1: %+v %v", m, ok)
	}
	if _, ok := Provider.Match("claude-x"); ok {
		t.Fatal("claimed claude")
	}
}
