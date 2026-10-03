package all_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/mackross/agentloom/llms"
	"github.com/mackross/agentloom/llms/providers/all"
	"github.com/mackross/agentloom/llms/providers/cerebras"
	"github.com/mackross/agentloom/llms/providers/fireworks"
)

func TestHostedOpenWeightAliasesAreProviderScoped(t *testing.T) {
	for _, p := range []*llms.Provider{cerebras.Provider, fireworks.Provider} {
		for _, m := range p.Models {
			for _, alias := range m.Aliases {
				if !strings.HasPrefix(alias, p.ID+"-") {
					t.Errorf("%s/%s has unscoped alias %q", p.ID, m.ID, alias)
				}
			}
		}
	}
}

func TestHostedAliasesDoNotClaimLocalModelNames(t *testing.T) {
	providers := all.Providers()
	for _, reverse := range []bool{false, true} {
		if reverse {
			for i, j := 0, len(providers)-1; i < j; i, j = i+1, j-1 {
				providers[i], providers[j] = providers[j], providers[i]
			}
		}
		c := llms.NewCatalog(providers...)
		for _, name := range []string{"qwen", "glm", "kimi", "kimi-k3", "ember", "minimax", "qwen-max", "kimi-fast", "glm-fast"} {
			if _, err := c.Resolve(name); !errors.Is(err, llms.ErrUnknownModel) {
				t.Fatalf("bare alias %q still claimed: %v", name, err)
			}
		}
		// A user's existing local selection must remain local regardless of
		// the order in which hosted providers are registered.
		for _, name := range []string{"qwen", "glm", "kimi", "minimax"} {
			if err := c.Add(llms.Model{Provider: "ollama", Name: name, ID: name + ":local"}); err != nil {
				t.Fatal(err)
			}
			m, err := c.Resolve(strings.ToUpper(name))
			if err != nil || m.Provider != "ollama" || m.ID != name+":local" {
				t.Fatalf("local %q = %+v, %v", name, m, err)
			}
		}
		for alias, want := range map[string]struct{ provider, id string }{
			"cerebras-qwen":       {"cerebras", cerebras.Qwen38_27BModel},
			"fireworks-qwen":      {"fireworks", fireworks.Qwen38MaxModel},
			"fireworks-qwen-max":  {"fireworks", fireworks.Qwen38MaxModel},
			"fireworks-glm":       {"fireworks", fireworks.GLM53Model},
			"fireworks-kimi":      {"fireworks", fireworks.Kimi3Model},
			"fireworks-kimi-k3":   {"fireworks", fireworks.Kimi3Model},
			"fireworks-ember":     {"fireworks", fireworks.Ember1Model},
			"fireworks-minimax":   {"fireworks", fireworks.MiniMaxM3Model},
			"fireworks-kimi-fast": {"fireworks", fireworks.Kimi3FastModel},
			"fireworks-glm-fast":  {"fireworks", fireworks.GLM53FastModel},
		} {
			for _, name := range []string{alias, strings.ToUpper(alias), want.provider + "/" + want.id} {
				m, err := c.Resolve(name)
				if err != nil || m.Provider != want.provider || m.ID != want.id {
					t.Errorf("Resolve(%q) = %+v, %v; want %+v", name, m, err, want)
				}
			}
		}
	}
}
