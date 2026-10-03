package all_test

import (
	"context"
	"testing"

	"github.com/mackross/agentloom/llms"
	"github.com/mackross/agentloom/llms/providers/all"
)

func TestCatalogServiceTiersHaveProviderSetters(t *testing.T) {
	cat := all.Catalog()
	for _, m := range cat.Models() {
		if len(m.ServiceTiers) == 0 {
			if m.Fast {
				t.Errorf("%s/%s: legacy Fast model has no service tier metadata", m.Provider, m.ID)
			}
			continue
		}
		s, _, err := cat.Open(context.Background(), m.Name, llms.Options{APIKey: "test"})
		if err != nil {
			t.Fatal(err)
		}
		setter, ok := s.(llms.ServiceTierSetter)
		if !ok {
			t.Fatalf("%s: listed tiers without a setter", m.Name)
		}
		for _, tier := range m.ServiceTiers {
			if tier.Label == "" {
				t.Errorf("%s/%s: missing display label", m.Name, tier.ID)
			}
			if err := setter.SetServiceTier(tier.ID); err != nil {
				t.Errorf("%s/%s: %v", m.Name, tier.ID, err)
			}
		}
		if err := setter.SetServiceTier("invalid-provider-tier"); err == nil {
			t.Errorf("%s: accepted an unknown tier", m.Name)
		}
		if err := setter.SetServiceTier(""); err != nil {
			t.Errorf("%s: cannot reset tier: %v", m.Name, err)
		}
	}
}
