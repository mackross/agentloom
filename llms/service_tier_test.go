package llms

import (
	"math"
	"testing"
)

func TestServiceTierForMultiplier(t *testing.T) {
	m := Model{ID: "example", ServiceTiers: []ServiceTier{
		{ID: "preview"}, // Unknown pricing is never selected automatically.
		{ID: "express", MaxMultiplier: 6},
		{ID: "priority", MaxMultiplier: 2},
		{ID: "baseline", MaxMultiplier: 1},
		{ID: "economy", MaxMultiplier: 0.5},
	}}
	for _, tc := range []struct {
		ceiling float64
		want    ServiceTierID
	}{
		{0.5, "economy"}, {1, "baseline"}, {1.99, "baseline"},
		{2, "priority"}, {5.99, "priority"}, {6, "express"}, {100, "express"},
	} {
		got, err := m.ServiceTierForMultiplier(tc.ceiling)
		if err != nil || got != tc.want {
			t.Errorf("ceiling %g: %q, %v; want %q", tc.ceiling, got, err, tc.want)
		}
	}
	for _, ceiling := range []float64{-1, 0, 0.49, math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := m.ServiceTierForMultiplier(ceiling); err == nil {
			t.Errorf("accepted ceiling %g", ceiling)
		}
	}
	if !m.AcceptsServiceTier("") || !m.AcceptsServiceTier("preview") || m.AcceptsServiceTier("other") {
		t.Fatal("incorrect tier acceptance")
	}
	for _, ceiling := range []float64{1, 2, 6} {
		if id, err := (Model{}).ServiceTierForMultiplier(ceiling); err != nil || id != "" {
			t.Fatalf("tierless model at %gx: %q, %v", ceiling, id, err)
		}
	}
	if _, err := (Model{}).ServiceTierForMultiplier(0.5); err == nil {
		t.Fatal("tierless model must not round up from a sub-standard ceiling")
	}
	// Select the nearest multiplier below the ceiling even in an unsorted catalog.
	m.ServiceTiers = []ServiceTier{{ID: "preferred", MaxMultiplier: 1}, {ID: "other", MaxMultiplier: 2}}
	if id, _ := m.ServiceTierForMultiplier(3); id != "other" {
		t.Fatal("did not select the nearest multiplier below the ceiling")
	}
	m.ServiceTiers = append(m.ServiceTiers, ServiceTier{ID: "tie", MaxMultiplier: 2})
	if id, _ := m.ServiceTierForMultiplier(2); id != "other" {
		t.Fatal("equal multipliers must preserve provider preference order")
	}
	// A caller may filter unavailable tiers without rewriting legacy metadata.
	m.FastServiceTier = "priority"
	if id, err := m.ServiceTierForMultiplier(1); err != nil || id != "preferred" {
		t.Fatalf("filtered tiers: %q, %v", id, err)
	}
}

func TestServiceTierMetadataValidation(t *testing.T) {
	for _, tiers := range [][]ServiceTier{
		{{ID: ""}},
		{{ID: "x"}, {ID: "x"}},
		{{ID: "x", MaxMultiplier: -1}},
		{{ID: "x", MaxMultiplier: math.NaN()}},
		{{ID: "x", MaxMultiplier: math.Inf(1)}},
	} {
		m := Model{Provider: "p", ID: "m", ServiceTiers: tiers}
		c := NewCatalog(&Provider{ID: "p"})
		if err := c.Add(m); err == nil {
			t.Errorf("accepted invalid tiers %+v", tiers)
		}
	}
}

func TestCatalogServiceTiersAreIndependent(t *testing.T) {
	p := &Provider{ID: "p", Models: []Model{{ID: "m", ServiceTiers: []ServiceTier{{ID: "base", MaxMultiplier: 1}}}}}
	c := NewCatalog(p)
	p.Models[0].ServiceTiers[0].MaxMultiplier = 99
	m, _ := c.Resolve("m")
	if m.ServiceTiers[0].MaxMultiplier != 1 {
		t.Fatal("catalog retained provider slice")
	}
	m.ServiceTiers[0].MaxMultiplier = 99
	c.Models()[0].ServiceTiers[0].ID = "changed"
	d, _ := c.Default("p")
	d.ServiceTiers[0].ID = "changed"
	m, _ = c.Resolve("p/m")
	if m.ServiceTiers[0].ID != "base" || m.ServiceTiers[0].MaxMultiplier != 1 {
		t.Fatal("caller mutated catalog tier metadata")
	}
}
