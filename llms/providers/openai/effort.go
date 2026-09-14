package openai

import (
	"github.com/mackross/openai-go/v3/responses"
	"github.com/mackross/openai-go/v3/shared"

	"github.com/mackross/agentloom/llms"
)

// SetEffort maps a global effort level onto the Responses reasoning effort.
// EffortDefault clears it. Unknown wire values such as "max" are passed through
// because the vendored SDK enum stops at "xhigh"; the API accepts them.
func (s *ResponsesStreamer) SetEffort(e llms.Effort) error {
	if e == llms.EffortDefault {
		s.Reasoning = shared.ReasoningParam{}
		return nil
	}
	s.Reasoning = shared.ReasoningParam{Effort: shared.ReasoningEffort(e)}
	return nil
}

// SetFast toggles the priority service tier. GPT-6 Astra supports Fast mode
// except with EU data residency, where the API rejects the tier; callers in a
// EU-resident project should leave Fast off.
func (s *ResponsesStreamer) SetFast(on bool) error {
	s.ServiceTier = ""
	if on {
		s.ServiceTier = responses.ResponseNewParamsServiceTierPriority
	}
	return nil
}
