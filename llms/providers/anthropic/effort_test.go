package anthropic

import (
	"testing"

	anthropicapi "github.com/anthropics/anthropic-sdk-go"

	"github.com/mackross/agentloom/llms"
)

func TestSetEffort(t *testing.T) {
	s := NewMessagesStreamerWithClient(anthropicapi.NewClient(), "claude-sonnet-4-6")
	if err := s.SetEffort(llms.EffortDefault); err != nil || s.Thinking.OfAdaptive == nil || s.OutputConfig.Effort != "" {
		t.Fatalf("default: %v %+v %+v", err, s.Thinking, s.OutputConfig)
	}
	if err := s.SetEffort(llms.EffortMax); err != nil || s.Thinking.OfAdaptive == nil || s.OutputConfig.Effort != anthropicapi.OutputConfigEffortMax {
		t.Fatalf("max: %v %+v", err, s.OutputConfig)
	}
	if err := s.SetEffort(llms.EffortMinimal); err != nil || s.OutputConfig.Effort != anthropicapi.OutputConfigEffortLow {
		t.Fatalf("minimal: %v %+v", err, s.OutputConfig)
	}
	if err := s.SetEffort(llms.EffortNone); err != nil || s.Thinking.OfDisabled == nil || s.OutputConfig.Effort != "" {
		t.Fatalf("none: %v %+v", err, s.Thinking)
	}
	m, ok := Provider.Match("claude-opus-4-7")
	if !ok || len(m.Efforts) == 0 {
		t.Fatalf("match 4.7: %+v %v", m, ok)
	}
	m, _ = Provider.Match("claude-haiku-4-5")
	if len(m.Efforts) != 0 {
		t.Fatalf("haiku should not list efforts: %+v", m)
	}
}
