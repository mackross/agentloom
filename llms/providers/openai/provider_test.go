package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mackross/agentloom/llms"
	"github.com/mackross/agentloom/threads"
)

func TestGPT6Models(t *testing.T) {
	catalog := llms.NewCatalog(Provider)
	for _, tc := range []struct {
		id   string
		none bool
	}{
		{"gpt-6-sol", true},
		{"gpt-6-luna", true},
		{"gpt-6.1-sol", false},
	} {
		t.Run(tc.id, func(t *testing.T) {
			listed := false
			for _, m := range Provider.Models {
				listed = listed || m.ID == tc.id
			}
			if !listed {
				t.Fatal("model missing from catalog")
			}
			m, err := catalog.Resolve(tc.id)
			if err != nil {
				t.Fatal(err)
			}
			if m.ID != tc.id || m.Provider != ID || !m.Fast {
				t.Fatalf("resolved model: %+v", m)
			}
			// The synthetic suffix tests fallback routing, not the existence of
			// a published snapshot. The docs currently list only bare model IDs.
			for _, id := range []string{tc.id, strings.ToUpper(tc.id), tc.id + "-test-snapshot"} {
				matched, ok := match(id)
				if !ok || matched.ID != id || !matched.Fast {
					t.Fatalf("match(%q) = %+v, %v", id, matched, ok)
				}
				for _, model := range []llms.Model{m, matched} {
					for _, effort := range []llms.Effort{llms.EffortDefault, llms.EffortLow, llms.EffortMedium, llms.EffortHigh, llms.EffortXHigh, llms.EffortMax} {
						if !model.AcceptsEffort(effort) {
							t.Errorf("%s rejects %s", id, effort)
						}
					}
					if model.AcceptsEffort(llms.EffortNone) != tc.none || model.AcceptsEffort(llms.EffortMinimal) {
						t.Errorf("%s has incorrect efforts: %v", id, model.Efforts)
					}
				}
			}
			efforts := []llms.Effort{llms.EffortMax}
			if tc.none {
				efforts = append(efforts, llms.EffortNone)
			}
			for _, effort := range efforts {
				s := NewResponsesStreamer(tc.id)
				if err := s.SetEffort(effort); err != nil {
					t.Fatal(err)
				}
				params, err := s.responseRequest(threads.Req{Items: []threads.Item{threads.UserText("hi")}})
				if err != nil {
					t.Fatal(err)
				}
				raw, err := json.Marshal(params)
				if err != nil {
					t.Fatal(err)
				}
				var wire struct {
					Model     string `json:"model"`
					Reasoning struct {
						Effort string `json:"effort"`
					} `json:"reasoning"`
				}
				if err := json.Unmarshal(raw, &wire); err != nil {
					t.Fatal(err)
				}
				if wire.Model != tc.id || wire.Reasoning.Effort != string(effort) {
					t.Fatalf("unexpected wire request: %s", raw)
				}
			}
		})
	}
}

func TestOpenAIExistingSelectionsUnchanged(t *testing.T) {
	if Provider.Models[0].ID != "gpt-5.6-sol" {
		t.Fatal("provider default changed")
	}
	catalog := llms.NewCatalog(Provider)
	for alias, want := range map[string]string{
		"sol": "gpt-5.6-sol", "terra": "gpt-5.6-terra",
		"luna": "gpt-5.6-luna", "astra": ModelAstra, "gpt-6": ModelAstra,
	} {
		m, err := catalog.Resolve(alias)
		if err != nil || m.ID != want {
			t.Errorf("Resolve(%q) = %+v, %v; want %s", alias, m, err, want)
		}
	}
}
