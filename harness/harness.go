// Package harness gives agent harnesses the pieces they otherwise rebuild:
// shared configuration files, provider credentials and subscription sign-in,
// and a live model selection whose effort and fast settings are remembered.
//
//	h, err := harness.Open(ctx, harness.Options{App: "weaver"})
//	ses, err := h.Session(ctx, "")
//	exec := threads.NewThreadExecutor(ses.Streamer())
//	ses.Switch(ctx, "sol")
//	ses.SetEffort(llms.EffortHigh)
package harness

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/mackross/agentloom/llms"
	"github.com/mackross/agentloom/llms/providers/all"
	"github.com/mackross/agentloom/threads"
)

// Options configures Open. The zero value uses the shared configuration
// directory and every built-in provider.
type Options struct {
	// App names the harness. It adds a models.<app>.loom.toml overlay that also
	// receives writes. Empty means shared settings only.
	App string
	// Dir is the configuration directory. Empty uses DefaultDir.
	Dir string
	// Providers replaces the built-in providers.
	Providers []*llms.Provider
	// Store replaces the file-backed store built from Dir and App.
	Store Store
	// Secret supplies credentials from elsewhere (a keychain, say). It is
	// consulted before stored tokens, the environment, and stored keys.
	Secret func(name string) (string, bool)
}

// Harness is an opened configuration. Open never writes; only Session
// methods, SignIn, and SignOut do.
type Harness struct {
	cat    *llms.Catalog
	store  Store
	secret func(string) (string, bool)

	mu       sync.Mutex
	settings Settings
}

// Open loads settings and builds the catalog.
func Open(ctx context.Context, o Options) (*Harness, error) {
	store := o.Store
	if store == nil {
		dir := o.Dir
		if dir == "" {
			var err error
			if dir, err = DefaultDir(); err != nil {
				return nil, err
			}
		}
		store = Files(dir, o.App)
	}
	settings, err := store.Load(ctx)
	if err != nil {
		return nil, err
	}
	providers := o.Providers
	if providers == nil {
		providers = all.Providers()
	}
	cat := llms.NewCatalog(providers...)
	if err := cat.Add(settings.Models...); err != nil {
		return nil, fmt.Errorf("harness: configured models: %w", err)
	}
	return &Harness{cat: cat, store: store, secret: o.Secret, settings: settings}, nil
}

// Models lists every model the harness can select.
func (h *Harness) Models() []llms.Model { return h.cat.Models() }

// Streamer opens a one-shot streamer for name with the remembered effort and
// fast settings applied. It writes nothing.
func (h *Harness) Streamer(ctx context.Context, name string) (threads.LLMStreamer, error) {
	s := h.current()
	sel, err := h.open(ctx, name, s.Effort, s.Fast)
	if err != nil {
		return nil, err
	}
	return sel.streamer, nil
}

// Session opens name (or the remembered default when name is empty) and
// returns a live selection that can be switched and tuned.
func (h *Harness) Session(ctx context.Context, name string) (*Session, error) {
	s := h.current()
	sel, err := h.open(ctx, name, s.Effort, s.Fast)
	if err != nil {
		return nil, err
	}
	return &Session{h: h, sel: sel}, nil
}

// SignIn runs the provider's subscription sign-in, showing each prompt with
// show, and stores the resulting token. A stored token is used ahead of
// environment variables until SignOut.
func (h *Harness) SignIn(ctx context.Context, provider string, show func(llms.SignInPrompt)) error {
	p, ok := h.cat.Provider(provider)
	if !ok {
		return fmt.Errorf("harness: unknown provider %q", provider)
	}
	if p.Subscription == nil {
		return fmt.Errorf("harness: %s has no subscription sign-in", p.ID)
	}
	tok, err := p.Subscription.SignIn(ctx, show)
	if err != nil {
		return err
	}
	return h.save(ctx, func(s *Settings) error {
		s.setAuth(p.ID, func(e *AuthEntry) { e.Token = tok })
		return nil
	})
}

// SignOut forgets the provider's stored subscription token.
func (h *Harness) SignOut(ctx context.Context, provider string) error {
	p, ok := h.cat.Provider(provider)
	if !ok {
		return fmt.Errorf("harness: unknown provider %q", provider)
	}
	return h.save(ctx, func(s *Settings) error {
		s.setAuth(p.ID, func(e *AuthEntry) { e.Token = nil })
		return nil
	})
}

