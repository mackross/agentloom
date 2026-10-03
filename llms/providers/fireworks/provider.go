package fireworks

import (
	"context"
	"fmt"
	"strings"

	"github.com/mackross/agentloom/llms"
	"github.com/mackross/agentloom/llms/internal/streamerutil"
	"github.com/mackross/agentloom/threads"
)

// ID is the provider id.
const ID = "fireworks"

// Serverless text models from https://docs.fireworks.ai/serverless/pricing.
// Dedicated-only and regional deployments are not part of this catalog.
var serverlessModels = []llms.Model{
	{ID: Kimi3Model, Aliases: []string{"fireworks-kimi", "fireworks-kimi-k3"}, Label: "Kimi K3", ServiceTiers: priorityTiers(1.25)},
	{ID: Ember1Model, Aliases: []string{"fireworks-ember"}, Label: "Ember 1", ServiceTiers: priorityTiers(1.25)},
	{ID: DeepSeekV41FlashModel, Label: "DeepSeek V4.1 Flash", ServiceTiers: priorityTiers(1.25)},
	{ID: GLM53Model, Aliases: []string{"fireworks-glm"}, Label: "GLM 5.3", ServiceTiers: priorityTiers(1.25)},
	{ID: GLM53FlashModel, Label: "GLM 5.3 Flash", ServiceTiers: priorityTiers(1.25)},
	{ID: Qwen38MaxModel, Aliases: []string{"fireworks-qwen", "fireworks-qwen-max"}, Label: "Qwen 3.8 Max", ServiceTiers: priorityTiers(1.5)},
	{ID: MiniMaxM3Model, Aliases: []string{"fireworks-minimax"}, Label: "MiniMax M3", ServiceTiers: priorityTiers(1.5)},
	{ID: GPTOSS120BModel, Label: "GPT-OSS 120B", ServiceTiers: priorityTiers(1.2)},
	{ID: Nemotron35LightningModel, Label: "Nemotron 3.5 Lightning 30B A3B", ServiceTiers: priorityTiers(1.25)},
	{ID: Nemotron3UltraModel, Label: "Nemotron 3 Ultra (preview)", ServiceTiers: priorityTiers(1.25)},
	// Fast is a router model ID, not a service_tier value. Its base price is
	// already 1.5x the corresponding ordinary model; no Priority is offered.
	{ID: Kimi3FastModel, Aliases: []string{"fireworks-kimi-fast"}, Label: "Kimi K3 Fast (1.5x base price)", ServiceTiers: priorityTiers(0)},
	{ID: GLM53FastModel, Aliases: []string{"fireworks-glm-fast"}, Label: "GLM 5.3 Fast (1.5x base price)", ServiceTiers: priorityTiers(0)},
}

// Provider describes Fireworks Serverless. Dedicated deployments are not supported.
var Provider = &llms.Provider{
	ID:     ID,
	Env:    []string{"FIREWORKS_API_KEY", "FIREWORKS_AI_API_KEY"},
	Models: serverlessModels,
	Match:  match,
	Open: func(_ context.Context, m llms.Model, o llms.Options) (threads.LLMStreamer, error) {
		if dedicatedOnly(m.ID) || strings.Contains(strings.ToLower(m.ID), "/deployments/") {
			return nil, fmt.Errorf("fireworks: %q requires a dedicated deployment; only serverless models are supported", m.ID)
		}
		if o.APIKey == "" {
			o.APIKey = llms.EnvAPIKey([]string{"FIREWORKS_API_KEY", "FIREWORKS_AI_API_KEY"})
		}
		if o.APIKey == "" {
			return nil, llms.ErrNoCredential
		}
		return NewChatCompletionsStreamerWithClient(streamerutil.OpenAIClient(o, BaseURL), m.ID), nil
	},
}

func dedicatedOnly(id string) bool {
	switch strings.ToLower(id) {
	case DeepSeekV4ProModel, DeepSeekV4FlashModel, MiniMaxM27Model:
		return true
	}
	return false
}

func match(id string) (llms.Model, bool) {
	lower := strings.ToLower(id)
	if dedicatedOnly(lower) {
		return llms.Model{}, false
	}
	for _, model := range serverlessModels {
		if lower == model.ID {
			model.ID = id
			return model, true
		}
	}
	// Permit future serverless model/router IDs without guessing prices.
	return llms.Prefix("accounts/fireworks/models/", "accounts/fireworks/routers/")(id)
}
