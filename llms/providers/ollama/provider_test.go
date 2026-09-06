package ollama

import (
	"context"
	"strings"
	"testing"

	"github.com/mackross/agentloom/llms"
)

func TestOpenReadsModelOptions(t *testing.T) {
	m := llms.Model{Name: "qwen", ID: "qwen3:8b", Options: map[string]any{
		"host":                            "terminus.local:11434",
		"headers":                         map[string]any{"authorization": "Bearer x"},
		"options":                         map[string]any{"num_ctx": int64(65536)},
		"think":                           "high",
		"keep_alive":                      "30m",
		"truncate":                        false,
		"allow_best_effort_tool_controls": true,
	}}
	s, err := Provider.Open(context.Background(), m, llms.Options{})
	if err != nil {
		t.Fatal(err)
	}
	cs := s.(*ChatStreamer)
	if cs.baseURL.String() != "http://terminus.local:11434" {
		t.Fatalf("baseURL = %s", cs.baseURL)
	}
	if cs.Headers.Get("Authorization") != "Bearer x" || cs.Think != "high" || cs.KeepAlive != "30m" || cs.Truncate == nil || *cs.Truncate || !cs.AllowBestEffortToolControls {
		t.Fatalf("fields: %+v", cs)
	}
	if cs.Options["num_ctx"].(interface{ String() string }).String() != "65536" {
		t.Fatalf("options = %v", cs.Options)
	}
	if err := cs.SetEffort(llms.EffortNone); err != nil || cs.Think != false {
		t.Fatalf("none: %v %v", err, cs.Think)
	}
	if err := cs.SetEffort(llms.EffortXHigh); err != nil || cs.Think != "max" {
		t.Fatalf("xhigh: %v %v", err, cs.Think)
	}
	if err := cs.SetEffort(llms.EffortDefault); err != nil || cs.Think != "high" {
		t.Fatalf("default should restore profile think: %v %v", err, cs.Think)
	}

	_, err = Provider.Open(context.Background(), llms.Model{Name: "bad", ID: "x", Options: map[string]any{"think": "ultra"}}, llms.Options{})
	if err == nil || !strings.Contains(err.Error(), "think") {
		t.Fatalf("expected think error, got %v", err)
	}
	_, err = Provider.Open(context.Background(), llms.Model{Name: "bad", ID: "x", Options: map[string]any{"host": "ftp://h"}}, llms.Options{})
	if err == nil || !strings.Contains(err.Error(), "host") {
		t.Fatalf("expected host error, got %v", err)
	}
}
