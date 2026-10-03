package cerebras

import (
	"fmt"

	"github.com/mackross/agentloom/llms"
)

// ServiceTierPriority is Cerebras's legacy Fast tier.
const ServiceTierPriority llms.ServiceTierID = "priority"

// SetServiceTier controls dedicated inference only. Shared Inference models
// deliberately do not advertise these tiers in the catalog.
func (s *ChatCompletionsStreamer) SetServiceTier(id llms.ServiceTierID) error {
	switch id {
	case "", ServiceTierPriority:
		s.ServiceTier = string(id)
		return nil
	default:
		return fmt.Errorf("cerebras: unsupported service tier %q", id)
	}
}
