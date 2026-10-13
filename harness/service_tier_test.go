package harness

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/mackross/agentloom/llms"
	"github.com/mackross/agentloom/threads"
)

type tierStub struct {
	threads.LLMStreamer
	tier llms.ServiceTierID
	fail bool
}

func (s *tierStub) SetServiceTier(id llms.ServiceTierID) error {
	if s.fail {
		return errors.New("tier unavailable")
	}
	s.tier = id
	return nil
}

func tierProviders() []*llms.Provider {
	open := func(context.Context, llms.Model, llms.Options) (threads.LLMStreamer, error) {
		return &tierStub{}, nil
	}
	return []*llms.Provider{
		{ID: "a", Open: open, Models: []llms.Model{
			{ID: "one", Fast: true, FastServiceTier: "priority", ServiceTiers: []llms.ServiceTier{
				{ID: "ultra", Label: "Ultra", MaxMultiplier: 6},
				{ID: "priority", Label: "Fast", MaxMultiplier: 2},
				{ID: "base", Label: "Standard", MaxMultiplier: 1},
			}},
			{ID: "unknown"},
		}},
		{ID: "b", Open: open, Models: []llms.Model{
			{ID: "two", ServiceTiers: []llms.ServiceTier{
				{ID: "express", Label: "Express", MaxMultiplier: 3},
				{ID: "normal", Label: "Normal", MaxMultiplier: 1},
			}},
		}},
	}
}

