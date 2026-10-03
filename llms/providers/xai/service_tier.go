package xai

import (
	"fmt"

	"github.com/mackross/agentloom/llms"
	"github.com/mackross/openai-go/v3/responses"
)

// ServiceTierPriority is xAI's legacy Fast tier.
const ServiceTierPriority llms.ServiceTierID = "priority"
const ServiceTierStandard llms.ServiceTierID = "default"

// https://docs.x.ai/developers/pricing#priority-processing-pricing
// The 2x premium applies to all text token types, after cache discounts.
var serviceTiers = []llms.ServiceTier{
	{ID: ServiceTierPriority, Label: "Priority", MaxMultiplier: 2, Notes: "Billed at priority rates only when the response confirms priority."},
	{ID: ServiceTierStandard, Label: "Standard", MaxMultiplier: 1},
}

func (s *ResponsesStreamer) SetServiceTier(id llms.ServiceTierID) error {
	switch id {
	case "", ServiceTierPriority, ServiceTierStandard:
		s.ServiceTier = responses.ResponseNewParamsServiceTier(id)
		return nil
	default:
		return fmt.Errorf("xai: unsupported service tier %q", id)
	}
}
