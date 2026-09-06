package harness

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/mackross/agentloom/llms"
	"github.com/mackross/agentloom/threads"
)

// Session is a live model selection. Switching models re-applies the
// session's effort and fast settings and remembers the choice; tuning is
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
	return s.sel.model
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

// Fast reports whether the priority tier is on.
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

// Switch changes the current model. Effort and fast carry over when the new
// model accepts them. The model becomes the remembered default and the
// remembered model for its provider. Switching to the current model is a
// no-op.
func (s *Session) Switch(ctx context.Context, name string) error {
	s.mu.Lock()
	effort, fast, cur := s.sel.effort, s.sel.fast, s.sel.model
	s.mu.Unlock()

	m, err := s.h.resolve(name)
	if err != nil {
		return err
	}
	if m.Provider == cur.Provider && strings.EqualFold(m.Name, cur.Name) {
		return nil
	}
	next, err := s.h.open(ctx, name, effort, fast)
	if err != nil {
		return err
	}
	if err := s.h.save(ctx, func(st *Settings) error {
		st.Default = next.model.Name
		st.setProviderModel(next.model.Provider, next.model.Name)
		st.Effort = next.effort
		st.Fast = next.fast
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

// SetFast turns the priority tier on or off. Turning it off always succeeds.
func (s *Session) SetFast(on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.sel.model
	setter, ok := s.sel.streamer.(llms.FastSetter)
	if on && (!m.Fast || !ok) {
		return fmt.Errorf("fast mode is not supported by %s", m.Name)
	}
	if ok {
		if err := setter.SetFast(on); err != nil {
			return err
		}
	}
	s.sel.fast = on
	return s.h.save(context.Background(), func(st *Settings) error {
		st.Fast = on
		return nil
	})
}

func joinEfforts(efforts []llms.Effort) string {
	parts := make([]string, len(efforts))
	for i, e := range efforts {
		parts[i] = e.String()
	}
	return strings.Join(parts, ", ")
}
