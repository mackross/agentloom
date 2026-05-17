package simpletool

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/mackross/agentloom/threads"
)

func TestProviderFuncClonesToolsSnapshot(t *testing.T) {
	provider := ProviderFunc(func(_ *threads.Thread) threads.ToolsSnapshot {
		return threads.ToolsSnapshot{
			Snapshot: threads.ToolOfferSnapshot{Offered: []threads.ToolSpec{{
				Name:        "calc",
				Description: "calculate",
				Payload:     threads.ToolPayloadJSONSchema{},
			}}},
			Handlers: []threads.ToolHandlerBinding{{
				Name:            "calc",
				HandlerLoadData: []byte(`{"function":"tool/calc@v1"}`),
			}},
		}
	})

	got := provider.ToolsSnapshot(nil)
	got.Handlers[0].HandlerLoadData = []byte(`{"function":"tool/other@v1"}`)

	again := provider.ToolsSnapshot(nil)
	if want := []byte(`{"function":"tool/calc@v1"}`); !bytes.Equal(again.Handlers[0].HandlerLoadData, want) {
		t.Fatalf("provider snapshot was not cloned: %s", string(again.Handlers[0].HandlerLoadData))
	}
}

func TestResolverFuncReceivesOpaqueLoadData(t *testing.T) {
	wantData := json.RawMessage(`{"function":"tool/write-file@v1","filename":"notes.txt"}`)
	var gotName string
	var gotData json.RawMessage

	resolver := ResolverFunc(func(_ context.Context, _ *threads.Thread, call threads.ToolCall, handlerLoadData json.RawMessage) (threads.ToolDispatch, error) {
		gotName = call.Name
		gotData = append(json.RawMessage(nil), handlerLoadData...)
		return threads.ToolDispatch{Items: []threads.Item{threads.AssistantText("ok")}}, nil
	})

	dispatch, err := resolver.ResolveTool(context.Background(), nil, threads.ToolCall{Name: "write_file"}, wantData)
	if err != nil {
		t.Fatalf("resolve tool: %v", err)
	}
	if gotName != "write_file" {
		t.Fatalf("unexpected tool name: %q", gotName)
	}
	if !bytes.Equal(gotData, wantData) {
		t.Fatalf("unexpected handler load data: %s", string(gotData))
	}
	if len(dispatch.Items) != 1 || dispatch.Items[0] != threads.AssistantText("ok") {
		t.Fatalf("unexpected dispatch items: %#v", dispatch.Items)
	}
}
