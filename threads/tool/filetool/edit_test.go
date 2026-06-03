package filetool

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	gschema "github.com/google/jsonschema-go/jsonschema"

	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/tool"
)

func TestAddEditSnapshotSpec(t *testing.T) {
	cat := tool.NewCatalog()
	if got := AddEdit(cat, EditConfig{}); got != cat {
		t.Fatalf("AddEdit returned a different catalog")
	}
	snap := cat.Snapshot()
	if len(snap.Offered) != 1 {
		t.Fatalf("expected one offered tool, got %#v", snap.Offered)
	}
	spec := snap.Offered[0]
	if spec.Name != "edit" {
		t.Fatalf("unexpected tool name: %q", spec.Name)
	}
	if spec.Description != "Edit a file using exact text replacement." {
		t.Fatalf("unexpected tool description: %q", spec.Description)
	}
	schema, ok := spec.Payload.(tool.PayloadJSONSchema)
	if !ok {
		t.Fatalf("expected JSON schema payload, got %T", spec.Payload)
	}
	got := gschema.Schema(schema)
	if !reflect.DeepEqual(got.PropertyOrder, []string{"path", "edits"}) {
		t.Fatalf("unexpected property order: %#v", got.PropertyOrder)
	}
	if got.Properties["edits"] == nil || got.Properties["edits"].Items == nil {
		t.Fatalf("missing edits schema: %#v", got.Properties)
	}
	if want := []threads.ToolHandlerBinding{{Name: "edit"}}; !reflect.DeepEqual(cat.ToolsSnapshot(nil).Handlers, want) {
		t.Fatalf("unexpected handler bindings: %#v", cat.ToolsSnapshot(nil).Handlers)
	}
}

func TestNewEditToolDirectProviderResolver(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.txt")
	mustWriteFile(t, path, "alpha\nbeta\n")
	editTool := NewEditTool(EditConfig{CWD: dir})
	var _ threads.ToolProvider = editTool
	var _ threads.ToolResolver = editTool

	dispatch, err := editTool.ResolveTool(context.Background(), nil, threads.ToolCall{
		CallID:  "c1",
		Name:    "edit",
		Payload: `{"path":"notes.txt","edits":[{"oldText":"alpha","newText":"ALPHA"}]}`,
	}, nil)
	if err != nil {
		t.Fatalf("ResolveTool: %v", err)
	}
	result := assertEditResult(t, dispatch, "Edited notes.txt")
	assertEditGolden(t, "success.txt", result.Output)
	assertFile(t, path, "ALPHA\nbeta\n")
	if diff, ok := result.Data["diff"].(string); !ok || !strings.Contains(diff, "-alpha") || !strings.Contains(diff, "+ALPHA") {
		t.Fatalf("unexpected diff data: %#v", result.Data)
	}
}

func TestEditAppliesMultipleEditsAgainstOriginal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.txt")
	mustWriteFile(t, path, "alpha\nbeta\ngamma\n")

	result := resolveEdit(t, NewEditTool(EditConfig{CWD: dir}), `{"path":"notes.txt","edits":[{"oldText":"alpha","newText":"ALPHA"},{"oldText":"gamma","newText":"GAMMA"}]}`)
	assertFile(t, path, "ALPHA\nbeta\nGAMMA\n")
	if !strings.Contains(result.Data["diff"].(string), "+GAMMA") {
		t.Fatalf("diff missing second replacement:\n%s", result.Data["diff"])
	}
}

func TestEditPreservesCRLFLineEndings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.txt")
	mustWriteFile(t, path, "alpha\r\nbeta\r\ngamma\r\n")

	resolveEdit(t, NewEditTool(EditConfig{CWD: dir}), `{"path":"notes.txt","edits":[{"oldText":"beta","newText":"BETA"}]}`)
	assertFile(t, path, "alpha\r\nBETA\r\ngamma\r\n")
}

func TestEditUsesDominantLineEndingForMixedFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.txt")
	mustWriteFile(t, path, "alpha\nbeta\r\ngamma\n")

	resolveEdit(t, NewEditTool(EditConfig{CWD: dir}), `{"path":"notes.txt","edits":[{"oldText":"alpha","newText":"ALPHA"}]}`)
	assertFile(t, path, "ALPHA\nbeta\ngamma\n")
}

func TestEditResultOldContentUsesOriginalBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.txt")
	original := "alpha\r\nbeta\r\n"
	mustWriteFile(t, path, original)

	var builderResult EditResult
	resolveEdit(t, NewEditTool(EditConfig{CWD: dir, ResultBuilder: func(call tool.Call, res EditResult) tool.Item {
		builderResult = res
		return tool.ResultText(call, "ok")
	}}), `{"path":"notes.txt","edits":[{"oldText":"beta","newText":"BETA"}]}`)
	if string(builderResult.OldContent) != original {
		t.Fatalf("OldContent = %q, want original bytes %q", builderResult.OldContent, original)
	}
	if string(builderResult.NewContent) != "alpha\r\nBETA\r\n" {
		t.Fatalf("NewContent = %q, want restored CRLF content", builderResult.NewContent)
	}
}

func TestEditValidationFailuresDoNotWriteOrRunProcessors(t *testing.T) {
	tests := []struct {
		name    string
		content string
		payload string
		want    string
	}{
		{name: "empty old text", content: "alpha\n", payload: `{"path":"notes.txt","edits":[{"oldText":"","newText":"ALPHA"}]}`, want: "oldText must not be empty"},
		{name: "missing old text", content: "alpha\n", payload: `{"path":"notes.txt","edits":[{"oldText":"missing","newText":"ALPHA"}]}`, want: "could not find edits[0]"},
		{name: "duplicate old text", content: "same\nsame\n", payload: `{"path":"notes.txt","edits":[{"oldText":"same","newText":"other"}]}`, want: "oldText must be unique"},
		{name: "no-op", content: "alpha\n", payload: `{"path":"notes.txt","edits":[{"oldText":"alpha","newText":"alpha"}]}`, want: "no changes made"},
		{name: "overlap", content: "abcde\n", payload: `{"path":"notes.txt","edits":[{"oldText":"abc","newText":"ABC"},{"oldText":"bcd","newText":"BCD"}]}`, want: "edits overlap"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "notes.txt")
			mustWriteFile(t, path, tt.content)
			runs := 0
			result := resolveEdit(t, NewEditTool(EditConfig{CWD: dir, Postprocess: PostprocessConfig{BeforeDefault: []FilePostprocessor{FilePostprocessorFunc(func(context.Context, FilePostprocessRequest) (FilePostprocessResult, error) {
				runs++
				return FilePostprocessResult{}, nil
			})}}}), tt.payload)
			assertFile(t, path, tt.content)
			if runs != 0 {
				t.Fatalf("processor ran after verification failure")
			}
			if !strings.Contains(result.Output, tt.want) || result.Data["error"] != result.Output {
				t.Fatalf("unexpected error result: %#v", result)
			}
		})
	}
}

func TestEditFormatsGoAndAddsImports(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	mustWriteFile(t, path, "package sample\n\nfunc main() { println(\"old\") }\n")
	result := resolveEdit(t, NewEditTool(EditConfig{CWD: dir}), `{"path":"main.go","edits":[{"oldText":"func main() { println(\"old\") }","newText":"func main(){fmt.Println(\"hi\")}"}]}`)
	want := `package sample

import "fmt"

func main() { fmt.Println("hi") }
`
	assertFile(t, path, want)
	if result.Output != "Edited main.go\nImports:\n+ fmt\ngofmt: updated 5 lines" {
		t.Fatalf("unexpected output: %q", result.Output)
	}
	assertPostprocessGolden(t, "edit_postprocess_success.txt", result.Output)
	for _, want := range []string{`+import "fmt"`, `+func main() { fmt.Println("hi") }`} {
		if !strings.Contains(result.Data["diff"].(string), want) {
			t.Fatalf("diff missing %q:\n%s", want, result.Data["diff"])
		}
	}
}

func TestEditCanDisableDefaultPostprocessing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.go")
	mustWriteFile(t, path, "package sample\n\nfunc add(){}\n")
	resolveEdit(t, NewEditTool(EditConfig{CWD: dir, Postprocess: PostprocessConfig{DisableDefault: true}}), `{"path":"sample.go","edits":[{"oldText":"func add(){}","newText":"func add(a int,b int)int{return a+b}"}]}`)
	assertFile(t, path, "package sample\n\nfunc add(a int,b int)int{return a+b}\n")
}

