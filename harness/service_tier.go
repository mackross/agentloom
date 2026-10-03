package harness

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/mackross/agentloom/llms"
)

// ServiceTierPreference remembers either a portable budget ceiling or an
// explicit provider/model-scoped tier. These modes are mutually exclusive.
// The zero value resets to provider/project defaults, not standard pricing.
type ServiceTierPreference struct {
	Provider      string
	Model         string // Wire model ID, not a catalog alias.
	ID            llms.ServiceTierID
	MaxMultiplier float64 // Zero disables budget-based selection.
}

func (p ServiceTierPreference) validate() error {
	if p.MaxMultiplier < 0 || math.IsNaN(p.MaxMultiplier) || math.IsInf(p.MaxMultiplier, 0) {
		return fmt.Errorf("service tier multiplier must be finite and non-negative")
	}
	if p.MaxMultiplier != 0 {
		if p.ID != "" || p.Provider != "" || p.Model != "" {
			return fmt.Errorf("service tier multiplier and explicit tier are mutually exclusive")
		}
	} else if p.ID != "" {
		if p.Provider == "" || p.Model == "" {
			return fmt.Errorf("explicit service tier requires provider and model")
		}
	} else if p.Provider != "" || p.Model != "" {
		return fmt.Errorf("service tier scope requires an explicit tier ID")
	}
	return nil
}

func (p ServiceTierPreference) resolve(m llms.Model) (llms.ServiceTierID, ServiceTierPreference, error) {
	if err := p.validate(); err != nil {
		return "", p, err
	}
	if p.MaxMultiplier != 0 {
		id, err := m.ServiceTierForMultiplier(p.MaxMultiplier)
		return id, p, err
	}
	if p.ID != "" && (p.Provider != m.Provider || p.Model != m.ID) {
		// Explicit IDs never carry over to a different provider/model.
		return "", ServiceTierPreference{}, nil
	}
	if !m.AcceptsServiceTier(p.ID) {
		return "", p, fmt.Errorf("service tier %q is not supported by %s", p.ID, m.Name)
	}
	return p.ID, p, nil
}

func applyServiceTier(sel *selection, id llms.ServiceTierID) error {
	if setter, ok := sel.streamer.(llms.ServiceTierSetter); ok {
		return setter.SetServiceTier(id)
	}
	if id != "" {
		return fmt.Errorf("%s has no service tier control", sel.model.Name)
	}
	if setter, ok := sel.streamer.(llms.FastSetter); ok {
		return setter.SetFast(false)
	}
	return nil
}

// ServiceTier returns the selected provider-defined tier. Empty means provider
// defaults, or a legacy third-party FastSetter with no tier metadata.
func (s *Session) ServiceTier() llms.ServiceTierID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sel.tier
}

// ServiceTierMultiplier returns the remembered budget ceiling, or zero when
// selection is explicit/default. It is not the selected tier's actual price.
func (s *Session) ServiceTierMultiplier() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sel.tierPreference.MaxMultiplier
}

// SetServiceTier selects an explicit tier from the current model's catalog
// metadata. It clears the multiplier preference. Empty restores provider
// defaults; a nonempty ID is scoped to the current provider and wire model.
func (s *Session) SetServiceTier(id llms.ServiceTierID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := ServiceTierPreference{}
	if id != "" {
		p = ServiceTierPreference{Provider: s.sel.model.Provider, Model: s.sel.model.ID, ID: id}
	}
	return s.setServiceTierLocked(p)
}

// SetServiceTierForMultiplier selects a tier at or below ceiling and remembers
// the ceiling across model switches. It errors if no known tier qualifies;
// it never silently falls back to a potentially paid project default.
// Use SetServiceTier("") to clear the preference.
func (s *Session) SetServiceTierForMultiplier(ceiling float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Unlike persisted zero (unset), an explicit zero ceiling is invalid.
	if ceiling <= 0 {
		return fmt.Errorf("service tier multiplier must be positive")
	}
	return s.setServiceTierLocked(ServiceTierPreference{MaxMultiplier: ceiling})
}

func (s *Session) setServiceTierLocked(p ServiceTierPreference) error {
	id, p, err := p.resolve(s.sel.model)
	if err != nil {
		return err
	}
	if err := applyServiceTier(s.sel, id); err != nil {
		return err
	}
	if err := s.h.save(context.Background(), func(st *Settings) error {
		st.ServiceTier = p
		st.Fast = false
		return nil
	}); err != nil {
		return errors.Join(err, restoreServiceTier(s.sel))
	}
	s.sel.tier = id
	s.sel.tierPreference = p
	s.sel.fast = id != "" && id == s.sel.model.FastServiceTier
	return nil
}

func restoreServiceTier(sel *selection) error {
	if sel.fast && sel.tier == "" {
		if setter, ok := sel.streamer.(llms.FastSetter); ok {
			return setter.SetFast(true)
		}
	}
	return applyServiceTier(sel, sel.tier)
}
