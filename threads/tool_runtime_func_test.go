package threads_test

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/mackross/agentloom/threads"
)

func TestToolProviderFuncSnapshotIsIsolatedFromProviderData(t *testing.T) {
	load := []byte(`{"function":"tool/calc@v1"}`)
	thread := threads.New()
	thread.SetToolProvider(threads.ToolProviderFunc(func(_ threads.Thread) threads.ToolsSnapshot {
		return threads.ToolsSnapshot{
			Snapshot: threads.ToolOfferSnapshot{Offered: []threads.ToolSpec{{
				Name:    "calc",
				Payload: threads.ToolPayloadText(),
			}}},
			Handlers: []threads.ToolHandlerBinding{{Name: "calc", HandlerLoadData: load}},
		}
	}))
	copy(load, `{"function":"tool/other@v1"}`)

	snap, err := thread.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	var tools *threads.ToolsSnapshot
	for _, item := range snap.Items {
		if item.Tools != nil {
			tools = item.Tools
		}
	}
	if tools == nil {
		t.Fatal("expected a queued tools snapshot")
	}
	if want := []byte(`{"function":"tool/calc@v1"}`); !bytes.Equal(tools.Handlers[0].HandlerLoadData, want) {
		t.Fatalf("queued snapshot shares provider data: %s", tools.Handlers[0].HandlerLoadData)
	}
}

func TestToolResolverFuncReceivesHandlerLoadData(t *testing.T) {
	want := json.RawMessage(`{"function":"tool/write-file@v1"}`)
	var got json.RawMessage
	resolver := threads.ToolResolverFunc(func(_ context.Context, _ threads.Thread, call threads.ToolCall, load json.RawMessage) (threads.ToolDispatch, error) {
		got = load
		return threads.ToolDispatch{Items: []threads.Item{threads.ToolCallResult{CallID: call.CallID, Output: "ok"}}}, nil
	})
	dispatch, err := resolver.ResolveTool(context.Background(), nil, threads.ToolCall{CallID: "c1", Name: "write_file"}, want)
	if err != nil {
		t.Fatalf("resolve tool: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("unexpected handler load data: %s", got)
	}
	if len(dispatch.Items) != 1 {
		t.Fatalf("unexpected dispatch items: %#v", dispatch.Items)
	}
}
