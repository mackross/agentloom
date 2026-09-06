package deepseek

import (
	"context"

	"github.com/mackross/agentloom/llms"
	"github.com/mackross/agentloom/llms/internal/streamerutil"
	"github.com/mackross/agentloom/threads"
)

// ID is the provider id.
const ID = "deepseek"

// Provider describes DeepSeek's Responses-compatible API.
var Provider = &llms.Provider{
	ID:  ID,
	Env: []string{"DEEPSEEK_API_KEY"},
	Models: []llms.Model{
		{ID: DefaultModel, Aliases: []string{"deepseek"}, Label: "fast"},
		{ID: "deepseek-v4-pro", Label: "flagship"},
	},
	Match: llms.Prefix("deepseek-"),
	Open: func(_ context.Context, m llms.Model, o llms.Options) (threads.LLMStreamer, error) {
		if o.APIKey == "" {
			o.APIKey = llms.EnvAPIKey([]string{"DEEPSEEK_API_KEY"})
		}
		if o.APIKey == "" {
			return nil, llms.ErrNoCredential
		}
		return NewResponsesStreamerWithClient(streamerutil.OpenAIClient(o, BaseURL), m.ID), nil
	},
}
