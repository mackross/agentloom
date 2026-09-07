package xai

import (
	"context"
	"net/http"
	"strings"

	"github.com/mackross/openai-go/v3/option"
	"golang.org/x/oauth2"

	"github.com/mackross/agentloom/llms"
	"github.com/mackross/agentloom/llms/internal/streamerutil"
	"github.com/mackross/agentloom/llms/subscription"
	"github.com/mackross/agentloom/threads"
)

// ID is the provider id.
const ID = "xai"

var reasoningEfforts = []llms.Effort{llms.EffortLow, llms.EffortMedium, llms.EffortHigh}

// Provider describes xAI: API keys from XAI_API_KEY or a Grok subscription
// sign-in.
var Provider = &llms.Provider{
	ID:           ID,
	Env:          []string{"XAI_API_KEY"},
	Subscription: subscription.DeviceFlow("xAI", oauthConfig()),
	Models: []llms.Model{
		{ID: DefaultModel, Aliases: []string{"grok", "grok-4.6-latest"}, Label: "flagship", Efforts: reasoningEfforts, Fast: true},
		{ID: "grok-code-fast-1", Aliases: []string{"code-fast", "grok-build-0.1"}, Label: "fast coding", Fast: true},
	},
	Match: match,
	Open:  open,
}

func match(id string) (llms.Model, bool) {
	lower := strings.ToLower(id)
	if !strings.HasPrefix(lower, "grok") {
		return llms.Model{}, false
	}
	m := llms.Model{ID: id, Fast: true}
	for _, p := range []string{"grok-4.5", "grok-4.6", "grok-4.20-multi-agent"} {
		if strings.HasPrefix(lower, p) {
			m.Efforts = reasoningEfforts
		}
	}
	return m, true
}

func oauthConfig() *oauth2.Config {
	return &oauth2.Config{
		ClientID: "b1a00492-073a-47ea-816f-4c329264a828",
		Scopes:   []string{"openid", "profile", "email", "offline_access", "grok-cli:access", "api:access"},
		Endpoint: oauth2.Endpoint{
			TokenURL:      "https://auth.x.ai/oauth2/token",
			DeviceAuthURL: "https://auth.x.ai/oauth2/device/code",
			AuthStyle:     oauth2.AuthStyleInParams,
		},
	}
}

func open(ctx context.Context, m llms.Model, o llms.Options) (threads.LLMStreamer, error) {
	if o.Token != nil {
		src := subscription.TokenSource(ctx, oauthConfig(), o.Token, o.OnRefresh, o.HTTPClient)
		mw := func(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
			if err := subscription.Authorize(src, req, nil); err != nil {
				return nil, err
			}
			return next(req)
		}
		o.APIKey = "oauth"
		return NewResponsesStreamerWithClient(streamerutil.OpenAIClient(o, baseURLFromEnv(), option.WithMiddleware(mw)), m.ID), nil
	}
	if o.APIKey == "" {
		o.APIKey = llms.EnvAPIKey([]string{"XAI_API_KEY"})
	}
	if o.APIKey == "" {
		return nil, llms.ErrNoCredential
	}
	return NewResponsesStreamerWithClient(streamerutil.OpenAIClient(o, baseURLFromEnv()), m.ID), nil
}
