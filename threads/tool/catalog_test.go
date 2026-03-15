package tool

import (
	"context"
	"reflect"
	"testing"

	gschema "github.com/google/jsonschema-go/jsonschema"
)

type resultView interface {
	ToolCallID() string
	ToolOutput() string
	ToolData() map[string]any
}

func TestCatalogSnapshotUsesPolicyAndPreservesAddOrder(t *testing.T) {
	cat := NewCatalog().
		AddFunc(Spec{Name: "first", Payload: PayloadText()}, func(context.Context, Call) Item {
			return ResultText(Call{CallID: "first"}, "ok")
		}).
		AddFunc(Spec{Name: "second", Payload: PayloadText()}, func(context.Context, Call) Item {
			return ResultText(Call{CallID: "second"}, "ok")
		}).
		Disallow("first")

	snap := cat.Snapshot()
	if got := []string{snap.Offered[0].Name, snap.Offered[1].Name}; !reflect.DeepEqual(got, []string{"first", "second"}) {
		t.Fatalf("unexpected offered order: %#v", got)
	}
	if got := snap.Allowed; !reflect.DeepEqual(got, []string{"second"}) {
		t.Fatalf("unexpected allowed names: %#v", got)
	}

	cat.AddFunc(Spec{Name: "third", Payload: PayloadText()}, func(context.Context, Call) Item {
		return ResultText(Call{CallID: "third"}, "ok")
	})
	snap = cat.Snapshot()
	if got := snap.Allowed; !reflect.DeepEqual(got, []string{"second", "third"}) {
		t.Fatalf("unexpected allowed names after add: %#v", got)
	}
}

func TestJSONReturnsSpecAndHandlerForCommonUsage(t *testing.T) {
	cat := NewCatalog().Add(JSON("lookup", "lookup item", func(_ context.Context, call Call, args struct {
		ID string `json:"id"`
	}) Item {
		return ResultText(call, args.ID)
	}))

	snap := cat.Snapshot()
	if got := snap.Offered[0].Name; got != "lookup" {
		t.Fatalf("unexpected tool name: %q", got)
	}
	if _, ok := snap.Offered[0].Payload.(PayloadJSONSchema); !ok {
		t.Fatalf("expected JSON schema payload, got %T", snap.Offered[0].Payload)
	}
}

func TestJSONHandlerReturnsModelVisibleErrorItemOnBadPayload(t *testing.T) {
	cat := NewCatalog().Add(Spec{
		Name:        "lookup",
		Description: "lookup item",
		Payload: PayloadFor[struct {
			ID string `json:"id"`
		}](),
	}, JSONHandler(func(_ context.Context, call Call, args struct {
		ID string `json:"id"`
	}) Item {
		return ResultText(call, args.ID)
	}))

	handler, err := cat.LoadTool("lookup")
	if err != nil {
		t.Fatalf("load tool: %v", err)
	}
	item := handler.HandleToolCall(context.Background(), Call{
		CallID:  "c1",
		Name:    "lookup",
		Payload: "{",
	})
	view, ok := item.(resultView)
	if !ok {
		t.Fatalf("expected result item view, got %T", item)
	}
	if got := view.ToolCallID(); got != "c1" {
		t.Fatalf("unexpected call id: %q", got)
	}
	if got := view.ToolOutput(); got == "" {
		t.Fatal("expected non-empty error result")
	}
}

func TestResultJSONBuildsToolCallResultableItem(t *testing.T) {
	item := ResultJSON(Call{CallID: "c42", Name: "sum"}, map[string]any{"sum": 3})
	view, ok := item.(resultView)
	if !ok {
		t.Fatalf("expected result item view, got %T", item)
	}
	if got := view.ToolCallID(); got != "c42" {
		t.Fatalf("unexpected call id: %q", got)
	}
	if got := view.ToolOutput(); got != `{"sum":3}` {
		t.Fatalf("unexpected result: %q", got)
	}
	meta := view.ToolData()
	if got := meta["json"]; !reflect.DeepEqual(got, map[string]any{"sum": 3}) {
		t.Fatalf("unexpected meta payload: %#v", got)
	}
}

func TestSnapshotDeepClonesPayloadDefinitions(t *testing.T) {
	type args struct {
		Value string `json:"value"`
	}

	cat := NewCatalog().AddFunc(Spec{Name: "json", Payload: PayloadFor[args]()}, func(context.Context, Call) Item {
		return ResultText(Call{CallID: "json"}, "ok")
	})
	first := cat.Snapshot()
	second := cat.Snapshot()

	schema, ok := first.Offered[0].Payload.(PayloadJSONSchema)
	if !ok {
		t.Fatalf("expected json schema payload, got %T", first.Offered[0].Payload)
	}
	gs := gschema.Schema(schema)
	gs.Properties["extra"] = &gschema.Schema{Type: "string"}

	other, ok := second.Offered[0].Payload.(PayloadJSONSchema)
	if !ok {
		t.Fatalf("expected json schema payload, got %T", second.Offered[0].Payload)
	}
	if gschema.Schema(other).Properties["extra"] != nil {
		t.Fatalf("expected independent cloned schema, got %#v", gschema.Schema(other).Properties["extra"])
	}
}
