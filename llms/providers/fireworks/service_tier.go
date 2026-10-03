package fireworks

import (
	"fmt"

	"github.com/mackross/agentloom/llms"
)

const (
	ServiceTierStandard llms.ServiceTierID = "default"
	ServiceTierPriority llms.ServiceTierID = "priority"
)

func priorityTiers(multiplier float64) []llms.ServiceTier {
	tiers := []llms.ServiceTier{}
	if multiplier > 0 {
		tiers = append(tiers, llms.ServiceTier{ID: ServiceTierPriority, Label: "Priority", MaxMultiplier: multiplier})
	}
	return append(tiers, llms.ServiceTier{ID: ServiceTierStandard, Label: "Standard", MaxMultiplier: 1})
}

// SetServiceTier selects serverless priority. Fireworks Standard is requested
// by omitting service_tier; Fast instead requires a separate router model ID.
func (s *ChatCompletionsStreamer) SetServiceTier(id llms.ServiceTierID) error {
	switch id {
	case "", ServiceTierStandard:
		s.ServiceTier = ""
	case ServiceTierPriority:
		if s.model == Kimi3FastModel || s.model == GLM53FastModel {
			return fmt.Errorf("fireworks: priority is unavailable on Fast routers")
		}
		s.ServiceTier = string(id)
	default:
		return fmt.Errorf("fireworks: unsupported service tier %q", id)
	}
	return nil
}
