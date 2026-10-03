package llms

import (
	"fmt"
	"math"
)

// ServiceTierID is an opaque, provider-defined tier identifier. The empty ID
// resets the provider's request parameter; it does not mean standard pricing.
type ServiceTierID string

// ServiceTier describes a model's service tier for selection and UI display.
// Catalog metadata describes API capabilities, not account entitlement or
// regional availability. Callers can filter a model's ServiceTiers before
// resolving a multiplier when they know additional access constraints.
type ServiceTier struct {
	ID    ServiceTierID
	Label string
	// MaxMultiplier is the largest applicable token-rate multiplier relative
	// to this model's standard processing. Zero means unknown. It is not a
	// bound on the total bill, tool charges, or the number of generated tokens.
	MaxMultiplier float64
	// Notes describes restrictions such as preview access or data residency.
	Notes string
}

// ServiceTierSetter is implemented by streamers with provider-defined tiers.
// An empty ID restores provider/project defaults. Implementations validate
// their supported IDs; model and account eligibility are the caller's job.
type ServiceTierSetter interface {
	SetServiceTier(ServiceTierID) error
}

// AcceptsServiceTier reports whether id is listed, or resets to provider defaults.
func (m Model) AcceptsServiceTier(id ServiceTierID) bool {
	if id == "" {
		return true
	}
	for _, tier := range m.ServiceTiers {
		if tier.ID == id {
			return true
		}
	}
	return false
}

// ServiceTierForMultiplier chooses the first tier in preference order with a
// known multiplier at or below ceiling. It never rounds up or selects implicit
// project defaults. Providers order tiers fastest/preferred first; price alone
// does not imply speed. No matching tier is an error, including for models
// without tier metadata.
func (m Model) ServiceTierForMultiplier(ceiling float64) (ServiceTierID, error) {
	if ceiling <= 0 || math.IsNaN(ceiling) || math.IsInf(ceiling, 0) {
		return "", fmt.Errorf("llms: service tier multiplier must be positive and finite")
	}
	if err := m.ValidateServiceTiers(); err != nil {
		return "", err
	}
	for _, tier := range m.ServiceTiers {
		if tier.MaxMultiplier > 0 && tier.MaxMultiplier <= ceiling {
			return tier.ID, nil
		}
	}
	return "", fmt.Errorf("llms: model %q has no known service tier within %gx", m.ID, ceiling)
}

// ValidateServiceTiers checks provider and configured tier metadata.
func (m Model) ValidateServiceTiers() error {
	seen := make(map[ServiceTierID]bool, len(m.ServiceTiers))
	for _, tier := range m.ServiceTiers {
		if tier.ID == "" || seen[tier.ID] {
			return fmt.Errorf("llms: model %q has an empty or duplicate service tier %q", m.ID, tier.ID)
		}
		seen[tier.ID] = true
		if tier.MaxMultiplier < 0 || math.IsNaN(tier.MaxMultiplier) || math.IsInf(tier.MaxMultiplier, 0) {
			return fmt.Errorf("llms: model %q service tier %q has an invalid multiplier", m.ID, tier.ID)
		}
	}
	return nil
}
