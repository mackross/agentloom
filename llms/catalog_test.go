package llms_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mackross/agentloom/llms"
	"github.com/mackross/agentloom/threads"
)

type stubStreamer struct {
	threads.LLMStreamer
	model  llms.Model
	opts   llms.Options
	effort llms.Effort
	fast   bool
}

func (s *stubStreamer) SetEffort(e llms.Effort) error { s.effort = e; return nil }
func (s *stubStreamer) SetFast(on bool) error         { s.fast = on; return nil }

func testProviders() (*llms.Provider, *llms.Provider) {
	acme := &llms.Provider{
		ID:  "acme",
		Env: []string{"ACME_API_KEY"},
		Models: []llms.Model{
			{ID: "acme-large", Aliases: []string{"large"}, Efforts: []llms.Effort{llms.EffortLow, llms.EffortHigh}, Fast: true},
			{ID: "acme-small", Aliases: []string{"small"}},
		},
		Match: llms.Prefix("acme-"),
		Open: func(_ context.Context, m llms.Model, o llms.Options) (threads.LLMStreamer, error) {
			if o.APIKey == "" {
				o.APIKey = llms.EnvAPIKey([]string{"ACME_API_KEY"})
			}
			if o.APIKey == "" {
				return nil, llms.ErrNoCredential
			}
			return &stubStreamer{model: m, opts: o}, nil
		},
	}
	local := &llms.Provider{
		ID: "local",
		Open: func(_ context.Context, m llms.Model, o llms.Options) (threads.LLMStreamer, error) {
			return &stubStreamer{model: m, opts: o}, nil
		},
	}
	return acme, local
}

func TestParseEffort(t *testing.T) {
	for _, in := range []string{"", "default", "DEFAULT", " default "} {
		e, err := llms.ParseEffort(in)
		if err != nil || e != llms.EffortDefault {
			t.Fatalf("ParseEffort(%q) = %q, %v", in, e, err)
		}
	}
	e, err := llms.ParseEffort("High")
	if err != nil || e != llms.EffortHigh {
		t.Fatalf("ParseEffort(High) = %q, %v", e, err)
	}
	if _, err := llms.ParseEffort("turbo"); err == nil || !strings.Contains(err.Error(), "xhigh") {
		t.Fatalf("expected error listing levels, got %v", err)
	}
	if llms.EffortDefault.String() != "default" || llms.EffortMax.String() != "max" {
		t.Fatal("String()")
	}
}

func TestCatalogResolve(t *testing.T) {
	acme, local := testProviders()
	cat := llms.NewCatalog(acme, local)

	m, err := cat.Resolve("LARGE")
	if err != nil || m.Provider != "acme" || m.ID != "acme-large" || m.Name != "acme-large" {
		t.Fatalf("alias resolve: %+v %v", m, err)
	}
	if !m.AcceptsEffort(llms.EffortDefault) || !m.AcceptsEffort(llms.EffortHigh) || m.AcceptsEffort(llms.EffortMax) {
		t.Fatal("AcceptsEffort")
	}
	m, err = cat.Resolve("acme-experimental")
	if err != nil || m.Provider != "acme" || m.Name != "acme-experimental" {
		t.Fatalf("match resolve: %+v %v", m, err)
	}
	m, err = cat.Resolve("local/anything:7b")
	if err != nil || m.Provider != "local" || m.ID != "anything:7b" {
		t.Fatalf("provider/id resolve: %+v %v", m, err)
	}
	m, err = cat.Resolve("acme/large")
	if err != nil || m.ID != "acme-large" {
		t.Fatalf("provider/alias resolve: %+v %v", m, err)
	}
	if _, err := cat.Resolve("accounts/nobody/x"); !errors.Is(err, llms.ErrUnknownModel) {
		t.Fatalf("expected ErrUnknownModel, got %v", err)
	}
	if _, err := cat.Resolve("nope"); !errors.Is(err, llms.ErrUnknownModel) {
		t.Fatalf("expected ErrUnknownModel, got %v", err)
	}
	if d, ok := cat.Default("acme"); !ok || d.ID != "acme-large" {
		t.Fatalf("Default: %+v %v", d, ok)
	}
	if _, ok := cat.Default("local"); ok {
		t.Fatal("local should have no default")
	}
}

func TestCatalogAdd(t *testing.T) {
	acme, local := testProviders()
	cat := llms.NewCatalog(acme, local)

	if err := cat.Add(llms.Model{Provider: "local", Name: "qwen", ID: "qwen3:8b", Aliases: []string{"q"}}); err != nil {
		t.Fatal(err)
	}
	m, err := cat.Resolve("q")
	if err != nil || m.ID != "qwen3:8b" {
		t.Fatalf("added alias: %+v %v", m, err)
	}
	if d, ok := cat.Default("local"); !ok || d.Name != "qwen" {
		t.Fatalf("Default after add: %+v %v", d, ok)
	}
	// Replace by (provider, name) keeps the catalog consistent.
	if err := cat.Add(llms.Model{Provider: "local", Name: "qwen", ID: "qwen3:14b", Aliases: []string{"qq"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.Resolve("q"); err == nil {
		t.Fatal("old alias should be gone")
	}
	m, _ = cat.Resolve("qq")
	if m.ID != "qwen3:14b" || len(cat.Models()) != 3 {
		t.Fatalf("replace: %+v, %d models", m, len(cat.Models()))
	}
	// Collisions are errors.
	err = cat.Add(llms.Model{Provider: "local", Name: "other", Aliases: []string{"Large"}})
	if err == nil || !strings.Contains(err.Error(), "collides") {
		t.Fatalf("expected collision, got %v", err)
	}
	if err := cat.Add(llms.Model{Provider: "ghost", Name: "x"}); err == nil {
		t.Fatal("unknown provider should fail")
	}
	// Curated overlay: replace acme-large's label without touching aliases.
	if err := cat.Add(llms.Model{Provider: "acme", Name: "acme-large", Label: "flagship"}); err != nil {
		t.Fatal(err)
	}
	m, _ = cat.Resolve("acme-large")
	if m.Label != "flagship" || m.ID != "acme-large" {
		t.Fatalf("overlay: %+v", m)
	}
}

func TestCatalogOpen(t *testing.T) {
	acme, local := testProviders()
	cat := llms.NewCatalog(acme, local)
	t.Setenv("ACME_API_KEY", "")
	if _, _, err := cat.Open(context.Background(), "large", llms.Options{}); !errors.Is(err, llms.ErrNoCredential) {
		t.Fatalf("expected ErrNoCredential, got %v", err)
	}
	t.Setenv("ACME_API_KEY", "k")
	s, m, err := cat.Open(context.Background(), "large", llms.Options{})
	if err != nil || m.ID != "acme-large" {
		t.Fatalf("open: %v %+v", err, m)
	}
	if s.(*stubStreamer).opts.APIKey != "k" {
		t.Fatal("provider should read env")
	}
	s, _, err = cat.Open(context.Background(), "local/x", llms.Options{BaseURL: "http://h"})
	if err != nil || s.(*stubStreamer).opts.BaseURL != "http://h" {
		t.Fatalf("options passthrough: %v", err)
	}
}
