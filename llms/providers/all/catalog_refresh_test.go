package all_test

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/mackross/agentloom/llms"
	"github.com/mackross/agentloom/llms/providers/all"
	"github.com/mackross/agentloom/llms/providers/cerebras"
	"github.com/mackross/agentloom/llms/providers/fireworks"
	"github.com/mackross/agentloom/llms/providers/googlegenai"
	"github.com/mackross/agentloom/llms/providers/xai"
)

func TestRefreshedCatalogDefaultsAndAliases(t *testing.T) {
	c := all.Catalog()
	for _, tc := range []struct{ provider, id string }{
		{"xai", "grok-4.7"}, {"google", "gemini-3.8-flash"},
		{"cerebras", "gpt-oss-120b"}, {"fireworks", fireworks.Kimi3Model},
	} {
		m, ok := c.Default(tc.provider)
		if !ok || m.ID != tc.id {
			t.Errorf("%s default = %s, want %s", tc.provider, m.ID, tc.id)
		}
	}
	for alias, id := range map[string]string{
		"grok": "grok-4.7", "grok-4.7-latest": "grok-4.7",
		"grok-4.6-latest": "grok-4.6", "cerebras-qwen": "qwen-3.8-27b",
		"gemini-pro": "gemini-3.1-pro-preview", "gemini-lite": "gemini-3.5-flash-lite",
		"fireworks-minimax": fireworks.MiniMaxM3Model, "fireworks-glm": fireworks.GLM53Model,
		"fireworks-kimi-fast": fireworks.Kimi3FastModel,
	} {
		m, err := c.Resolve(alias)
		if err != nil || m.ID != id {
			t.Errorf("%s = %s, %v; want %s", alias, m.ID, err, id)
		}
	}
}

func TestGrok47EffortsAndPriority(t *testing.T) {
	c := all.Catalog()
	for _, id := range []string{"grok-4.7", "GROK-4.7-test-snapshot"} {
		m, err := c.Resolve(id)
		if err != nil || !m.AcceptsEffort(llms.EffortXHigh) || m.AcceptsEffort(llms.EffortMax) {
			t.Fatalf("%s: %+v, %v", id, m, err)
		}
		tier, err := m.ServiceTierForMultiplier(2)
		if err != nil || tier != xai.ServiceTierPriority {
			t.Fatalf("priority: %s, %v", tier, err)
		}
		tier, err = m.ServiceTierForMultiplier(1.99)
		if err != nil || tier != xai.ServiceTierStandard {
			t.Fatalf("standard: %s, %v", tier, err)
		}
	}
	old, _ := c.Resolve("grok-4.6")
	if old.AcceptsEffort(llms.EffortXHigh) {
		t.Fatal("4.7 capabilities leaked into older models")
	}
}

func TestCerebrasSharedCatalog(t *testing.T) {
	c := llms.NewCatalog(cerebras.Provider)
	if len(c.Models()) != 2 {
		t.Fatalf("shared models = %d, want 2", len(c.Models()))
	}
	for _, m := range c.Models() {
		if m.Fast || len(m.ServiceTiers) != 0 {
			t.Fatalf("shared model advertises dedicated tiers: %+v", m)
		}
		s, _, err := c.Open(context.Background(), m.Name, llms.Options{APIKey: "test"})
		if err != nil {
			t.Fatal(err)
		}
		streamer := s.(*cerebras.ChatCompletionsStreamer)
		if m.ID == cerebras.Qwen38_27BModel {
			if !m.AcceptsEffort(llms.EffortNone) || m.AcceptsEffort(llms.EffortXHigh) || streamer.ReasoningFormat == "hidden" {
				t.Fatal("incorrect Qwen reasoning capabilities")
			}
		} else if m.ID != cerebras.GPTOSS120BModel || streamer.ReasoningFormat != "hidden" {
			t.Fatalf("unexpected shared model/config: %+v", m)
		}
	}
	for _, alias := range []string{"gemma", "gemma-4", cerebras.Llama31_8BModel, cerebras.Qwen3235BModel} {
		if _, err := c.Resolve(alias); err == nil {
			t.Errorf("retired/dedicated model still curated: %s", alias)
		}
	}
}

