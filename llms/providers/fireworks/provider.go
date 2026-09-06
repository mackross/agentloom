package fireworks

import (
	"context"

	"github.com/mackross/agentloom/llms"
	"github.com/mackross/agentloom/llms/internal/streamerutil"
	"github.com/mackross/agentloom/threads"
)

// ID is the provider id.
const ID = "fireworks"

// Provider describes Fireworks AI.
var Provider = &llms.Provider{
	ID:  ID,
	Env: []string{"FIREWORKS_API_KEY", "FIREWORKS_AI_API_KEY"},
	Models: []llms.Model{
		{ID: Kimi3Model, Aliases: []string{"kimi", "kimi-k3"}, Label: "Kimi K3"},
		{ID: DeepSeekV4ProModel, Label: "DeepSeek V4 Pro"},
		{ID: DeepSeekV4FlashModel, Label: "DeepSeek V4 Flash"},
		{ID: MiniMaxM27Model, Aliases: []string{"minimax"}, Label: "MiniMax M2.7"},
		{ID: GPTOSS120BModel, Label: "GPT-OSS 120B"},
	},
	Match: llms.Prefix("accounts/fireworks/models/"),
	Open: func(_ context.Context, m llms.Model, o llms.Options) (threads.LLMStreamer, error) {
		if o.APIKey == "" {
			o.APIKey = llms.EnvAPIKey([]string{"FIREWORKS_API_KEY", "FIREWORKS_AI_API_KEY"})
		}
		if o.APIKey == "" {
			return nil, llms.ErrNoCredential
		}
		return NewChatCompletionsStreamerWithClient(streamerutil.OpenAIClient(o, BaseURL), m.ID), nil
	},
}
