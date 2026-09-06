package openai

import (
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"

	"github.com/mackross/agentloom/llms"
)

// SetEffort maps a global effort level onto the Responses reasoning effort.
// EffortDefault clears it.
func (s *ResponsesStreamer) SetEffort(e llms.Effort) error {
	if e == llms.EffortDefault {
		s.Reasoning = shared.ReasoningParam{}
		return nil
	}
	s.Reasoning = shared.ReasoningParam{Effort: shared.ReasoningEffort(e)}
	return nil
}

// SetFast toggles the priority service tier.
func (s *ResponsesStreamer) SetFast(on bool) error {
	s.ServiceTier = ""
	if on {
		s.ServiceTier = responses.ResponseNewParamsServiceTierPriority
	}
	return nil
}