func tierHarness(t *testing.T, store Store) *Harness {
	t.Helper()
	h, err := Open(context.Background(), Options{Providers: tierProviders(), Store: store})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestSessionTierBudgetSwitchAndReopen(t *testing.T) {
	ctx := context.Background()
	store := Memory(Settings{})
	h := tierHarness(t, store)
	s, err := h.Session(ctx, "one")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetServiceTierForMultiplier(2); err != nil {
		t.Fatal(err)
	}
	if s.ServiceTier() != "priority" || s.ServiceTierMultiplier() != 2 || !s.Fast() {
		t.Fatal("wrong initial tier selection")
	}
	if err := s.Switch(ctx, "two"); err != nil {
		t.Fatal(err)
	}
	if s.ServiceTier() != "normal" || s.ServiceTierMultiplier() != 2 || s.Streamer().(*tierStub).tier != "normal" {
		t.Fatal("ceiling must be re-resolved, not the old provider's ID copied")
	}
	reopened, err := tierHarness(t, store).Session(ctx, "")
	if err != nil || reopened.ServiceTier() != "normal" || reopened.ServiceTierMultiplier() != 2 {
		t.Fatalf("preference not persisted: %v", err)
	}
	if err := s.Switch(ctx, "unknown"); err != nil {
		t.Fatalf("switch to tierless model: %v", err)
	}
	if s.Model().ID != "unknown" || s.ServiceTier() != "" || s.ServiceTierMultiplier() != 2 || s.Fast() {
		t.Fatal("tierless model must use normal service and retain the ceiling")
	}
	reopened, err = tierHarness(t, store).Session(ctx, "")
	if err != nil || reopened.Model().ID != "unknown" || reopened.ServiceTier() != "" || reopened.ServiceTierMultiplier() != 2 {
		t.Fatalf("tierless preference not persisted: %v", err)
	}
	if err := s.Switch(ctx, "one"); err != nil || s.ServiceTier() != "priority" {
		t.Fatalf("ceiling lost after switching through a tierless model: %v", err)
	}
	if err := s.SetServiceTierForMultiplier(6); err != nil {
		t.Fatal(err)
	}
	if err := s.Switch(ctx, "one"); err != nil || s.ServiceTier() != "ultra" {
		t.Fatalf("higher ceiling did not carry over: %v", err)
	}
}

func TestSessionExplicitTierScopeAndLegacy(t *testing.T) {
	ctx := context.Background()
	store := Memory(Settings{Fast: true})
	s, err := tierHarness(t, store).Session(ctx, "one")
	if err != nil || s.ServiceTier() != "priority" {
		t.Fatalf("legacy fast setting: %v", err)
	}
	if err := s.SetServiceTier("ultra"); err != nil {
		t.Fatal(err)
	}
	if s.Fast() || s.ServiceTierMultiplier() != 0 {
		t.Fatal("explicit ultra must not be reported as legacy fast")
	}
	reopened, err := tierHarness(t, store).Session(ctx, "one")
	if err != nil || reopened.ServiceTier() != "ultra" {
		t.Fatalf("explicit preference not persisted: %v", err)
	}
	if err := s.Switch(ctx, "two"); err != nil || s.ServiceTier() != "" {
		t.Fatalf("explicit provider/model tier leaked across switch: %v", err)
	}
	if err := s.Switch(ctx, "one"); err != nil || s.ServiceTier() != "" {
		t.Fatalf("legacy fast resurrected after explicit preference reset: %v", err)
	}
	if err := s.SetServiceTierForMultiplier(6); err != nil {
		t.Fatal(err)
	}
	if err := s.SetFast(true); err != nil || s.ServiceTier() != "priority" || s.ServiceTierMultiplier() != 0 {
		t.Fatalf("legacy setter must replace budget preference: %v", err)
	}
	if err := s.SetFast(false); err != nil || s.ServiceTier() != "" {
		t.Fatalf("legacy off: %v", err)
	}
}

type failingTierStore struct {
	Store
	fail bool
}

func (s *failingTierStore) Save(ctx context.Context, fn func(*Settings) error) error {
	if s.fail {
		return errors.New("save failed")
	}
	return s.Store.Save(ctx, fn)
}

func TestSessionTierErrorsLeaveSelectionUnchanged(t *testing.T) {
	store := &failingTierStore{Store: Memory(Settings{})}
	s, err := tierHarness(t, store).Session(context.Background(), "one")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetServiceTier("base"); err != nil {
		t.Fatal(err)
	}
	for _, n := range []float64{0, -1, 0.5, math.NaN(), math.Inf(1)} {
		if err := s.SetServiceTierForMultiplier(n); err == nil {
			t.Errorf("accepted %g", n)
		}
	}
	if err := s.SetServiceTier("express"); err == nil {
		t.Fatal("accepted another provider's tier")
	}
	streamer := s.Streamer().(*tierStub)
	streamer.fail = true
	if err := s.SetServiceTier("ultra"); err == nil {
		t.Fatal("ignored setter failure")
	}
	streamer.fail = false
	store.fail = true
	if err := s.SetServiceTier("ultra"); err == nil {
		t.Fatal("ignored persistence failure")
	}
	if s.ServiceTier() != "base" || streamer.tier != "base" {
		t.Fatal("failed update changed live state")
	}
}

func TestTierCatalogAndMemoryIsolation(t *testing.T) {
	initial := Settings{Models: []llms.Model{{Provider: "a", ID: "custom", ServiceTiers: []llms.ServiceTier{{ID: "custom-tier", MaxMultiplier: 1}}}}}
	store := Memory(initial)
	initial.Models[0].ServiceTiers[0].ID = "mutated"
	h := tierHarness(t, store)
	options := h.ModelOptions(context.Background())
	if len(options[0].Model.ServiceTiers) != 3 || options[0].Model.ServiceTiers[0].Label != "Ultra" {
		t.Fatal("UI model options missing tier metadata")
	}
	loaded, _ := store.Load(context.Background())
	if loaded.Models[0].ServiceTiers[0].ID != "custom-tier" {
		t.Fatal("memory store retained input slice")
	}
	loaded.Models[0].ServiceTiers[0].ID = "mutated"
	loaded, _ = store.Load(context.Background())
	if loaded.Models[0].ServiceTiers[0].ID != "custom-tier" {
		t.Fatal("load leaked store slice")
	}
}

func TestFilesServiceTierRoundTripAndOverlayReset(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	body := `
[service_tier]
max_multiplier = 2

[[models]]
provider = "a"
id = "custom"
service_tiers = [
  {id = "boost", label = "Boost", max_multiplier = 2.5, notes = "Preview"},
  {id = "normal", label = "Normal", max_multiplier = 1},
]
`
	if err := os.WriteFile(filepath.Join(dir, "models.loom.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	store := Files(dir, "app")
	loaded, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ServiceTier.MaxMultiplier != 2 || len(loaded.Models[0].ServiceTiers) != 2 ||
		loaded.Models[0].ServiceTiers[0].MaxMultiplier != 2.5 || loaded.Models[0].ServiceTiers[0].Notes != "Preview" {
		t.Fatalf("decoded settings: %+v", loaded)
	}
	if _, leaked := loaded.Models[0].Options["service_tiers"]; leaked {
		t.Fatal("service tier metadata leaked into provider options")
	}
	for _, p := range []ServiceTierPreference{
		{Provider: "a", Model: "one", ID: "ultra"},
		{MaxMultiplier: 6},
		{},
	} {
		if err := store.Save(ctx, func(s *Settings) error { s.ServiceTier = p; return nil }); err != nil {
			t.Fatal(err)
		}
		got, err := store.Load(ctx)
		if err != nil || got.ServiceTier != p {
			t.Fatalf("roundtrip %v: %v, %v", p, got.ServiceTier, err)
		}
	}
	shared, err := Files(dir, "").Load(ctx)
	if err != nil || shared.ServiceTier.MaxMultiplier != 2 {
		t.Fatal("app write changed shared preference")
	}
}

func TestFilesRejectInvalidServiceTiers(t *testing.T) {
	for _, body := range []string{
		"service_tier = 'priority'",
		"[service_tier]\nmax_multiplier = -1",
		"[service_tier]\nmax_multiplier = nan",
		"[service_tier]\nmax_multiplier = inf",
		"[service_tier]\nmax_multiplier = '2'",
		"[service_tier]\nid = 'priority'",
		"[service_tier]\nid = 'priority'\nprovider = 'a'\nmodel = 'one'\nmax_multiplier = 2",
		"[[models]]\nprovider = 'a'\nid = 'one'\nservice_tiers = [{id='x'}, {id='x'}]",
		"[[models]]\nprovider = 'a'\nid = 'one'\nservice_tiers = [{id='x', max_multiplier=-1}]",
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "models.loom.toml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Files(dir, "").Load(context.Background()); err == nil {
			t.Errorf("accepted invalid config:\n%s", body)
		}
	}
}