func TestGoogleTextCatalog(t *testing.T) {
	c := llms.NewCatalog(googlegenai.Provider)
	if len(c.Models()) != 9 {
		t.Fatalf("text model count = %d, want 9", len(c.Models()))
	}
	for _, m := range c.Models() {
		matched, ok := googlegenai.Provider.Match("models/" + strings.ToUpper(m.ID))
		if !ok || len(matched.Efforts) != len(m.Efforts) || len(matched.ServiceTiers) != len(m.ServiceTiers) {
			t.Errorf("prefixed match lost metadata: %+v", matched)
		}
		wantMinimal := m.ID != "gemini-3.8-flash" && m.ID != "gemini-3.7-flash" && !strings.Contains(m.ID, "-pro-")
		if m.AcceptsEffort(llms.EffortMinimal) != wantMinimal || m.AcceptsEffort(llms.EffortNone) {
			t.Errorf("incorrect thinking vocabulary: %s", m.ID)
		}
		tier, err := m.ServiceTierForMultiplier(1.8)
		if err != nil || tier != googlegenai.ServiceTierPriority {
			t.Errorf("%s priority = %s, %v", m.ID, tier, err)
		}
	}
	for _, id := range []string{"gemini-3.8-live", "gemini-3.8-flash-tts", "gemini-3.1-flash-image", "gemini-future"} {
		m, _ := googlegenai.Provider.Match(id)
		if len(m.Efforts) != 0 || len(m.ServiceTiers) != 0 {
			t.Errorf("guessed text capabilities for %s", id)
		}
	}
}

func TestFireworksServerlessCatalog(t *testing.T) {
	c := llms.NewCatalog(fireworks.Provider)
	rates := map[string]float64{
		fireworks.Kimi3Model: 1.25, fireworks.Ember1Model: 1.25,
		fireworks.DeepSeekV41FlashModel: 1.25, fireworks.GLM53Model: 1.25,
		fireworks.GLM53FlashModel: 1.25, fireworks.Qwen38MaxModel: 1.5,
		fireworks.MiniMaxM3Model: 1.5, fireworks.GPTOSS120BModel: 1.2,
		fireworks.Nemotron35LightningModel: 1.25, fireworks.Nemotron3UltraModel: 1.25,
	}
	if len(c.Models()) != len(rates)+2 {
		t.Fatal("unexpected serverless catalog")
	}
	for id, rate := range rates {
		m, err := c.Resolve(id)
		if err != nil {
			t.Fatal(err)
		}
		tier, err := m.ServiceTierForMultiplier(rate)
		if err != nil || tier != fireworks.ServiceTierPriority {
			t.Errorf("%s priority: %s, %v", id, tier, err)
		}
		tier, err = m.ServiceTierForMultiplier(math.Nextafter(rate, 0))
		if err != nil || tier != fireworks.ServiceTierStandard {
			t.Errorf("%s exceeded budget: %s, %v", id, tier, err)
		}
	}
	for _, id := range []string{fireworks.Kimi3FastModel, fireworks.GLM53FastModel} {
		m, err := c.Resolve(id)
		if err != nil || m.AcceptsServiceTier(fireworks.ServiceTierPriority) {
			t.Fatalf("Fast router advertises priority: %s, %v", id, err)
		}
	}
	for _, id := range []string{fireworks.DeepSeekV4ProModel, fireworks.DeepSeekV4FlashModel, fireworks.MiniMaxM27Model, "accounts/test/deployments/private"} {
		_, _, err := c.Open(context.Background(), "fireworks/"+id, llms.Options{APIKey: "test"})
		if err == nil || !strings.Contains(err.Error(), "only serverless") {
			t.Errorf("did not reject dedicated-only model %s: %v", id, err)
		}
	}
}
