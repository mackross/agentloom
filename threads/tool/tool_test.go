package tool

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	gschema "github.com/google/jsonschema-go/jsonschema"
)

func TestSnapshotJSONRoundTripPreservesAllowedNilVsEmpty(t *testing.T) {
	t.Run("nil", func(t *testing.T) {
		var snap Snapshot
		data, err := json.Marshal(snap)
		if err != nil {
			t.Fatalf("marshal snapshot: %v", err)
		}
		var got Snapshot
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("unmarshal snapshot: %v", err)
		}
		if got.Allowed != nil {
			t.Fatalf("expected nil allowed, got %#v", got.Allowed)
		}
	})

	t.Run("empty", func(t *testing.T) {
		snap := Snapshot{Allowed: []string{}}
		data, err := json.Marshal(snap)
		if err != nil {
			t.Fatalf("marshal snapshot: %v", err)
		}
		var got Snapshot
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("unmarshal snapshot: %v", err)
		}
		if got.Allowed == nil || len(got.Allowed) != 0 {
			t.Fatalf("expected empty allowed, got %#v", got.Allowed)
		}
	})
}

func TestSpecJSONRoundTripPreservesOpenAIPayloads(t *testing.T) {
	specs := []Spec{
		{Name: "text", Payload: PayloadText()},
		{Name: "json", Payload: PayloadJSONSchema(gschema.Schema{Type: "object"})},
		{Name: "regex", Payload: PayloadRegexp("^[a-z]+$")},
		{Name: "lark", Payload: PayloadLark("start: WORD")},
	}
	for _, want := range specs {
		data, err := json.Marshal(want)
		if err != nil {
			t.Fatalf("marshal spec %q: %v", want.Name, err)
		}
		var got Spec
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("unmarshal spec %q: %v", want.Name, err)
		}
		if got.Name != want.Name {
			t.Fatalf("wrong name: got %q want %q", got.Name, want.Name)
		}
		switch want.Name {
		case "json":
			schema, ok := got.Payload.(PayloadJSONSchema)
			if !ok || gschema.Schema(schema).Type != "object" {
				t.Fatalf("expected object json schema payload, got %T %#v", got.Payload, got.Payload)
			}
		case "regex":
			if _, ok := got.Payload.(PayloadRegexp); !ok {
				t.Fatalf("expected regexp payload, got %T", got.Payload)
			}
		case "lark":
			if _, ok := got.Payload.(PayloadLark); !ok {
				t.Fatalf("expected lark payload, got %T", got.Payload)
			}
		}
	}
}

func TestPayloadForInfersStructSchema(t *testing.T) {
	type args struct {
		Token string `json:"token"`
		Count int    `json:"count,omitempty"`
	}

	p := PayloadFor[args]()
	schema, ok := p.(PayloadJSONSchema)
	if !ok {
		t.Fatalf("expected json schema payload, got %T", p)
	}
	got := gschema.Schema(schema)
	if got.Type != "object" {
		t.Fatalf("expected object schema, got %#v", got.Type)
	}
	if got.Properties["token"] == nil || got.Properties["token"].Type != "string" {
		t.Fatalf("unexpected token property: %#v", got.Properties["token"])
	}
	if got.Properties["count"] == nil || got.Properties["count"].Type != "integer" {
		t.Fatalf("unexpected count property: %#v", got.Properties["count"])
	}
	if !slices.Contains(got.Required, "token") || slices.Contains(got.Required, "count") {
		t.Fatalf("unexpected required fields: %#v", got.Required)
	}
}

func TestPayloadForPanicsOnNonStruct(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil || !strings.Contains(r.(string), "requires struct type") {
			t.Fatalf("expected struct type panic, got %#v", r)
		}
	}()
	_ = PayloadFor[string]()
}
