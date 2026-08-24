package responsesutil

import (
	"encoding/json"
	"fmt"

	"github.com/openai/openai-go/v3/responses"

	"github.com/mackross/agentloom/threads"
)

func ReasoningOutput(provider string, item responses.ResponseOutputItemUnion) threads.ReasoningItem {
	reasoning := item.AsReasoning()
	out := threads.ReasoningItem{Provider: provider, ID: item.ID, Opaque: []byte(item.RawJSON())}
	for _, content := range reasoning.Content {
		out.Text += content.Text
	}
	for _, content := range reasoning.Summary {
		out.Summary += content.Text
	}
	if out.Text != "" {
		out.Visibility = threads.ReasoningVisibilityText
	} else if out.Summary != "" {
		out.Visibility = threads.ReasoningVisibilitySummary
	}
	return out
}

func ReasoningInput(provider string, item threads.ReasoningItem) (responses.ResponseInputItemUnionParam, error) {
	if len(item.Opaque) == 0 {
		return responses.ResponseInputItemUnionParam{}, fmt.Errorf("%s reasoning item %q has no opaque data", provider, item.ID)
	}
	var raw responses.ResponseReasoningItem
	if err := json.Unmarshal(item.Opaque, &raw); err != nil {
		return responses.ResponseInputItemUnionParam{}, fmt.Errorf("%s reasoning item %q has invalid opaque data: %w", provider, item.ID, err)
	}
	if raw.Type != "reasoning" {
		return responses.ResponseInputItemUnionParam{}, fmt.Errorf("%s reasoning item %q has invalid opaque type %q", provider, item.ID, raw.Type)
	}
	reasoning := raw.ToParam()
	return responses.ResponseInputItemUnionParam{OfReasoning: &reasoning}, nil
}
