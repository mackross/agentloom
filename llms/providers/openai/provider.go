package openai

import (
	"context"
	"strings"

	"github.com/mackross/agentloom/llms"
	"github.com/mackross/agentloom/llms/internal/streamerutil"
	"github.com/mackross/agentloom/threads"
)

// ID is the provider id.
const ID = "openai"

// ModelAstra is the GPT-6 Astra wire id. It differs from the GPT-5.6 family in
// ways the streamer cannot express through typed SDK params today; see
// astraEfforts and effort.go.
const ModelAstra = "gpt-6-astra"

var (
	reasoningEfforts = []llms.Effort{llms.EffortNone, llms.EffortMinimal, llms.EffortLow, llms.EffortMedium, llms.EffortHigh, llms.EffortXHigh}
	// Astra and GPT-6.1 Sol require reasoning; sending `none` is a request error.
	astraEfforts = []llms.Effort{llms.EffortLow, llms.EffortMedium, llms.EffortHigh, llms.EffortXHigh, llms.EffortMax}
	// GPT-6 Sol and Luna additionally allow reasoning to be disabled.
	gpt6Efforts = []llms.Effort{llms.EffortNone, llms.EffortLow, llms.EffortMedium, llms.EffortHigh, llms.EffortXHigh, llms.EffortMax}
)

// Provider describes OpenAI: API keys from OPENAI_API_KEY or a Codex
// subscription sign-in.
var Provider = &llms.Provider{
	ID:           ID,
	Env:          []string{"OPENAI_API_KEY"},
	Subscription: Codex{},
	Models: withServiceTiers([]llms.Model{
		{ID: "gpt-5.6-sol", Aliases: []string{"sol", "gpt-5.6"}, Label: "flagship coding", Efforts: reasoningEfforts, Fast: true},
		// Astra is listed after sol so the provider default is unchanged: it is
		// gated per project and has no `none`/`minimal` effort to fall back to.
		// Fast mode works outside EU data residency, where service_tier
		// fast/priority is rejected.
		{ID: ModelAstra, Aliases: []string{"astra", "gpt-6"}, Label: "flagship agentic", Efforts: astraEfforts, Fast: true},
		{ID: "gpt-6.1-sol", Label: "balanced agentic", Efforts: astraEfforts, Fast: true},
		{ID: "gpt-6-sol", Label: "coding and agentic", Efforts: gpt6Efforts, Fast: true},
		{ID: "gpt-6-luna", Label: "cost-efficient agentic", Efforts: gpt6Efforts, Fast: true},
		{ID: "gpt-5.6-terra", Aliases: []string{"terra"}, Label: "balanced", Efforts: reasoningEfforts, Fast: true},
		{ID: "gpt-5.6-luna", Aliases: []string{"luna"}, Label: "cost-efficient", Efforts: reasoningEfforts, Fast: true},
		{ID: DefaultModel, Label: "small", Fast: true},
	}),
	Match: match,
	Open:  open,
}

func match(id string) (llms.Model, bool) {
	lower := strings.ToLower(id)
	for _, p := range []string{"gpt-", "o1", "o3", "o4", "codex-", "chatgpt-"} {
		if strings.HasPrefix(lower, p) {
			m := llms.Model{ID: id, Fast: true, FastServiceTier: ServiceTierFast, ServiceTiers: serviceTiers(id)}
			switch {
			case lower == "gpt-6-sol", strings.HasPrefix(lower, "gpt-6-sol-"),
				lower == "gpt-6-luna", strings.HasPrefix(lower, "gpt-6-luna-"):
				m.Efforts = gpt6Efforts
			case strings.HasPrefix(lower, "gpt-6"), lower == ModelAstra:
				m.Efforts = astraEfforts
			default:
				if strings.HasPrefix(lower, "gpt-5") || strings.HasPrefix(lower, "o") {
					m.Efforts = reasoningEfforts
				}
			}
			return m, true
		}
	}
	return llms.Model{}, false
}

func open(ctx context.Context, m llms.Model, o llms.Options) (threads.LLMStreamer, error) {
	if o.Token != nil {
		return openCodex(ctx, m, o)
	}
	if o.APIKey == "" {
		o.APIKey = llms.EnvAPIKey([]string{"OPENAI_API_KEY"})
	}
	if o.APIKey == "" {
		return nil, llms.ErrNoCredential
	}
	return NewResponsesStreamerWithClient(streamerutil.OpenAIClient(o, ""), m.ID), nil
}
