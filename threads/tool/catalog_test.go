package tool

import (
	"context"
	"reflect"
	"testing"

	gschema "github.com/google/jsonschema-go/jsonschema"
	"github.com/mackross/agentloom/threads"
)

type resultView interface {
	ToolCallID() string
	ToolOutput() string
	ToolData() map[string]any
}

func TestCatalogSnapshotUsesPolicyAndPreservesAddOrder(t *testing.T) {
	cat := NewCatalog().
		AddFunc(Spec{Name: "first", Payload: PayloadText()}, func(_ context.Context, _ *threads.Thread, _ Call, ret ReturnItem) (Handling, error) {
			return Handling{}, ret(ResultText(Call{CallID: "first"}, "ok"))
		}).
		AddFunc(Spec{Name: "second", Payload: PayloadText()}, func(_ context.Context, _ *threads.Thread, _ Call, ret ReturnItem) (Handling, error) {
			return Handling{}, ret(ResultText(Call{CallID: "second"}, "ok"))
		}).
		Disallow("first")

	snap := cat.Snapshot()
	if got := []string{snap.Offered[0].Name, snap.Offered[1].Name}; !reflect.DeepEqual(got, []string{"first", "second"}) {
		t.Fatalf("unexpected offered order: %#v", got)
	}
	if got := snap.Allowed; !reflect.DeepEqual(got, []string{"second"}) {
		t.Fatalf("unexpected allowed names: %#v", got)
	}

	cat.AddFunc(Spec{Name: "third", Payload: PayloadText()}, func(_ context.Context, _ *threads.Thread, _ Call, ret ReturnItem) (Handling, error) {
		return Handling{}, ret(ResultText(Call{CallID: "third"}, "ok"))
	})
	snap = cat.Snapshot()
	if got := snap.Allowed; !reflect.DeepEqual(got, []string{"second", "third"}) {
		t.Fatalf("unexpected allowed names after add: %#v", got)
	}
}

func TestJSONReturnsSpecAndHandlerForCommonUsage(t *testing.T) {
	cat := NewCatalog().Add(JSON("lookup", "lookup item", func(_ context.Context, _ *threads.Thread, call Call, args struct {
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
	}, JSONHandler(func(_ context.Context, _ *threads.Thread, call Call, args struct {
		ID string `json:"id"`
	}) Item {
		return ResultText(call, args.ID)
	}))

	handler, err := cat.LoadTool("lookup")
	if err != nil {
		t.Fatalf("load tool: %v", err)
	}
	var item Item
	_, err = handler.HandleToolCall(context.Background(), nil, Call{
		CallID:  "c1",
		Name:    "lookup",
		Payload: "{",
	}, func(v Item) error {
		item = v
		return nil
	})
	if err != nil {
		t.Fatalf("handle tool call: %v", err)
	}
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

func TestCatalogDispatchBuffersImmediateReturns(t *testing.T) {
	var lateReturn ReturnItem
	cat := NewCatalog().AddFunc(Spec{Name: "work", Payload: PayloadText()}, func(_ context.Context, _ *threads.Thread, call Call, ret ReturnItem) (Handling, error) {
		if err := ret(ResultText(call, "now")); err != nil {
			return Handling{}, err
		}
		lateReturn = ret
		return Handling{}, nil
	})
	dispatch, err := cat.Dispatch(context.Background(), nil, Call{CallID: "c1", Name: "work"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if got := len(dispatch.Items); got != 1 {
		t.Fatalf("unexpected immediate item count: %d", got)
	}
	if err := lateReturn(ResultText(Call{CallID: "c1"}, "later")); err == nil {
		t.Fatal("expected late return without thread to fail")
	}
}

func TestCatalogDispatchLateReturnRequiresEventLoop(t *testing.T) {
	var lateReturn ReturnItem
	cat := NewCatalog().AddFunc(Spec{Name: "work", Payload: PayloadText()}, func(_ context.Context, _ *threads.Thread, _ Call, ret ReturnItem) (Handling, error) {
		lateReturn = ret
		return Handling{}, nil
	})
	if _, err := cat.Dispatch(context.Background(), nil, Call{CallID: "c1", Name: "work"}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if err := lateReturn(ResultText(Call{CallID: "c1"}, "later")); err == nil {
		t.Fatal("expected late return without event loop to fail")
	}
}

func TestCatalogDispatchRoutesLateReturnThroughThreadEventLoop(t *testing.T) {
	thread := threads.New()
	loop := threads.NewEventLoop(thread)
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- loop.Run(runCtx) }()
	defer func() {
		_ = loop.Close()
		cancel()
		<-done
	}()

	call := Call{CallID: "c1", Name: "work"}
	if err := loop.Do(context.Background(), func(thread *threads.Thread) error {
		thread.QueueItem(threads.ToolCall{CallID: call.CallID, Name: call.Name})
		thread.QueueItem(threads.ToolCallStarted{CallID: call.CallID})
		return nil
	}); err != nil {
		t.Fatalf("seed tool call: %v", err)
	}

	var lateReturn ReturnItem
	cat := NewCatalog().AddFunc(Spec{Name: "work", Payload: PayloadText()}, func(_ context.Context, _ *threads.Thread, _ Call, ret ReturnItem) (Handling, error) {
		lateReturn = ret
		return Handling{}, nil
	})
	if _, err := cat.Dispatch(context.Background(), thread, call); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if err := lateReturn(ResultText(call, "later")); err != nil {
		t.Fatalf("late return: %v", err)
	}
	var found bool
	if err := loop.Do(context.Background(), func(thread *threads.Thread) error {
		snap, err := thread.Snapshot()
		if err != nil {
			return err
		}
		for _, item := range snap.Items {
			found = found || item.Type == "tool_result" && item.ID == call.CallID && item.Output == "later"
		}
		return nil
	}); err != nil {
		t.Fatalf("inspect thread: %v", err)
	}
	if !found {
		t.Fatal("late return was not queued on thread")
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

	cat := NewCatalog().AddFunc(Spec{Name: "json", Payload: PayloadFor[args]()}, func(_ context.Context, _ *threads.Thread, _ Call, ret ReturnItem) (Handling, error) {
		return Handling{}, ret(ResultText(Call{CallID: "json"}, "ok"))
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
