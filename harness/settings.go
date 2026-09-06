package harness

import (
	"context"
	"sync"

	"github.com/mackross/agentloom/llms"
)

// Settings is everything a harness remembers between runs.
type Settings struct {
	// Default is the model used when none is named: a model name, alias, or
	// provider id.
	Default string
	Effort  llms.Effort
	Fast    bool
	// Providers holds per-provider settings keyed by provider id.
	Providers map[string]ProviderSettings
	// Models are configured overlays added to the catalog.
	Models []llms.Model
	// Auth holds credentials keyed by provider id (or any other secret name).
	Auth map[string]AuthEntry
}

// ProviderSettings remembers the last model used with a provider.
type ProviderSettings struct {
	Model string
}

// AuthEntry is a stored credential.
type AuthEntry struct {
	APIKey string
	Token  *llms.Token
}

// Store loads and saves Settings. Save hands the current settings to fn and
// persists whatever fn changed.
type Store interface {
	Load(ctx context.Context) (Settings, error)
	Save(ctx context.Context, fn func(*Settings) error) error
}

// Memory returns an in-memory Store seeded with initial. It is intended for
// tests and for harnesses that manage persistence themselves.
func Memory(initial Settings) Store {
	return &memory{settings: initial.clone()}
}

type memory struct {
	mu       sync.Mutex
	settings Settings
}

func (m *memory) Load(context.Context) (Settings, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.settings.clone(), nil
}

func (m *memory) Save(_ context.Context, fn func(*Settings) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := m.settings.clone()
	if err := fn(&next); err != nil {
		return err
	}
	m.settings = next
	return nil
}

func (s Settings) clone() Settings {
	out := s
	out.Providers = make(map[string]ProviderSettings, len(s.Providers))
	for k, v := range s.Providers {
		out.Providers[k] = v
	}
	out.Auth = make(map[string]AuthEntry, len(s.Auth))
	for k, v := range s.Auth {
		if v.Token != nil {
			tok := *v.Token
			v.Token = &tok
		}
		out.Auth[k] = v
	}
	out.Models = make([]llms.Model, len(s.Models))
	for i, m := range s.Models {
		m.Aliases = append([]string(nil), m.Aliases...)
		m.Efforts = append([]llms.Effort(nil), m.Efforts...)
		if m.Options != nil {
			opts := make(map[string]any, len(m.Options))
			for k, v := range m.Options {
				opts[k] = v
			}
			m.Options = opts
		}
		out.Models[i] = m
	}
	return out
}

func (s Settings) provider(id string) ProviderSettings {
	if s.Providers == nil {
		return ProviderSettings{}
	}
	return s.Providers[id]
}

func (s *Settings) setProviderModel(id, model string) {
	if s.Providers == nil {
		s.Providers = map[string]ProviderSettings{}
	}
	ps := s.Providers[id]
	ps.Model = model
	s.Providers[id] = ps
}

func (s Settings) auth(id string) AuthEntry {
	if s.Auth == nil {
		return AuthEntry{}
	}
	return s.Auth[id]
}

func (s *Settings) setAuth(id string, fn func(*AuthEntry)) {
	if s.Auth == nil {
		s.Auth = map[string]AuthEntry{}
	}
	e := s.Auth[id]
	fn(&e)
	s.Auth[id] = e
}
