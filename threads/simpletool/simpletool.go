package simpletool

import (
	"context"
	"encoding/json"

	"github.com/mackross/agentloom/threads"
)

type ProviderFunc func() threads.ToolsSnapshot

func (f ProviderFunc) ToolsSnapshot() threads.ToolsSnapshot {
	if f == nil {
		panic("simpletool.ProviderFunc is nil")
	}
	return cloneToolsSnapshot(f())
}

type ResolverFunc func(context.Context, threads.ToolCall, json.RawMessage) (threads.ToolDispatch, error)

func (f ResolverFunc) ResolveTool(ctx context.Context, call threads.ToolCall, handlerLoadData json.RawMessage) (threads.ToolDispatch, error) {
	if f == nil {
		panic("simpletool.ResolverFunc is nil")
	}
	return f(ctx, call, append(json.RawMessage(nil), handlerLoadData...))
}

func cloneToolsSnapshot(in threads.ToolsSnapshot) threads.ToolsSnapshot {
	buf, err := json.Marshal(in)
	if err != nil {
		panic("simpletool clone tools snapshot marshal failed: " + err.Error())
	}
	var out threads.ToolsSnapshot
	if err := json.Unmarshal(buf, &out); err != nil {
		panic("simpletool clone tools snapshot unmarshal failed: " + err.Error())
	}
	return out
}
