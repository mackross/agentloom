package cerebras

import (
	"context"
	"strings"

	"github.com/mackross/agentloom/llms"
	"github.com/mackross/agentloom/llms/internal/streamerutil"
	"github.com/mackross/agentloom/threads"
)

// ID is the provider id.
const ID = "cerebras"

var reasoningEfforts = []llms.Effort{llms.EffortLow, llms.EffortMedium, llms.EffortHigh}
var qwenEfforts = []llms.Effort{llms.EffortNone, llms.EffortLow, llms.EffortMedium, llms.EffortHigh}

// Provider lists Cerebras Shared Inference models. Service tiers are dedicated-
// only and are intentionally not advertised for these models.
// https://inference-docs.cerebras.ai/models/overview
var Provider = &llms.Provider{
	ID:  ID,
	Env: []string{"CEREBRAS_API_KEY"},
	Models: []llms.Model{
		{ID: GPTOSS120BModel, Label: "GPT-OSS 120B", Efforts: reasoningEfforts},
		{ID: Qwen38_27BModel, Aliases: []string{"cerebras-qwen"}, Label: "Qwen 3.8 27B", Efforts: qwenEfforts},
	},
	Open: func(_ context.Context, m llms.Model, o llms.Options) (threads.LLMStreamer, error) {
		if o.APIKey == "" {
			o.APIKey = llms.EnvAPIKey([]string{"CEREBRAS_API_KEY"})
		}
		if o.APIKey == "" {
			return nil, llms.ErrNoCredential
		}
		s := NewChatCompletionsStreamerWithClient(streamerutil.OpenAIClient(o, BaseURL), m.ID)
		if reasoningModel(m.ID) {
			s.ReasoningFormat = "hidden"
		}
		return s, nil
	},
}

func reasoningModel(id string) bool {
	lower := strings.ToLower(id)
	return strings.HasPrefix(lower, "gpt-oss-") || strings.HasPrefix(lower, "zai-glm-")
}
