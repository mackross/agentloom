package anthropic

import (
	anthropicapi "github.com/anthropics/anthropic-sdk-go"

	"github.com/mackross/agentloom/llms"
)

// SetEffort maps a global effort level onto Anthropic's adaptive thinking and
// output effort. EffortDefault enables adaptive thinking with the model's own
// effort; EffortNone disables thinking.
func (s *MessagesStreamer) SetEffort(e llms.Effort) error {
	s.OutputConfig.Effort = ""
	switch e {
	case llms.EffortNone:
		s.Thinking = anthropicapi.ThinkingConfigParamUnion{OfDisabled: &anthropicapi.ThinkingConfigDisabledParam{}}
		return nil
	case llms.EffortMinimal, llms.EffortLow:
		s.OutputConfig.Effort = anthropicapi.OutputConfigEffortLow
	case llms.EffortMedium:
		s.OutputConfig.Effort = anthropicapi.OutputConfigEffortMedium
	case llms.EffortHigh:
		s.OutputConfig.Effort = anthropicapi.OutputConfigEffortHigh
	case llms.EffortXHigh, llms.EffortMax:
		s.OutputConfig.Effort = anthropicapi.OutputConfigEffortMax
	}
	s.Thinking = anthropicapi.ThinkingConfigParamUnion{OfAdaptive: &anthropicapi.ThinkingConfigAdaptiveParam{}}
	return nil
}
