package llms

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/mackross/agentloom/threads"
)

// Model is a selectable model: curated by a provider, added from configuration,
// or synthesized from a wire id by Provider.Match.
type Model struct {
	Provider string
	// Name is what users type. It defaults to ID.
	Name string
	// ID is the model id sent on the wire.
	ID      string
	Aliases []string
	Label   string
	// Efforts lists the explicit levels this model accepts. nil means the model
	// has no effort control. EffortDefault is always accepted.
	Efforts []Effort
	// Fast reports whether the model has a priority tier.
	Fast bool
	// Options carries provider-specific settings (ollama: host, headers,
	// options, think, keep_alive). Unknown configuration keys land here.
	Options map[string]any
}

// AcceptsEffort reports whether e is EffortDefault or listed in Efforts.
func (m Model) AcceptsEffort(e Effort) bool {
	if e == EffortDefault {
		return true
	}
	for _, x := range m.Efforts {
		if x == e {
			return true
		}
	}
	return false
}

// Provider describes one model provider: its identity, how it is
// authenticated, the models it knows about, and how to open a streamer.
type Provider struct {
	ID string
	// Env lists credential environment variables in precedence order. A
	// provider with no Env and no Subscription needs no credential.
	Env []string
	// Subscription is the provider's sign-in flow, or nil.
	Subscription Subscription
	// Models are the curated models; the first is the provider default.
	// Catalog stamps Provider and defaults Name to ID.
	Models []Model
	// Match claims an uncurated wire id and describes it. nil never claims.
	Match func(id string) (Model, bool)
	// Open builds a streamer for m. With an empty Options.APIKey and nil
	// Options.Token the provider reads its Env itself.
	Open func(ctx context.Context, m Model, o Options) (threads.LLMStreamer, error)
}

// Prefix returns a Match that claims ids starting with any prefix. Matching is
// case-insensitive; the returned Model carries only the id.
func Prefix(prefixes ...string) func(string) (Model, bool) {
	return func(id string) (Model, bool) {
		lower := strings.ToLower(id)
		for _, p := range prefixes {
			if strings.HasPrefix(lower, strings.ToLower(p)) {
				return Model{ID: id}, true
			}
		}
		return Model{}, false
	}
}

// Options configures Provider.Open.
type Options struct {
	APIKey     string
	BaseURL    string
	HTTPClient *http.Client
	// Token authenticates through a subscription instead of an API key.
	Token *Token
	// OnRefresh is called when the provider refreshes Token.
	OnRefresh func(*Token) error
}

// Token is a subscription credential.
type Token struct {
	Access  string
	Refresh string
	Expiry  time.Time
}

// Subscription is a provider's sign-in flow.
type Subscription interface {
	Name() string
	SignIn(ctx context.Context, show func(SignInPrompt)) (*Token, error)
}

// SignInPrompt is what to show the user during a device sign-in.
type SignInPrompt struct {
	URL      string
	Code     string
	Interval time.Duration
}

// EffortSetter is implemented by streamers with an effort knob. SetEffort maps
// the level onto the provider's knob; validation against Model.Efforts is the
// caller's job.
type EffortSetter interface {
	SetEffort(Effort) error
}

// FastSetter is implemented by streamers with a priority tier.
type FastSetter interface {
	SetFast(bool) error
}

var (
	ErrNoCredential = errors.New("llms: no credential")
	ErrUnknownModel = errors.New("llms: unknown model")
)

// EnvAPIKey returns the first non-empty environment value among env.
func EnvAPIKey(env []string) string {
	for _, key := range env {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	return ""
}
