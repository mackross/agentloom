package tool

import (
	"context"
	"testing"

	"github.com/mackross/agentloom/threads"
)

type structToolTestArgs struct {
	Value string `json:"value"`
}

type structToolTestDelegate struct{}

func (structToolTestDelegate) OnStructToolCall(_ context.Context, _ threads.Thread, call Call, args structToolTestArgs) Item {
	return ResultText(call, args.Value)
}

func TestStructToolImplementsHandler(t *testing.T) {
	var _ Handler = NewStructTool[structToolTestArgs]("answer", "answer", structToolTestDelegate{})
}

func TestStructToolHandleToolCall(t *testing.T) {
	st := NewStructTool[structToolTestArgs]("answer", "answer", structToolTestDelegate{})
	var got Item
	handling, err := st.HandleToolCall(context.Background(), nil, Call{CallID: "c1", Name: "answer", Payload: `{"value":"ok"}`}, func(item Item) error {
		got = item
		return nil
	})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if handling.Continue != threads.ToolContinueManual {
		t.Fatalf("unexpected handling: %#v", handling)
	}
	result := got.(threads.ToolCallResult)
	if result.CallID != "c1" || result.Output != "ok" {
		t.Fatalf("unexpected result: %#v", result)
	}
}
