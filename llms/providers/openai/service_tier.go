package openai

import (
	"fmt"
	"strings"

	"github.com/mackross/agentloom/llms"
	"github.com/mackross/openai-go/v3/responses"
)

// OpenAI service tier IDs. Standard explicitly opts out of project defaults;
// Fast uses the backwards-compatible priority wire value.
const (
	ServiceTierStandard  llms.ServiceTierID = "default"
	ServiceTierFast      llms.ServiceTierID = "priority"
	ServiceTierUltrafast llms.ServiceTierID = "ultrafast"
	ServiceTierFlex      llms.ServiceTierID = "flex"
)

// Pricing source: https://developers.openai.com/api/docs/pricing
// Fast/Ultrafast restrictions: /api/docs/guides/fast-mode and ultrafast-mode.
// Keep unknown pricing at zero, rather than guessing for uncurated model IDs.
func serviceTiers(model string) []llms.ServiceTier {
	model = strings.ToLower(model)
	is := func(id string) bool { return model == id || strings.HasPrefix(model, id+"-") }
	fast := llms.ServiceTier{ID: ServiceTierFast, Label: "Fast"}
	standard := llms.ServiceTier{ID: ServiceTierStandard, Label: "Standard", MaxMultiplier: 1}
	var tiers []llms.ServiceTier
	latest := is(ModelAstra) || is("gpt-6.1-sol") || is("gpt-6-sol") || is("gpt-6-luna") ||
		is("gpt-5.6-sol") || is("gpt-5.6-terra") || is("gpt-5.6-luna")
	if latest {
		fast.MaxMultiplier = 2
	}
	if is(DefaultModel) {
		fast.MaxMultiplier = 1.75
	}
	if strings.HasPrefix(model, "gpt-6") && !is("gpt-6.1-sol") {
		fast.Notes = "Unavailable with EU data residency."
	}
	if is(ModelAstra) || is("gpt-6.1-sol") {
		notes := "Global processing and US data residency only; account rate limits apply."
		if is("gpt-6.1-sol") {
			notes = "Global processing and US or EU data residency; separate Ultrafast rate limits apply."
		}
		tiers = append(tiers, llms.ServiceTier{
			ID: ServiceTierUltrafast, Label: "Ultrafast", MaxMultiplier: 6,
			Notes: notes,
		})
	}
	if is("gpt-5.6-sol") {
		tiers = append(tiers, llms.ServiceTier{
			ID: ServiceTierUltrafast, Label: "Ultrafast (preview)",
			Notes: "Requires preview access; pricing unknown. Global processing and US data residency only.",
		})
	}
	tiers = append(tiers, fast, standard)
	if latest {
		tiers = append(tiers, llms.ServiceTier{
			ID: ServiceTierFlex, Label: "Flex", MaxMultiplier: 0.5,
			Notes: "Slower processing; capacity is not guaranteed.",
		})
	}
	return tiers
}

func withServiceTiers(models []llms.Model) []llms.Model {
	for i := range models {
		models[i].ServiceTiers = serviceTiers(models[i].ID)
		models[i].FastServiceTier = ServiceTierFast
	}
	return models
}

// SetServiceTier selects an OpenAI tier, or resets project defaults with "".
// Account access and regional restrictions are enforced by the API.
func (s *ResponsesStreamer) SetServiceTier(id llms.ServiceTierID) error {
	switch id {
	case "", ServiceTierStandard, ServiceTierFast, ServiceTierUltrafast, ServiceTierFlex:
		s.ServiceTier = responses.ResponseNewParamsServiceTier(id)
		return nil
	default:
		return fmt.Errorf("openai: unsupported service tier %q", id)
	}
}
