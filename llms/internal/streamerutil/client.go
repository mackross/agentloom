package streamerutil

import (
	openaiapi "github.com/mackross/openai-go/v3"
	"github.com/mackross/openai-go/v3/option"

	"github.com/mackross/agentloom/llms"
)

// OpenAIClient builds an openai-go client from llms.Options. defaultBaseURL is
// used when o.BaseURL is empty; an empty result leaves the SDK default.
func OpenAIClient(o llms.Options, defaultBaseURL string, extra ...option.RequestOption) openaiapi.Client {
	var opts []option.RequestOption
	base := o.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	if base != "" {
		opts = append(opts, option.WithBaseURL(base))
	}
	if o.APIKey != "" {
		opts = append(opts, option.WithAPIKey(o.APIKey))
	}
	if o.HTTPClient != nil {
		opts = append(opts, option.WithHTTPClient(o.HTTPClient))
	}
	return openaiapi.NewClient(append(opts, extra...)...)
}
