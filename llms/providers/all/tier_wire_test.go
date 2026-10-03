package all_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mackross/agentloom/llms"
	"github.com/mackross/agentloom/llms/providers/fireworks"
	"github.com/mackross/agentloom/llms/providers/googlegenai"
	"github.com/mackross/agentloom/threads"
)

func TestNewProviderTierWireParameters(t *testing.T) {
	for _, tc := range []struct {
		name         string
		p            *llms.Provider
		model        string
		key          string
		standard     llms.ServiceTierID
		standardWire string
		response     string
	}{
		{"fireworks", fireworks.Provider, fireworks.Kimi3Model, "service_tier", fireworks.ServiceTierStandard, "",
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"},
		{"google", googlegenai.Provider, "gemini-3.5-flash-lite", "serviceTier", googlegenai.ServiceTierStandard, "standard",
			"data: {\"candidates\":[{\"index\":0,\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}]}\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := make(chan map[string]any, 4)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				requests <- body
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, tc.response)
			}))
			defer server.Close()
			c := llms.NewCatalog(tc.p)
			s, _, err := c.Open(context.Background(), tc.model, llms.Options{
				APIKey: "test", BaseURL: server.URL, HTTPClient: server.Client(),
			})
			if err != nil {
				t.Fatal(err)
			}
			if tc.name == "google" {
				if err := s.(llms.EffortSetter).SetEffort(llms.EffortMinimal); err != nil {
					t.Fatal(err)
				}
			}
			setter := s.(llms.ServiceTierSetter)
			for _, choice := range []struct {
				id   llms.ServiceTierID
				wire string
			}{
				{"priority", "priority"}, {tc.standard, tc.standardWire}, {"", ""},
			} {
				if err := setter.SetServiceTier(choice.id); err != nil {
					t.Fatal(err)
				}
				if err := s.StreamReq(threads.Req{Items: []threads.Item{threads.UserText("hi")}}, func(threads.Item) error { return nil }); err != nil {
					t.Fatal(err)
				}
				body := <-requests
				got, _ := body[tc.key].(string)
				if got != choice.wire {
					t.Fatalf("%s = %q, want %q; body %+v", tc.key, got, choice.wire, body)
				}
				if tc.name == "google" {
					config, _ := body["generationConfig"].(map[string]any)
					thinking, _ := config["thinkingConfig"].(map[string]any)
					if thinking["thinkingLevel"] != "MINIMAL" {
						t.Fatalf("minimal effort not serialized: %+v", body)
					}
				}
			}
		})
	}
}