// Secret returns a stored API key by name, for services that are not model
// providers (a search API, say). Options.Secret is consulted first.
func (h *Harness) Secret(name string) string {
	if h.secret != nil {
		if v, ok := h.secret(name); ok {
			return v
		}
	}
	return h.current().auth(name).APIKey
}

type selection struct {
	model      llms.Model
	streamer   threads.LLMStreamer
	credential string
	effort     llms.Effort
	fast       bool
}

func (h *Harness) current() Settings {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.settings
}

func (h *Harness) save(ctx context.Context, fn func(*Settings) error) error {
	if err := h.store.Save(ctx, fn); err != nil {
		return err
	}
	s, err := h.store.Load(ctx)
	if err != nil {
		return err
	}
	h.mu.Lock()
	h.settings = s
	h.mu.Unlock()
	return nil
}

// resolve maps "" to the remembered default, a provider id to that provider's
// remembered or first model, and anything else through the catalog.
func (h *Harness) resolve(name string) (llms.Model, error) {
	name = strings.TrimSpace(name)
	s := h.current()
	if name == "" {
		name = s.Default
	}
	if name == "" {
		if len(h.cat.Models()) == 0 {
			return llms.Model{}, errors.New("harness: no models configured")
		}
		return h.cat.Models()[0], nil
	}
	if p, ok := h.cat.Provider(name); ok {
		if remembered := s.provider(p.ID).Model; remembered != "" {
			if m, err := h.cat.Resolve(remembered); err == nil && m.Provider == p.ID {
				return m, nil
			}
		}
		if m, ok := h.cat.Default(p.ID); ok {
			return m, nil
		}
		return llms.Model{}, fmt.Errorf("harness: no %s models configured", p.ID)
	}
	return h.cat.Resolve(name)
}

func (h *Harness) open(ctx context.Context, name string, effort llms.Effort, fast bool) (*selection, error) {
	m, err := h.resolve(name)
	if err != nil {
		return nil, err
	}
	p, _ := h.cat.Provider(m.Provider)
	opts, credential, err := h.credentials(ctx, p)
	if err != nil {
		return nil, err
	}
	if p.Open == nil {
		return nil, fmt.Errorf("harness: %s cannot open models", p.ID)
	}
	streamer, err := p.Open(ctx, m, opts)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", m.Name, err)
	}
	sel := &selection{model: m, streamer: streamer, credential: credential}
	if !m.AcceptsEffort(effort) {
		effort = llms.EffortDefault
	}
	if effort != llms.EffortDefault {
		if es, ok := streamer.(llms.EffortSetter); ok {
			if err := es.SetEffort(effort); err != nil {
				return nil, err
			}
			sel.effort = effort
		}
	}
	if fast && m.Fast {
		if fs, ok := streamer.(llms.FastSetter); ok {
			if err := fs.SetFast(true); err != nil {
				return nil, err
			}
			sel.fast = true
		}
	}
	return sel, nil
}

// credentials resolves how to authenticate p: Options.Secret, then a stored
// subscription token, then the environment, then a stored API key.
func (h *Harness) credentials(ctx context.Context, p *llms.Provider) (llms.Options, string, error) {
	var o llms.Options
	if h.secret != nil {
		if key, ok := h.secret(p.ID); ok && key != "" {
			o.APIKey = key
			return o, "secret", nil
		}
	}
	entry := h.current().auth(p.ID)
	if entry.Token != nil {
		o.Token = entry.Token
		o.OnRefresh = func(tok *llms.Token) error {
			return h.save(context.WithoutCancel(ctx), func(s *Settings) error {
				s.setAuth(p.ID, func(e *AuthEntry) { e.Token = tok })
				return nil
			})
		}
		label := "subscription"
		if p.Subscription != nil {
			label = p.Subscription.Name() + " subscription"
		}
		return o, label, nil
	}
	for _, key := range p.Env {
		if llms.EnvAPIKey([]string{key}) != "" {
			o.APIKey = llms.EnvAPIKey([]string{key})
			return o, key, nil
		}
	}
	if entry.APIKey != "" {
		o.APIKey = entry.APIKey
		return o, "stored api_key", nil
	}
	if len(p.Env) == 0 && p.Subscription == nil {
		return o, "none", nil
	}
	hint := strings.Join(p.Env, " or ")
	if p.Subscription != nil {
		if hint != "" {
			hint += " or "
		}
		hint += "sign in with " + p.ID
	}
	return o, "", fmt.Errorf("%s: %w: set %s", p.ID, llms.ErrNoCredential, hint)
}
