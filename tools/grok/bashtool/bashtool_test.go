package bashtool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mackross/agentloom/threads"
)

func TestSnapshotExposesOnlyRunTerminalCommand(t *testing.T) {
	snapshot := New(Config{CWD: t.TempDir(), Async: true}).ToolsSnapshot(nil)
	if len(snapshot.Snapshot.Offered) != 1 || snapshot.Snapshot.Offered[0].Name != Name {
		t.Fatalf("offered = %#v", snapshot.Snapshot.Offered)
	}
	data, err := json.Marshal(snapshot.Snapshot.Offered[0].Payload)
	if err != nil {
		t.Fatal(err)
	}
	schema := string(data)
	for _, want := range []string{`"required":["command","description"]`, `"minimum":0`, `"additionalProperties":false`} {
		if !strings.Contains(schema, want) {
			t.Fatalf("schema %s does not contain %s", schema, want)
		}
	}
	for _, unwanted := range []string{`"background"`, `"bash"`} {
		if strings.Contains(schema, unwanted) {
			t.Fatalf("schema exposes %s: %s", unwanted, schema)
		}
	}
	if snapshot.Snapshot.Allowed[0] != Name || snapshot.Handlers[0].Name != Name {
		t.Fatalf("canonical name leaked into routing: %#v", snapshot)
	}
}

func TestResolveRequiresDescription(t *testing.T) {
	tool := New(Config{CWD: t.TempDir()})
	snapshot := tool.ToolsSnapshot(nil)
	dispatch, err := tool.ResolveTool(context.Background(), nil, threads.ToolCall{
		CallID: "c1", Name: Name, Payload: `{"command":"true"}`,
	}, snapshot.Handlers[0].HandlerLoadData)
	if err != nil {
		t.Fatal(err)
	}
	result := dispatch.Items[0].(threads.ToolCallResult)
	if !strings.Contains(result.Output, "description is required") || dispatch.Started {
		t.Fatalf("unexpected validation dispatch: %#v", dispatch)
	}
}

func TestResolveAcceptsDescriptionAndRunsForeground(t *testing.T) {
	tool := New(Config{CWD: t.TempDir(), Async: true})
	result, dispatch := run(t, tool, `{"command":"printf done","description":"verify execution"}`)
	if result.Output != "done" || !dispatch.Started {
		t.Fatalf("unexpected result: %#v, dispatch: %#v", result, dispatch)
	}
}

func TestTimeoutIsMilliseconds(t *testing.T) {
	tool := New(Config{CWD: t.TempDir()})
	start := time.Now()
	result, _ := run(t, tool, `{"command":"sleep 10","timeout":150,"description":"test timeout units"}`)
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("150ms timeout took %v", elapsed)
	}
	if !strings.Contains(strings.ToLower(result.Output), "deadline") &&
		!strings.Contains(strings.ToLower(result.Output), "timeout") {
		t.Fatalf("expected timeout result, got %q", result.Output)
	}
}

func run(t *testing.T, tool *Tool, payload string) (threads.ToolCallResult, threads.ToolDispatch) {
	t.Helper()
	snapshot := tool.ToolsSnapshot(nil)
	dispatch, err := tool.ResolveTool(context.Background(), nil, threads.ToolCall{
		CallID: "c1", Name: Name, Payload: payload,
	}, snapshot.Handlers[0].HandlerLoadData)
	if err != nil {
		t.Fatal(err)
	}
	if len(dispatch.Items) != 1 {
		t.Fatalf("items = %#v", dispatch.Items)
	}
	return dispatch.Items[0].(threads.ToolCallResult), dispatch
}