func TestEditCustomResultBuilderReceivesPostprocessReports(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	mustWriteFile(t, path, "package sample\n\nfunc main() {}\n")
	var builderResult EditResult
	result := resolveEdit(t, NewEditTool(EditConfig{CWD: dir, ResultBuilder: func(call tool.Call, res EditResult) tool.Item {
		builderResult = res
		return tool.ResultJSON(call, map[string]any{"replacements": res.Replacements, "postprocess": len(res.Postprocess)})
	}}), `{"path":"main.go","edits":[{"oldText":"func main() {}","newText":"func main(){fmt.Println(\"hi\")}"}]}`)
	if !strings.Contains(result.Output, `"replacements":1`) {
		t.Fatalf("unexpected output: %q", result.Output)
	}
	if got := reportProcessors(builderResult.Postprocess); !reflect.DeepEqual(got, []string{"goimports"}) {
		t.Fatalf("builder reports = %#v", got)
	}
	if builderResult.Diff == "" || !strings.Contains(string(builderResult.NewContent), `import "fmt"`) {
		t.Fatalf("builder result missing final content/diff: %#v", builderResult)
	}
}

func TestEditSyntaxErrorIsModelVisibleAndLeavesFileUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.go")
	original := "package bad\n\nfunc main() {}\n"
	mustWriteFile(t, path, original)
	result := resolveEdit(t, NewEditTool(EditConfig{CWD: dir}), `{"path":"bad.go","edits":[{"oldText":"func main() {}","newText":"func main {"}]}`)
	assertFile(t, path, original)
	if !strings.Contains(result.Output, "edited bad.go\ngo syntax: 3:11: expected '(', found '{'") {
		t.Fatalf("unexpected syntax error output: %q", result.Output)
	}
	assertPostprocessGolden(t, "edit_syntax_error.txt", result.Output)
}

func TestEditModelVisibleErrorGoldens(t *testing.T) {
	tests := []struct {
		name    string
		content string
		payload string
		golden  string
	}{
		{
			name:    "missing",
			content: "alpha\n",
			payload: `{"path":"notes.txt","edits":[{"oldText":"missing","newText":"ALPHA"}]}`,
			golden:  "missing_old_text.txt",
		},
		{
			name:    "duplicate",
			content: "same\nsame\n",
			payload: `{"path":"notes.txt","edits":[{"oldText":"same","newText":"other"}]}`,
			golden:  "duplicate_old_text.txt",
		},
		{
			name:    "overlap",
			content: "abcde\n",
			payload: `{"path":"notes.txt","edits":[{"oldText":"abc","newText":"ABC"},{"oldText":"bcd","newText":"BCD"}]}`,
			golden:  "overlapping_edits.txt",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "notes.txt")
			mustWriteFile(t, path, tt.content)
			result := resolveEdit(t, NewEditTool(EditConfig{CWD: dir}), tt.payload)
			assertFile(t, path, tt.content)
			assertEditGolden(t, tt.golden, result.Output)
		})
	}
}

func resolveEdit(t *testing.T, resolver *EditTool, payload string) threads.ToolCallResult {
	t.Helper()
	dispatch, err := resolver.ResolveTool(context.Background(), nil, threads.ToolCall{CallID: "c1", Name: "edit", Payload: payload}, nil)
	if err != nil {
		t.Fatalf("ResolveTool: %v", err)
	}
	return onlyResult(t, dispatch)
}

func assertEditResult(t *testing.T, dispatch threads.ToolDispatch, want string) threads.ToolCallResult {
	t.Helper()
	result := onlyResult(t, dispatch)
	if !dispatch.Started {
		t.Fatalf("dispatch not started: %#v", dispatch)
	}
	if dispatch.Recovery != threads.ToolRecoveryUnsafe {
		t.Fatalf("Recovery = %q, want unsafe", dispatch.Recovery)
	}
	if result.Output != want {
		t.Fatalf("unexpected output:\n got: %q\nwant: %q", result.Output, want)
	}
	return result
}

func assertEditGolden(t *testing.T, name string, got string) {
	t.Helper()
	path := filepath.Join("testdata", "edit", "golden", name)
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v", path, err)
	}
	wantText := strings.TrimSuffix(string(want), "\n")
	if got != wantText {
		t.Fatalf("golden mismatch for %s:\n--- got ---\n%q\n--- want ---\n%q", name, got, wantText)
	}
}
