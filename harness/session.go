package harness

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/mackross/agentloom/llms"
	"github.com/mackross/agentloom/threads"
)

// Session is a live model selection. Switching models re-applies the
// session's effort and service tier preferences and remembers the choice; tuning is
// validated against what the model accepts and remembered too.
type Session struct {
	h        *Harness
	mu       sync.Mutex
	sel      *selection
	onSwitch []func(*Session)
}

// Streamer returns the current streamer.
func (s *Session) Streamer() threads.LLMStreamer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sel.streamer
}

// Model describes the current model.
func (s *Session) Model() llms.Model {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.sel.model
	m.ServiceTiers = append([]llms.ServiceTier(nil), m.ServiceTiers...)
	return m
}

// Credential says how the current model is authenticated: an environment
// variable name, "<name> subscription", "stored api_key", "secret", or "none".
func (s *Session) Credential() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sel.credential
}

// Effort returns the current effort level.
func (s *Session) Effort() llms.Effort {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sel.effort
}

// Fast reports whether the legacy priority tier is on (not every premium tier).
// Deprecated: use ServiceTier.
func (s *Session) Fast() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sel.fast
}

// OnSwitch registers fn to run after every successful Switch.
func (s *Session) OnSwitch(fn func(*Session)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onSwitch = append(s.onSwitch, fn)
}

// Switch changes the current model. Effort and legacy fast carry over when the
// new model accepts them. A multiplier ceiling is re-resolved; an explicit tier
// is reset on a different provider/model. The model becomes the remembered default and the
// remembered model for its provider. Switching to the current model is a
// no-op.
func (s *Session) Switch(ctx context.Context, name string) error {
	s.mu.Lock()
	effort, fast, cur := s.sel.effort, s.sel.fast, s.sel.model
	preference := s.sel.tierPreference
	s.mu.Unlock()

	m, err := s.h.resolve(name)
	if err != nil {
		return err
	}
	if m.Provider == cur.Provider && strings.EqualFold(m.Name, cur.Name) {
		return nil
	}
	next, err := s.h.open(ctx, name, effort, fast, preference)
	if err != nil {
		return err
	}
	if err := s.h.save(ctx, func(st *Settings) error {
		st.Default = next.model.Name
		st.setProviderModel(next.model.Provider, next.model.Name)
		st.Effort = next.effort
		st.Fast = next.fast && next.tierPreference == (ServiceTierPreference{})
		st.ServiceTier = next.tierPreference
		return nil
	}); err != nil {
		return err
	}
	s.mu.Lock()
	s.sel = next
	handlers := make([]func(*Session), len(s.onSwitch))
	copy(handlers, s.onSwitch)
	s.mu.Unlock()
	for _, fn := range handlers {
		fn(s)
	}
	return nil
}

// SetEffort changes the effort level. The error names the levels the model
// accepts.
func (s *Session) SetEffort(e llms.Effort) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.sel.model
	if !m.AcceptsEffort(e) {
		if len(m.Efforts) == 0 {
			return fmt.Errorf("%s has no effort control", m.Name)
		}
		return fmt.Errorf("effort %s is not supported by %s; use default, %s", e, m.Name, joinEfforts(m.Efforts))
	}
	setter, ok := s.sel.streamer.(llms.EffortSetter)
	if !ok {
		if e == llms.EffortDefault {
			return nil
		}
		return fmt.Errorf("%s has no effort control", m.Name)
	}
	if err := setter.SetEffort(e); err != nil {
		return err
	}
	s.sel.effort = e
	return s.h.save(context.Background(), func(st *Settings) error {
		st.Effort = e
		return nil
	})
}

// SetFast selects the legacy priority tier or restores provider defaults,
// clearing any explicit tier or multiplier preference.
// Deprecated: use SetServiceTier or SetServiceTierForMultiplier.
func (s *Session) SetFast(on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.sel.model
	setter, ok := s.sel.streamer.(llms.FastSetter)
	_, tierOK := s.sel.streamer.(llms.ServiceTierSetter)
	if on && (!m.Fast || (!ok && !(tierOK && m.FastServiceTier != ""))) {
		return fmt.Errorf("fast mode is not supported by %s", m.Name)
	}
	var id llms.ServiceTierID
	if on {
		id = m.FastServiceTier
	}
	if tierOK && (!on || id != "") {
		if err := applyServiceTier(s.sel, id); err != nil {
			return err
		}
	} else if ok {
		if err := setter.SetFast(on); err != nil {
			return err
		}
	}
	if err := s.h.save(context.Background(), func(st *Settings) error {
		st.Fast = on
		st.ServiceTier = ServiceTierPreference{}
		return nil
	}); err != nil {
		return errors.Join(err, restoreServiceTier(s.sel))
	}
	s.sel.fast = on
	s.sel.tier = id
	s.sel.tierPreference = ServiceTierPreference{}
	return nil
}

func joinEfforts(efforts []llms.Effort) string {
	parts := make([]string, len(efforts))
	for i, e := range efforts {
		parts[i] = e.String()
	}
	return strings.Join(parts, ", ")
}
