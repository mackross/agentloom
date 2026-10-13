package openai

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/mackross/agentloom/llms"
	"github.com/mackross/agentloom/threads"
)

func TestServiceTierCatalogAndPricing(t *testing.T) {
	c := llms.NewCatalog(Provider)
	for _, tc := range []struct {
		model   string
		ceiling float64
		want    llms.ServiceTierID
	}{
		{ModelAstra, 6, ServiceTierUltrafast},
		{ModelAstra, 5, ServiceTierFast},
		{ModelAstra, 1, ServiceTierStandard},
		{ModelAstra, 0.5, ServiceTierFlex},
		{"gpt-6-sol", 6, ServiceTierFast},
		{"gpt-6-luna", 2, ServiceTierFast},
		{"gpt-6.1-sol", 6, ServiceTierUltrafast},
		{"gpt-6.1-sol", 5.99, ServiceTierFast},
		{"gpt-6.1-sol", 2, ServiceTierFast},
		{"gpt-6.1-sol", 1, ServiceTierStandard},
		{"gpt-6.1-sol", 0.5, ServiceTierFlex},
		{"gpt-5.6-sol", 100, ServiceTierFast}, // Preview price is unknown.
		{DefaultModel, 1.75, ServiceTierFast},
		{DefaultModel, 1.7, ServiceTierStandard},
		{"gpt-future-model", 100, ServiceTierStandard}, // Never guess its premium.
	} {
		t.Run(fmt.Sprintf("%s/%gx", tc.model, tc.ceiling), func(t *testing.T) {
			m, err := c.Resolve(tc.model)
			if err != nil {
				t.Fatal(err)
			}
			tier, err := m.ServiceTierForMultiplier(tc.ceiling)
			if err != nil || tier != tc.want {
				t.Fatalf("selected tier = %q, %v; want %q", tier, err, tc.want)
			}
			s := NewResponsesStreamer(m.ID)
			if err := s.SetServiceTier(tier); err != nil {
				t.Fatal(err)
			}
			params, err := s.responseRequest(threads.Req{Items: []threads.Item{threads.UserText("hi")}})
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(params)
			if err != nil {
				t.Fatal(err)
			}
			var fields struct {
				Model       string `json:"model"`
				ServiceTier string `json:"service_tier"`
			}
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			if fields.Model != m.ID || fields.ServiceTier != string(tc.want) {
				t.Fatalf("wire model/tier = %q/%q; want %q/%q", fields.Model, fields.ServiceTier, m.ID, tc.want)
			}
		})
	}
	for _, m := range c.Models() {
		if len(m.ServiceTiers) == 0 || !m.AcceptsServiceTier(m.FastServiceTier) {
			t.Errorf("missing UI metadata for %s", m.ID)
		}
		for _, tier := range m.ServiceTiers {
			if tier.Label == "" {
				t.Errorf("missing label for %s/%s", m.ID, tier.ID)
			}
		}
	}
}

func TestServiceTierResidencyNotes(t *testing.T) {
	for _, tc := range []struct {
		model string
		tier  llms.ServiceTierID
		want  string
	}{
		{"gpt-6.1-sol", ServiceTierFast, ""},
		{"gpt-6.1-sol", ServiceTierUltrafast, "Global processing and US or EU data residency; separate Ultrafast rate limits apply."},
		{ModelAstra, ServiceTierFast, "Unavailable with EU data residency."},
		{ModelAstra, ServiceTierUltrafast, "Global processing and US data residency only; account rate limits apply."},
	} {
		t.Run(tc.model+"/"+string(tc.tier), func(t *testing.T) {
			for _, tier := range serviceTiers(tc.model) {
				if tier.ID == tc.tier {
					if tier.Notes != tc.want {
						t.Fatalf("notes = %q; want %q", tier.Notes, tc.want)
					}
					return
				}
			}
			t.Fatalf("missing tier %q", tc.tier)
		})
	}
}

func TestServiceTierWireValues(t *testing.T) {
	s := NewResponsesStreamer(ModelAstra)
	for _, tier := range []llms.ServiceTierID{
		ServiceTierStandard, ServiceTierFast, ServiceTierUltrafast, ServiceTierFlex, "",
	} {
		if err := s.SetServiceTier(tier); err != nil {
			t.Fatal(err)
		}
		params, err := s.responseRequest(threads.Req{Items: []threads.Item{threads.UserText("hi")}})
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(params)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		if tier == "" {
			if _, ok := fields["service_tier"]; ok {
				t.Fatalf("reset must omit service_tier: %s", raw)
			}
		} else {
			var got string
			if err := json.Unmarshal(fields["service_tier"], &got); err != nil || got != string(tier) {
				t.Fatalf("wire tier = %q, %v; want %q", got, err, tier)
			}
		}
	}
	if err := s.SetServiceTier(ServiceTierUltrafast); err != nil {
		t.Fatal(err)
	}
	if err := s.SetServiceTier("other-provider-tier"); err == nil || string(s.ServiceTier) != "ultrafast" {
		t.Fatal("invalid tier must not change state")
	}
	if err := s.SetFast(true); err != nil || string(s.ServiceTier) != "priority" {
		t.Fatal("legacy fast mapping changed")
	}
	if err := s.SetFast(false); err != nil || s.ServiceTier != "" {
		t.Fatal("legacy off must restore project defaults")
	}
}
