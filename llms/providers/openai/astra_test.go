package openai

import (
	"encoding/json"
	"testing"

	"github.com/mackross/openai-go/v3/responses"
	"github.com/mackross/openai-go/v3/shared"

	"github.com/mackross/agentloom/llms"
	"github.com/mackross/agentloom/threads"
)

func TestAstraModelCatalog(t *testing.T) {
	m, err := llms.NewCatalog(Provider).Resolve("astra")
	if err != nil {
		t.Fatalf("resolve astra: %v", err)
	}
	if m.ID != ModelAstra || m.Provider != ID {
		t.Fatalf("astra model: %+v", m)
	}
	for _, e := range []llms.Effort{llms.EffortLow, llms.EffortMax, llms.EffortDefault} {
		if !m.AcceptsEffort(e) {
			t.Fatalf("astra rejects %s", e)
		}
	}
	for _, e := range []llms.Effort{llms.EffortNone, llms.EffortMinimal} {
		if m.AcceptsEffort(e) {
			t.Fatalf("astra accepted removed effort %s", e)
		}
	}

	for _, id := range []string{"gpt-6-nova", ModelAstra} {
		m, ok := Provider.Match(id)
		if !ok || len(m.Efforts) == 0 || m.AcceptsEffort(llms.EffortNone) || !m.AcceptsEffort(llms.EffortMax) {
			t.Fatalf("Match(%q): %+v %v", id, m, ok)
		}
	}
}

func TestAstraSendsMaxEffort(t *testing.T) {
	s := NewResponsesStreamer(ModelAstra)
	if err := s.SetEffort(llms.EffortMax); err != nil {
		t.Fatalf("SetEffort(max): %v", err)
	}
	params, err := s.responseRequest(threads.Req{Items: []threads.Item{threads.UserText("hi")}})
	if err != nil {
		t.Fatalf("responseRequest: %v", err)
	}
	if params.Reasoning.Effort != shared.ReasoningEffort("max") {
		t.Fatalf("effort = %q, want max", params.Reasoning.Effort)
	}
}

func TestConfigurationUpdateItems(t *testing.T) {
	items, err := conversationInput(ModelAstra, shared.ReasoningEffortLow, threads.Req{
		Items: []threads.Item{
			threads.UserText("first"),
			threads.UserText("second"),
			threads.UserText("third"),
			threads.UserText("fourth"),
		},
		ItemMeta: []map[string]any{
			nil,
			ReasoningEffortUpdate(llms.EffortHigh).Metadata,
			ReasoningEffortUpdate(llms.EffortHigh).Metadata,
			ReasoningEffortUpdate(llms.EffortLow).Metadata,
		},
	}, reasoningProvider)
	if err != nil {
		t.Fatalf("conversationInput: %v", err)
	}

	var updates []string
	for _, item := range items {
		raw, err := json.Marshal(item)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var probe struct {
			Type      string `json:"type"`
			Reasoning struct {
				Effort string `json:"effort"`
			} `json:"reasoning"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if probe.Type == "configuration_update" {
			updates = append(updates, probe.Reasoning.Effort)
		}
	}
	if len(updates) != 1 || updates[0] != "high" {
		t.Fatalf("updates = %#v, want [high] (one per change, none repeating request effort low)", updates)
	}
	if len(items) != 5 {
		t.Fatalf("items = %d, want 4 messages + 1 update", len(items))
	}
}

func TestConfigurationUpdateIgnoredOffAstra(t *testing.T) {
	items, err := conversationInput("gpt-5.6-sol", shared.ReasoningEffortLow, threads.Req{
		Items:    []threads.Item{threads.UserText("a")},
		ItemMeta: []map[string]any{ReasoningEffortUpdate(llms.EffortHigh).Metadata},
	}, reasoningProvider)
	if err != nil {
		t.Fatalf("conversationInput: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %#v, want only the message", items)
	}
}

func TestIncompleteReasons(t *testing.T) {
	steered := responses.ResponseStreamEventUnion{Response: responses.Response{
		IncompleteDetails: responses.ResponseIncompleteDetails{Reason: "steered"},
	}}
	if got := incompleteReason(steered); got != "steered" {
		t.Fatalf("steered reason = %q", got)
	}
	truncated := responses.ResponseStreamEventUnion{Response: responses.Response{
		Status: responses.ResponseStatusIncomplete,
	}}
	if got := incompleteReason(truncated); got != "incomplete" {
		t.Fatalf("truncated reason = %q", got)
	}
	failed := responses.ResponseStreamEventUnion{Response: responses.Response{
		Error: responses.ResponseError{Code: "misalignment", Message: "task stopped"},
	}}
	if got := failedReason(failed); got != "misalignment: task stopped" {
		t.Fatalf("failed reason = %q", got)
	}
}
