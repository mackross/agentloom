package googlegenai

import (
	"google.golang.org/genai"

	"github.com/mackross/agentloom/llms"
)

// SetEffort maps a global effort level onto the Gemini thinking level.
// EffortDefault removes the thinking config.
func (s *GenerateContentStreamer) SetEffort(e llms.Effort) error {
	if e == llms.EffortDefault {
		s.Config.ThinkingConfig = nil
		return nil
	}
	level := genai.ThinkingLevelLow
	switch e {
	case llms.EffortMedium:
		level = genai.ThinkingLevelMedium
	case llms.EffortHigh, llms.EffortXHigh, llms.EffortMax:
		level = genai.ThinkingLevelHigh
	}
	s.Config.ThinkingConfig = &genai.ThinkingConfig{ThinkingLevel: level}
	return nil
}
