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

// Provider describes Cerebras. Reasoning models stream hidden reasoning.
var Provider = &llms.Provider{
	ID:  ID,
	Env: []string{"CEREBRAS_API_KEY"},
	Models: []llms.Model{
		{ID: Gemma4_31BModel, Aliases: []string{"gemma", "gemma-4"}, Label: "Gemma 4 31B", Fast: true},
		{ID: GPTOSS120BModel, Label: "GPT-OSS 120B", Efforts: reasoningEfforts, Fast: true},
		{ID: Qwen3235BModel, Label: "Qwen 3 235B", Fast: true},
		{ID: Llama31_8BModel, Label: "Llama 3.1 8B", Fast: true},
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
