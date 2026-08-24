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

func TestSearchReplaceSchemaIsNarrow(t *testing.T) {
	snap := NewSearchReplaceTool(canonical.EditConfig{}).ToolsSnapshot(nil)
	if len(snap.Snapshot.Offered) != 1 || snap.Snapshot.Offered[0].Name != "search_replace" {
		t.Fatalf("offered tools = %#v", snap.Snapshot.Offered)
	}
	if !reflect.DeepEqual(snap.Handlers, []threads.ToolHandlerBinding{{Name: "search_replace"}}) {
		t.Fatalf("handlers = %#v", snap.Handlers)
	}
	schema := gschema.Schema(snap.Snapshot.Offered[0].Payload.(tool.PayloadJSONSchema))
	if !reflect.DeepEqual(schema.Required, []string{"file_path", "old_string", "new_string"}) {
		t.Fatalf("required = %#v", schema.Required)
	}
	if schema.Properties["old_string"].MinLength == nil || *schema.Properties["old_string"].MinLength != 1 {
		t.Fatal("old_string is not nonempty in schema")
	}
	for _, unsupported := range []string{"edits", "replace_all"} {
		if schema.Properties[unsupported] != nil {
			t.Fatalf("schema exposes %q", unsupported)
		}
	}
}

func TestSearchReplaceOutcomes(t *testing.T) {
	tests := []struct {
		name, content, old, new, want, errorText string
	}{
		{"unique", "alpha beta\n", "beta", "BETA", "alpha BETA\n", ""},
		{"deletion", "alpha beta\n", " beta", "", "alpha\n", ""},
		{"missing", "alpha\n", "beta", "BETA", "alpha\n", "could not find"},
		{"ambiguous", "same same\n", "same", "other", "same same\n", "must be unique"},
		{"empty", "alpha\n", "", "other", "alpha\n", "use the write tool"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "notes.txt")
			if err := os.WriteFile(path, []byte(tt.content), 0o644); err != nil {
				t.Fatal(err)
			}
			result := runSearchReplace(t, NewSearchReplaceTool(canonical.EditConfig{CWD: dir}),
				`{"file_path":"notes.txt","old_string":`+quote(tt.old)+`,"new_string":`+quote(tt.new)+`}`)
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Fatalf("content = %q, want %q", got, tt.want)
			}
			if tt.errorText != "" && !strings.Contains(result.Output, tt.errorText) {
				t.Fatalf("output = %q, want %q", result.Output, tt.errorText)
			}
		})
	}
}

func TestSearchReplacePreservesCanonicalGoPostprocessing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	if err := os.WriteFile(path, []byte("package sample\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	result := runSearchReplace(t, NewSearchReplaceTool(canonical.EditConfig{CWD: dir}),
		`{"file_path":"main.go","old_string":"func main() {}","new_string":"func main(){fmt.Println(\"hi\")}"}`)
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), `import "fmt"`) || !strings.Contains(result.Output, "gofmt: updated") {
		t.Fatalf("canonical postprocessing was not preserved: %q\n%s", result.Output, got)
	}
}

func runSearchReplace(t *testing.T, resolver *SearchReplaceTool, payload string) threads.ToolCallResult {
	t.Helper()
	dispatch, err := resolver.ResolveTool(context.Background(), nil, threads.ToolCall{
		CallID: "c1", Name: "search_replace", Payload: payload,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(dispatch.Items) != 1 {
		t.Fatalf("dispatch items = %#v", dispatch.Items)
	}
	result, ok := dispatch.Items[0].(threads.ToolCallResult)
	if !ok {
		t.Fatalf("dispatch item type = %T", dispatch.Items[0])
	}
	return result
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
