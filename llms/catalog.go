package llms

import (
	"context"
	"fmt"
	"strings"

	"github.com/mackross/agentloom/threads"
)

// Catalog indexes providers and their models and resolves user-typed names.
type Catalog struct {
	providers []*Provider
	byID      map[string]*Provider
	models    []Model
	index     map[string]int // lower-cased name or alias => position in models
}

// NewCatalog builds a catalog over providers. Curated models are stamped with
// their provider id and default Name to ID.
func NewCatalog(providers ...*Provider) *Catalog {
	c := &Catalog{byID: map[string]*Provider{}, index: map[string]int{}}
	for _, p := range providers {
		if p == nil || p.ID == "" {
			continue
		}
		if _, dup := c.byID[p.ID]; dup {
			continue
		}
		c.providers = append(c.providers, p)
		c.byID[p.ID] = p
	}
	for _, p := range c.providers {
		for _, m := range p.Models {
			m.Provider = p.ID
			// Curated models must not collide; treat as a programming error.
			if err := c.Add(m); err != nil {
				panic(err)
			}
		}
	}
	return c
}

// Add adds or replaces models. A model replaces an existing one with the same
// provider and name. Names and aliases must be unique across the catalog,
// case-insensitively.
func (c *Catalog) Add(models ...Model) error {
	for _, m := range models {
		m = stamp(m)
		if m.Provider == "" {
			return fmt.Errorf("llms: model %q has no provider", m.Name)
		}
		if _, ok := c.byID[m.Provider]; !ok {
			return fmt.Errorf("llms: model %q: unknown provider %q", m.Name, m.Provider)
		}
		if m.Name == "" {
			return fmt.Errorf("llms: %s model has no name", m.Provider)
		}
		pos := -1
		if i, ok := c.index[key(m.Name)]; ok && c.models[i].Provider == m.Provider && key(c.models[i].Name) == key(m.Name) {
			pos = i
		}
		for _, k := range keys(m) {
			if i, ok := c.index[k]; ok && i != pos {
				return fmt.Errorf("llms: %s model %q: %q collides with %s model %q", m.Provider, m.Name, k, c.models[i].Provider, c.models[i].Name)
			}
		}
		if pos >= 0 {
			for _, k := range keys(c.models[pos]) {
				delete(c.index, k)
			}
			c.models[pos] = m
		} else {
			pos = len(c.models)
			c.models = append(c.models, m)
		}
		for _, k := range keys(m) {
			c.index[k] = pos
		}
	}
	return nil
}

// Resolve finds a model by "provider/id", by name or alias (case-insensitive),
// or by asking each provider's Match to claim the id.
func (c *Catalog) Resolve(name string) (Model, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Model{}, fmt.Errorf("%w: empty name", ErrUnknownModel)
	}
	if pid, id, ok := strings.Cut(name, "/"); ok && id != "" {
		if p, found := c.byID[key(pid)]; found {
			if i, ok := c.index[key(id)]; ok && c.models[i].Provider == p.ID {
				return c.models[i], nil
			}
			if p.Match != nil {
				if m, ok := p.Match(id); ok {
					return stampFor(p, m, id), nil
				}
			}
			return stampFor(p, Model{}, id), nil
		}
	}
	if i, ok := c.index[key(name)]; ok {
		return c.models[i], nil
	}
	for _, p := range c.providers {
		if p.Match == nil {
			continue
		}
		if m, ok := p.Match(name); ok {
			return stampFor(p, m, name), nil
		}
	}
	return Model{}, fmt.Errorf("%w: %q", ErrUnknownModel, name)
}

// Open resolves name and opens a streamer for it.
func (c *Catalog) Open(ctx context.Context, name string, o Options) (threads.LLMStreamer, Model, error) {
	m, err := c.Resolve(name)
	if err != nil {
		return nil, Model{}, err
	}
	p := c.byID[m.Provider]
	if p.Open == nil {
		return nil, m, fmt.Errorf("llms: provider %q cannot open models", p.ID)
	}
	s, err := p.Open(ctx, m, o)
	if err != nil {
		return nil, m, err
	}
	return s, m, nil
}

// Models lists every curated and added model in order.
func (c *Catalog) Models() []Model {
	out := make([]Model, len(c.models))
	copy(out, c.models)
	return out
}

// Providers lists providers in registration order.
func (c *Catalog) Providers() []*Provider {
	out := make([]*Provider, len(c.providers))
	copy(out, c.providers)
	return out
}

// Provider looks up a provider by id.
func (c *Catalog) Provider(id string) (*Provider, bool) {
	p, ok := c.byID[key(id)]
	return p, ok
}

// Default returns the default model for a provider: the first catalog model
// belonging to it.
func (c *Catalog) Default(provider string) (Model, bool) {
	provider = key(provider)
	for _, m := range c.models {
		if m.Provider == provider {
			return m, true
		}
	}
	return Model{}, false
}

func stamp(m Model) Model {
	m.Provider = key(m.Provider)
	m.Name = strings.TrimSpace(m.Name)
	m.ID = strings.TrimSpace(m.ID)
	if m.Name == "" {
		m.Name = m.ID
	}
	if m.ID == "" {
		m.ID = m.Name
	}
	return m
}

func stampFor(p *Provider, m Model, id string) Model {
	m.Provider = p.ID
	if m.ID == "" {
		m.ID = id
	}
	return stamp(m)
}

func key(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func keys(m Model) []string {
	out := []string{key(m.Name)}
	for _, a := range m.Aliases {
		if k := key(a); k != "" && k != key(m.Name) {
			out = append(out, k)
		}
	}
	return out
}
