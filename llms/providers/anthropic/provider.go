package anthropic

import (
	"context"

	anthropicapi "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/mackross/agentloom/llms"
	"github.com/mackross/agentloom/threads"
)

// ID is the provider id.
const ID = "anthropic"

var reasoningEfforts = []llms.Effort{llms.EffortNone, llms.EffortLow, llms.EffortMedium, llms.EffortHigh, llms.EffortMax}

// Provider describes Anthropic's Messages API.
var Provider = &llms.Provider{
	ID:  ID,
	Env: []string{"ANTHROPIC_API_KEY"},
	Models: []llms.Model{
		{ID: string(LatestLargeModel), Aliases: []string{"sonnet", "claude"}, Label: "balanced", Efforts: reasoningEfforts},
		{ID: string(LatestStrongModel), Aliases: []string{"opus"}, Label: "flagship", Efforts: reasoningEfforts},
		{ID: string(LatestFastModel), Aliases: []string{"haiku"}, Label: "fast"},
	},
	Match: func(id string) (llms.Model, bool) {
		m, ok := llms.Prefix("claude")(id)
		if ok && !supportsAssistantPrefix(id) {
			// Claude 4.6 and later support adaptive thinking and output effort.
			m.Efforts = reasoningEfforts
		}
		return m, ok
	},
	Open: func(_ context.Context, m llms.Model, o llms.Options) (threads.LLMStreamer, error) {
		if o.APIKey == "" {
			o.APIKey = llms.EnvAPIKey([]string{"ANTHROPIC_API_KEY"})
		}
		if o.APIKey == "" {
			return nil, llms.ErrNoCredential
		}
		opts := []option.RequestOption{option.WithAPIKey(o.APIKey)}
		if o.BaseURL != "" {
			opts = append(opts, option.WithBaseURL(o.BaseURL))
		}
		if o.HTTPClient != nil {
			opts = append(opts, option.WithHTTPClient(o.HTTPClient))
		}
		return NewMessagesStreamerWithClient(anthropicapi.NewClient(opts...), m.ID), nil
	},
}
