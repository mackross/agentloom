package harness

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mackross/agentloom/llms"
	"github.com/mackross/agentloom/threads"
)

type stub struct {
	threads.LLMStreamer
	model  llms.Model
	opts   llms.Options
	effort llms.Effort
	fast   bool
}

func (s *stub) SetEffort(e llms.Effort) error { s.effort = e; return nil }
func (s *stub) SetFast(on bool) error         { s.fast = on; return nil }

type signIn struct{ tok *llms.Token }

func (signIn) Name() string { return "Acme Plus" }
func (s signIn) SignIn(context.Context, func(llms.SignInPrompt)) (*llms.Token, error) {
	return s.tok, nil
}

type countingStore struct {
	Store
	saves atomic.Int32
}

func (c *countingStore) Save(ctx context.Context, fn func(*Settings) error) error {
	c.saves.Add(1)
	return c.Store.Save(ctx, fn)
}

func providers() []*llms.Provider {
	open := func(_ context.Context, m llms.Model, o llms.Options) (threads.LLMStreamer, error) {
		return &stub{model: m, opts: o}, nil
	}
	acme := &llms.Provider{
		ID: "acme", Env: []string{"ACME_API_KEY"}, Subscription: signIn{tok: &llms.Token{Access: "tok"}},
		Models: []llms.Model{
			{ID: "acme-large", Aliases: []string{"large"}, Efforts: []llms.Effort{llms.EffortLow, llms.EffortHigh}, Fast: true},
			{ID: "acme-small", Aliases: []string{"small"}},
		},
		Match: llms.Prefix("acme-"),
		Open:  open,
	}
	local := &llms.Provider{ID: "local", Open: open}
	return []*llms.Provider{acme, local}
}

func openTest(t *testing.T, initial Settings) (*Harness, *countingStore) {
	t.Helper()
	t.Setenv("ACME_API_KEY", "env-key")
	store := &countingStore{Store: Memory(initial)}
	h, err := Open(context.Background(), Options{Providers: providers(), Store: store})
	if err != nil {
		t.Fatal(err)
	}
	return h, store
}

