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
var minimalReasoningEfforts = []llms.Effort{llms.EffortMinimal, llms.EffortLow, llms.EffortMedium, llms.EffortHigh}

// Text-output models supported by the generateContent adapter. Live, TTS,
// image and video generation require separate adapters.
// https://ai.google.dev/gemini-api/docs/models
var textModels = []llms.Model{
	{ID: DefaultModel, Aliases: []string{"gemini", "gemini-3.8"}, Label: "Gemini 3.8 Flash", Efforts: reasoningEfforts, ServiceTiers: serviceTiers},
	{ID: "gemini-3.1-pro-preview", Aliases: []string{"gemini-pro"}, Label: "Gemini 3.1 Pro (preview)", Efforts: reasoningEfforts, ServiceTiers: serviceTiers},
	{ID: "gemini-3.5-flash-lite", Aliases: []string{"gemini-lite"}, Label: "Gemini 3.5 Flash-Lite", Efforts: minimalReasoningEfforts, ServiceTiers: serviceTiers},
	{ID: "gemini-3.7-flash", Label: "Gemini 3.7 Flash", Efforts: reasoningEfforts, ServiceTiers: serviceTiers},
	{ID: "gemini-3.6-flash", Label: "Gemini 3.6 Flash", Efforts: minimalReasoningEfforts, ServiceTiers: serviceTiers},
	{ID: "gemini-3.5-flash", Label: "Gemini 3.5 Flash", Efforts: minimalReasoningEfforts, ServiceTiers: serviceTiers},
	{ID: "gemini-3.1-flash-lite", Label: "Gemini 3.1 Flash-Lite", Efforts: minimalReasoningEfforts, ServiceTiers: serviceTiers},
	{ID: "gemini-3-flash-preview", Label: "Gemini 3 Flash (preview)", Efforts: minimalReasoningEfforts, ServiceTiers: serviceTiers},
	{ID: "gemini-3.1-pro-preview-customtools", Label: "Gemini 3.1 Pro custom tools (preview)", Efforts: reasoningEfforts, ServiceTiers: serviceTiers},
}

// Provider describes Google Gemini through the Gemini API.
var Provider = &llms.Provider{
	ID:     ID,
	Env:    []string{"GOOGLE_API_KEY", "GEMINI_API_KEY"},
	Models: textModels,
	Match: func(id string) (llms.Model, bool) {
		lower := strings.TrimPrefix(strings.ToLower(id), "models/")
		if !strings.HasPrefix(lower, "gemini-") {
			return llms.Model{}, false
		}
		m := llms.Model{ID: strings.TrimPrefix(id, "models/")}
		for _, known := range textModels {
			if lower == known.ID {
				m.Efforts = known.Efforts
				m.ServiceTiers = known.ServiceTiers
				break
			}
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
