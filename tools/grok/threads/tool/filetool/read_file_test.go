package filetool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	gschema "github.com/google/jsonschema-go/jsonschema"

	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/tool"
	canonical "github.com/mackross/agentloom/threads/tool/filetool"
)

func TestReadFileSchema(t *testing.T) {
	r := NewReadFileTool(ReadConfig{})
	if r.ReadTool == nil {
		t.Fatal("canonical ReadTool was not embedded")
	}

	snap := r.ToolsSnapshot(nil)
	if len(snap.Snapshot.Offered) != 1 {
		t.Fatalf("offered tools = %#v", snap.Snapshot.Offered)
	}
	spec := snap.Snapshot.Offered[0]
	if spec.Name != "read_file" || spec.Description != "Read text contents of a file." {
		t.Fatalf("unexpected spec: %#v", spec)
	}
	if got := snap.Handlers; !reflect.DeepEqual(got, []threads.ToolHandlerBinding{{Name: "read_file"}}) {
		t.Fatalf("handlers = %#v", got)
	}

	schema := gschema.Schema(spec.Payload.(tool.PayloadJSONSchema))
	if !reflect.DeepEqual(schema.Required, []string{"target_file"}) {
		t.Fatalf("required = %#v", schema.Required)
	}
	if !reflect.DeepEqual(schema.PropertyOrder, []string{"target_file", "offset", "limit"}) {
		t.Fatalf("properties = %#v", schema.PropertyOrder)
	}
	for _, unsupported := range []string{"path", "pages", "format"} {
		if schema.Properties[unsupported] != nil {
			t.Fatalf("schema unexpectedly contains %q", unsupported)
		}
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"additionalProperties":false`) {
		t.Fatalf("schema does not forbid additional properties: %s", raw)
	}
	if schema.Properties["limit"].Minimum == nil || *schema.Properties["limit"].Minimum != 0 {
		t.Fatalf("limit minimum = %#v", schema.Properties["limit"].Minimum)
	}
}

func TestReadFileDelegatesPathAndRange(t *testing.T) {
	dir := t.TempDir()
	const contents = "one\ntwo\nthree\nfour\n"
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	wrapped := NewReadFileTool(ReadConfig{CWD: dir})
	canon := canonical.NewReadTool(canonical.ReadConfig{CWD: dir})
	got := resolveOutput(t, wrapped, threads.ToolCall{
		CallID: "wrapped", Name: "read_file",
		Payload: `{"target_file":"notes.txt","offset":2,"limit":2}`,
	})
	want := resolveOutput(t, canon, threads.ToolCall{
		CallID: "canonical", Name: "read",
		Payload: `{"path":"notes.txt","offset":2,"limit":2}`,
	})
	if got != want || got != "two\nthree\n" {
		t.Fatalf("wrapped output = %q, canonical output = %q", got, want)
	}
}

func TestReadFileRejectsUnsupportedFields(t *testing.T) {
	r := NewReadFileTool(ReadConfig{CWD: t.TempDir()})
	out := resolveOutput(t, r, threads.ToolCall{
		CallID: "c1", Name: "read_file",
		Payload: `{"target_file":"x","pages":"1"}`,
	})
	if !strings.Contains(out, `unknown field "pages"`) {
		t.Fatalf("output = %q", out)
	}
}

type resolver interface {
	ResolveTool(context.Context, threads.Thread, threads.ToolCall, json.RawMessage) (threads.ToolDispatch, error)
}

func resolveOutput(t *testing.T, r resolver, call threads.ToolCall) string {
	t.Helper()
	dispatch, err := r.ResolveTool(context.Background(), nil, call, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !dispatch.Started || len(dispatch.Items) != 1 {
		t.Fatalf("dispatch = %#v", dispatch)
	}
	result, ok := dispatch.Items[0].(threads.ToolCallResult)
	if !ok {
		t.Fatalf("result type = %T", dispatch.Items[0])
	}
	return result.Output
}
