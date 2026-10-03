package googlegenai

import (
	"fmt"

	"github.com/mackross/agentloom/llms"
	"google.golang.org/genai"
)

const (
	ServiceTierStandard llms.ServiceTierID = "standard"
	ServiceTierPriority llms.ServiceTierID = "priority"
)

// https://ai.google.dev/gemini-api/docs/pricing
// All curated text models have a maximum Priority token-rate ratio of 1.8.
// Flex is not listed here: its cached-token ratios vary by model.
var serviceTiers = []llms.ServiceTier{
	{ID: ServiceTierPriority, Label: "Priority", MaxMultiplier: 1.8, Notes: "GenerateContent requires billing Tier 2 or 3; congestion may downgrade to Standard."},
	{ID: ServiceTierStandard, Label: "Standard", MaxMultiplier: 1},
}

func (s *GenerateContentStreamer) SetServiceTier(id llms.ServiceTierID) error {
	switch id {
	case "", ServiceTierStandard, ServiceTierPriority:
		s.Config.ServiceTier = genai.ServiceTier(id)
		return nil
	default:
		return fmt.Errorf("google: unsupported service tier %q", id)
	}
}
