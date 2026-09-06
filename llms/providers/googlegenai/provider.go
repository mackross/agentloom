package googlegenai

import (
	"context"
	"strings"

	"google.golang.org/genai"

	"github.com/mackross/agentloom/llms"
	"github.com/mackross/agentloom/threads"
)

// ID is the provider id.
const ID = "google"

var reasoningEfforts = []llms.Effort{llms.EffortLow, llms.EffortMedium, llms.EffortHigh}

// Provider describes Google Gemini through the Gemini API.
var Provider = &llms.Provider{
	ID:  ID,
	Env: []string{"GOOGLE_API_KEY", "GEMINI_API_KEY"},
	Models: []llms.Model{
		{ID: DefaultModel, Aliases: []string{"gemini", "gemini-3.8"}, Label: "Gemini 3.8 Flash", Efforts: reasoningEfforts},
	},
	Match: func(id string) (llms.Model, bool) {
		lower := strings.TrimPrefix(strings.ToLower(id), "models/")
		if !strings.HasPrefix(lower, "gemini-") {
			return llms.Model{}, false
		}
		m := llms.Model{ID: strings.TrimPrefix(id, "models/")}
		if strings.HasPrefix(lower, "gemini-3.8") {
			m.Efforts = reasoningEfforts
		}
		return m, true
	},
	Open: func(ctx context.Context, m llms.Model, o llms.Options) (threads.LLMStreamer, error) {
		if o.APIKey == "" {
			o.APIKey = llms.EnvAPIKey([]string{"GOOGLE_API_KEY", "GEMINI_API_KEY"})
		}
		if o.APIKey == "" {
			return nil, llms.ErrNoCredential
		}
		cfg := &genai.ClientConfig{APIKey: o.APIKey, Backend: genai.BackendGeminiAPI, HTTPClient: o.HTTPClient}
		if o.BaseURL != "" {
			cfg.HTTPOptions.BaseURL = o.BaseURL
		}
		client, err := genai.NewClient(ctx, cfg)
		if err != nil {
			return nil, err
		}
		return NewGenerateContentStreamerWithClient(client, m.ID), nil
	},
}
