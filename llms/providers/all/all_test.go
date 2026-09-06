package all_test

import (
	"context"
	"errors"
	"testing"

	"github.com/mackross/agentloom/llms"
	"github.com/mackross/agentloom/llms/providers/all"
	"github.com/mackross/agentloom/llms/providers/ollama"
)

func TestCatalogAliases(t *testing.T) {
	cat := all.Catalog()
	for alias, want := range map[string]string{
		"sol": "openai", "grok": "xai", "gemma": "cerebras", "gemini": "google",
		"sonnet": "anthropic", "opus": "anthropic", "kimi": "fireworks", "deepseek": "deepseek",
	} {
		m, err := cat.Resolve(alias)
		if err != nil || m.Provider != want {
			t.Errorf("Resolve(%q) = %+v, %v; want provider %s", alias, m, err, want)
		}
	}
	for _, p := range cat.Providers() {
		if p.ID == ollama.ID {
			continue
		}
		d, ok := cat.Default(p.ID)
		if !ok {
			t.Errorf("%s: no default model", p.ID)
			continue
		}
		if p.Match != nil {
			if m, ok := p.Match(d.ID); !ok || m.ID != d.ID {
				t.Errorf("%s: Match(%q) = %+v, %v", p.ID, d.ID, m, ok)
			}
		}
	}
	m, err := cat.Resolve("ollama/qwen3:8b")
	if err != nil || m.Provider != ollama.ID || m.ID != "qwen3:8b" {
		t.Fatalf("ollama/qwen3:8b: %+v %v", m, err)
	}
	m, err = cat.Resolve("models/gemini-3.9-pro")
	if err != nil || m.Provider != "google" || m.ID != "gemini-3.9-pro" {
		t.Fatalf("gemini match: %+v %v", m, err)
	}
}

func TestOpenRequiresCredential(t *testing.T) {
	cat := all.Catalog()
	for _, key := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GOOGLE_API_KEY", "GEMINI_API_KEY", "XAI_API_KEY", "CEREBRAS_API_KEY", "FIREWORKS_API_KEY", "FIREWORKS_AI_API_KEY", "DEEPSEEK_API_KEY"} {
		t.Setenv(key, "")
	}
	for _, p := range cat.Providers() {
		if p.ID == ollama.ID {
			continue
		}
		d, _ := cat.Default(p.ID)
		if _, _, err := cat.Open(context.Background(), d.Name, llms.Options{}); !errors.Is(err, llms.ErrNoCredential) {
			t.Errorf("%s: Open without credential = %v", p.ID, err)
		}
		s, m, err := cat.Open(context.Background(), d.Name, llms.Options{APIKey: "test"})
		if err != nil {
			t.Errorf("%s: Open with key: %v", p.ID, err)
			continue
		}
		if _, ok := s.(llms.EffortSetter); len(m.Efforts) > 0 && !ok {
			t.Errorf("%s: model lists efforts but streamer has no SetEffort", p.ID)
		}
		if _, ok := s.(llms.FastSetter); m.Fast && !ok {
			t.Errorf("%s: model is Fast but streamer has no SetFast", p.ID)
		}
	}
}
