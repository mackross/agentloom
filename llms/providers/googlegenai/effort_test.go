package googlegenai

import (
	"testing"

	"google.golang.org/genai"

	"github.com/mackross/agentloom/llms"
)

func TestSetEffort(t *testing.T) {
	s := &GenerateContentStreamer{model: DefaultModel}
	if err := s.SetEffort(llms.EffortHigh); err != nil || s.Config.ThinkingConfig == nil || s.Config.ThinkingConfig.ThinkingLevel != genai.ThinkingLevelHigh {
		t.Fatalf("high: %v %+v", err, s.Config.ThinkingConfig)
	}
	if err := s.SetEffort(llms.EffortLow); err != nil || s.Config.ThinkingConfig.ThinkingLevel != genai.ThinkingLevelLow {
		t.Fatalf("low: %v %+v", err, s.Config.ThinkingConfig)
	}
	if err := s.SetEffort(llms.EffortDefault); err != nil || s.Config.ThinkingConfig != nil {
		t.Fatalf("default: %v %+v", err, s.Config.ThinkingConfig)
	}
}