func TestOpenNeverWrites(t *testing.T) {
	h, store := openTest(t, Settings{Models: []llms.Model{{Provider: "local", Name: "qwen", ID: "qwen3:8b", Options: map[string]any{"host": "h"}}}})
	if _, err := h.Streamer(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if store.saves.Load() != 0 {
		t.Fatalf("saves = %d", store.saves.Load())
	}
	m, err := h.Session(context.Background(), "qwen")
	if err != nil || m.Model().Options["host"] != "h" || m.Credential() != "none" {
		t.Fatalf("configured model: %v %+v %s", err, m.Model(), m.Credential())
	}
}

func TestResolveDefaults(t *testing.T) {
	h, _ := openTest(t, Settings{Default: "small", Providers: map[string]ProviderSettings{"acme": {Model: "large"}}})
	ses, err := h.Session(context.Background(), "")
	if err != nil || ses.Model().ID != "acme-small" {
		t.Fatalf("default: %v %+v", err, ses.Model())
	}
	ses, err = h.Session(context.Background(), "acme")
	if err != nil || ses.Model().ID != "acme-large" {
		t.Fatalf("provider remembered: %v %+v", err, ses.Model())
	}
	if _, err := h.Session(context.Background(), "local"); err == nil || !strings.Contains(err.Error(), "no local models") {
		t.Fatalf("provider without models: %v", err)
	}
	if _, err := h.Session(context.Background(), "nope"); !errors.Is(err, llms.ErrUnknownModel) {
		t.Fatalf("unknown: %v", err)
	}
}

func TestCredentialPrecedence(t *testing.T) {
	h, _ := openTest(t, Settings{Auth: map[string]AuthEntry{"acme": {APIKey: "stored"}}})
	ses, _ := h.Session(context.Background(), "large")
	if ses.Credential() != "ACME_API_KEY" || ses.Streamer().(*stub).opts.APIKey != "env-key" {
		t.Fatalf("env should win: %s", ses.Credential())
	}
	t.Setenv("ACME_API_KEY", "")
	ses, _ = h.Session(context.Background(), "large")
	if ses.Credential() != "stored api_key" || ses.Streamer().(*stub).opts.APIKey != "stored" {
		t.Fatalf("stored key: %s", ses.Credential())
	}
	if err := h.SignIn(context.Background(), "acme", nil); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ACME_API_KEY", "env-key")
	ses, _ = h.Session(context.Background(), "large")
	if ses.Credential() != "Acme Plus subscription" || ses.Streamer().(*stub).opts.Token.Access != "tok" {
		t.Fatalf("token should beat env: %s", ses.Credential())
	}
	if err := h.SignOut(context.Background(), "acme"); err != nil {
		t.Fatal(err)
	}
	ses, _ = h.Session(context.Background(), "large")
	if ses.Credential() != "ACME_API_KEY" {
		t.Fatalf("after sign out: %s", ses.Credential())
	}
	if err := h.SignIn(context.Background(), "local", nil); err == nil {
		t.Fatal("local has no sign-in")
	}

	h2, _ := openTest(t, Settings{})
	h2.secret = func(name string) (string, bool) { return "vault-" + name, true }
	ses, _ = h2.Session(context.Background(), "large")
	if ses.Credential() != "secret" || ses.Streamer().(*stub).opts.APIKey != "vault-acme" {
		t.Fatalf("secret hook: %s", ses.Credential())
	}
	if h2.Secret("brave") != "vault-brave" {
		t.Fatal("Secret hook")
	}

	t.Setenv("ACME_API_KEY", "")
	h3, _ := openTest(t, Settings{})
	t.Setenv("ACME_API_KEY", "")
	_, err := h3.Session(context.Background(), "large")
	if !errors.Is(err, llms.ErrNoCredential) || !strings.Contains(err.Error(), "set ACME_API_KEY or sign in with acme") {
		t.Fatalf("missing credential error: %v", err)
	}
}

func TestSessionTuning(t *testing.T) {
	h, store := openTest(t, Settings{Effort: llms.EffortHigh, Fast: true})
	ses, err := h.Session(context.Background(), "large")
	if err != nil {
		t.Fatal(err)
	}
	st := ses.Streamer().(*stub)
	if ses.Effort() != llms.EffortHigh || st.effort != llms.EffortHigh || !ses.Fast() || !st.fast {
		t.Fatalf("remembered tuning not applied: %s %v", ses.Effort(), ses.Fast())
	}
	err = ses.SetEffort(llms.EffortMax)
	if err == nil || err.Error() != "effort max is not supported by acme-large; use default, low, high" {
		t.Fatalf("unsupported effort: %v", err)
	}
	if err := ses.SetEffort(llms.EffortLow); err != nil || st.effort != llms.EffortLow {
		t.Fatalf("SetEffort: %v", err)
	}
	if s, _ := store.Load(context.Background()); s.Effort != llms.EffortLow {
		t.Fatalf("effort not remembered: %+v", s)
	}

	// Switching to a model without effort or fast drops both to default.
	var switched []string
	ses.OnSwitch(func(s *Session) { switched = append(switched, s.Model().Name) })
	if err := ses.Switch(context.Background(), "small"); err != nil {
		t.Fatal(err)
	}
	if ses.Effort() != llms.EffortDefault || ses.Fast() || len(switched) != 1 {
		t.Fatalf("after switch: %s %v %v", ses.Effort(), ses.Fast(), switched)
	}
	if err := ses.SetEffort(llms.EffortLow); err == nil || !strings.Contains(err.Error(), "no effort control") {
		t.Fatalf("expected no effort control, got %v", err)
	}
	if err := ses.SetFast(true); err == nil {
		t.Fatal("small should not support fast")
	}
	if err := ses.SetFast(false); err != nil {
		t.Fatalf("SetFast(false) must succeed: %v", err)
	}
	s, _ := store.Load(context.Background())
	if s.Default != "acme-small" || s.Providers["acme"].Model != "acme-small" {
		t.Fatalf("switch not remembered: %+v", s)
	}
	// Same model is a no-op and fires nothing.
	if err := ses.Switch(context.Background(), "SMALL"); err != nil || len(switched) != 1 {
		t.Fatalf("no-op switch: %v %v", err, switched)
	}
	// Switching back restores the remembered effort? No: the session's own
	// (default) carries over; remembered settings only seed new sessions.
	if err := ses.Switch(context.Background(), "large"); err != nil {
		t.Fatal(err)
	}
	if ses.Effort() != llms.EffortDefault || ses.Fast() {
		t.Fatalf("session tuning should carry over: %s %v", ses.Effort(), ses.Fast())
	}
}
