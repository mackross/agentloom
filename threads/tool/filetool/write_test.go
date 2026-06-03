package filetool

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	gschema "github.com/google/jsonschema-go/jsonschema"

	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/tool"
)

func TestAddWriteSnapshotSpec(t *testing.T) {
	cat := tool.NewCatalog()
	if got := AddWrite(cat, WriteConfig{}); got != cat {
		t.Fatalf("AddWrite returned a different catalog")
	}

	snap := cat.Snapshot()
	if len(snap.Offered) != 1 {
		t.Fatalf("expected one offered tool, got %#v", snap.Offered)
	}
	spec := snap.Offered[0]
	if spec.Name != "write" {
		t.Fatalf("unexpected tool name: %q", spec.Name)
	}
	if spec.Description != "Create or overwrite a file" {
		t.Fatalf("unexpected tool description: %q", spec.Description)
	}
	schema, ok := spec.Payload.(tool.PayloadJSONSchema)
	if !ok {
		t.Fatalf("expected JSON schema payload, got %T", spec.Payload)
	}
	got := gschema.Schema(schema)
	if got.Type != "object" {
		t.Fatalf("schema type = %q, want object", got.Type)
	}
	if !reflect.DeepEqual(got.PropertyOrder, []string{"path", "content"}) {
		t.Fatalf("unexpected property order: %#v", got.PropertyOrder)
	}
	wantDescriptions := map[string]string{
		"path":    "Relative or absolute file path",
		"content": "Exact text content for entire file",
	}
	for name, want := range wantDescriptions {
		prop := got.Properties[name]
		if prop == nil {
			t.Fatalf("missing schema property %q in %#v", name, got.Properties)
		}
		if prop.Description != want {
			t.Fatalf("property %q description = %q, want %q", name, prop.Description, want)
		}
	}
	tools := cat.ToolsSnapshot(nil)
	if want := []threads.ToolHandlerBinding{{Name: "write"}}; !reflect.DeepEqual(tools.Handlers, want) {
		t.Fatalf("unexpected handler bindings: %#v", tools.Handlers)
	}
}

func TestWriteConfigDescriptionOverrides(t *testing.T) {
	toolDescription := "custom write"
	pathDescription := "custom path"
	contentDescription := "custom content"
	cat := AddWrite(tool.NewCatalog(), WriteConfig{
		ToolDescription:    &toolDescription,
		PathDescription:    &pathDescription,
		ContentDescription: &contentDescription,
	})
	spec := cat.Snapshot().Offered[0]
	if spec.Description != toolDescription {
		t.Fatalf("tool description = %q, want %q", spec.Description, toolDescription)
	}
	schema := gschema.Schema(spec.Payload.(tool.PayloadJSONSchema))
	if schema.Properties["path"].Description != pathDescription ||
		schema.Properties["content"].Description != contentDescription {
		t.Fatalf("unexpected property descriptions: %#v", schema.Properties)
	}
}

func TestNewWriteToolDirectProviderResolver(t *testing.T) {
	dir := t.TempDir()
	writeTool := NewWriteTool(WriteConfig{CWD: dir})
	var _ threads.ToolProvider = writeTool
	var _ threads.ToolResolver = writeTool

	dispatch, err := writeTool.ResolveTool(context.Background(), nil, threads.ToolCall{
		CallID:  "c1",
		Name:    "write",
		Payload: `{"path":"notes.txt","content":"hello\n"}`,
	}, nil)
	if err != nil {
		t.Fatalf("ResolveTool: %v", err)
	}
	assertWriteResult(t, dispatch, "Wrote notes.txt")
	assertFile(t, filepath.Join(dir, "notes.txt"), "hello\n")
}

func TestWriteCreatesParentDirectories(t *testing.T) {
	dir := t.TempDir()
	dispatch, err := AddWrite(tool.NewCatalog(), WriteConfig{CWD: dir}).Dispatch(context.Background(), nil, tool.Call{
		CallID:  "c1",
		Name:    "write",
		Payload: `{"path":"nested/dir/file.txt","content":"created"}`,
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	assertWriteResult(t, dispatch, "Wrote nested/dir/file.txt")
	assertFile(t, filepath.Join(dir, "nested", "dir", "file.txt"), "created")
}

func TestWriteOverwritesExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.txt")
	mustWriteFile(t, path, "old contents")

	dispatch, err := NewWriteTool(WriteConfig{CWD: dir}).ResolveTool(context.Background(), nil, threads.ToolCall{
		CallID:  "c1",
		Name:    "write",
		Payload: `{"path":"notes.txt","content":"new"}`,
	}, nil)
	if err != nil {
		t.Fatalf("ResolveTool: %v", err)
	}
	assertWriteResult(t, dispatch, "Wrote notes.txt")
	assertFile(t, path, "new")
}

func TestWriteAbsolutePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "abs.txt")
	dispatch, err := NewWriteTool(WriteConfig{CWD: filepath.Join(dir, "unused")}).ResolveTool(context.Background(), nil, threads.ToolCall{
		CallID:  "c1",
		Name:    "write",
		Payload: `{"path":` + jsonString(path) + `,"content":"absolute"}`,
	}, nil)
	if err != nil {
		t.Fatalf("ResolveTool: %v", err)
	}
	assertWriteResult(t, dispatch, "Wrote "+path)
	assertFile(t, path, "absolute")
}

func TestWriteFormatsGoAndAddsImports(t *testing.T) {
	dir := t.TempDir()
	content := `package sample

func main(){fmt.Println("hi")}
`
	dispatch, err := NewWriteTool(WriteConfig{CWD: dir}).ResolveTool(context.Background(), nil, threads.ToolCall{
		CallID:  "c1",
		Name:    "write",
		Payload: `{"path":"sample/main.go","content":` + jsonString(content) + `}`,
	}, nil)
	if err != nil {
		t.Fatalf("ResolveTool: %v", err)
	}
	want := `package sample

import "fmt"

func main() { fmt.Println("hi") }
`
	assertWriteResult(t, dispatch, "Wrote sample/main.go\nImports:\n+ fmt\ngofmt: updated 5 lines")
	assertFile(t, filepath.Join(dir, "sample", "main.go"), want)
}

func TestWriteCanDisableDefaultPostprocessing(t *testing.T) {
	dir := t.TempDir()
	content := "package sample\n\nfunc add(a int,b int)int{return a+b}\n"
	dispatch, err := NewWriteTool(WriteConfig{
		CWD: dir,
		Postprocess: PostprocessConfig{
			DisableDefault: true,
		},
	}).ResolveTool(context.Background(), nil, threads.ToolCall{
		CallID:  "c1",
		Name:    "write",
		Payload: `{"path":"sample.go","content":` + jsonString(content) + `}`,
	}, nil)
	if err != nil {
		t.Fatalf("ResolveTool: %v", err)
	}
	assertWriteResult(t, dispatch, "Wrote sample.go")
	assertFile(t, filepath.Join(dir, "sample.go"), content)
}

func TestWriteCanInjectProcessorsBeforeAndAfterDefaults(t *testing.T) {
	dir := t.TempDir()
	before := FilePostprocessorFunc(func(ctx context.Context, req FilePostprocessRequest) (FilePostprocessResult, error) {
		if req.Operation != FileOperationWrite || req.Tool != "write" || req.DisplayPath != "sample.go" {
			t.Fatalf("unexpected before request %#v", req)
		}
		content := strings.ReplaceAll(string(req.Content), "__CALL__", "fmt.Println(\"hi\")")
		return FilePostprocessResult{
			Content:        []byte(content),
			ContentChanged: true,
			Report:         &FilePostprocessReport{Processor: "before", Summary: "expanded placeholder"},
		}, nil
	})
	after := FilePostprocessorFunc(func(ctx context.Context, req FilePostprocessRequest) (FilePostprocessResult, error) {
		if !strings.Contains(string(req.Content), `import "fmt"`) {
			t.Fatalf("after default did not see formatted imports:\n%s", req.Content)
		}
		content := string(req.Content) + "\n// after\n"
		return FilePostprocessResult{
			Content:        []byte(content),
			ContentChanged: true,
			Report:         &FilePostprocessReport{Processor: "after", Summary: "added footer"},
		}, nil
	})
	var builderResult WriteResult
	dispatch, err := NewWriteTool(WriteConfig{
		CWD: dir,
		Postprocess: PostprocessConfig{
			BeforeDefault: []FilePostprocessor{before},
			AfterDefault:  []FilePostprocessor{after},
		},
		ResultBuilder: func(call tool.Call, res WriteResult) tool.Item {
			builderResult = res
			return tool.ResultJSON(call, map[string]any{
				"path":        res.DisplayPath,
				"bytes":       res.Bytes,
				"postprocess": res.Postprocess,
			})
		},
	}).ResolveTool(context.Background(), nil, threads.ToolCall{
		CallID:  "c1",
		Name:    "write",
		Payload: `{"path":"sample.go","content":"package sample\n\nfunc main(){__CALL__}\n"}`,
	}, nil)
	if err != nil {
		t.Fatalf("ResolveTool: %v", err)
	}
	result := onlyResult(t, dispatch)
	if !strings.Contains(result.Output, `"path":"sample.go"`) {
		t.Fatalf("custom builder output missing path: %q", result.Output)
	}
	if got := reportProcessors(builderResult.Postprocess); !reflect.DeepEqual(got, []string{"before", "goimports", "after"}) {
		t.Fatalf("postprocess reports = %#v", got)
	}
	assertFile(t, filepath.Join(dir, "sample.go"), `package sample

import "fmt"

func main() { fmt.Println("hi") }

// after
`)
}

func TestWritePostprocessFailureLeavesExistingFileUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.go")
	mustWriteFile(t, path, "package old\n")

	dispatch, err := NewWriteTool(WriteConfig{CWD: dir}).ResolveTool(context.Background(), nil, threads.ToolCall{
		CallID:  "c1",
		Name:    "write",
		Payload: `{"path":"bad.go","content":"package bad\nfunc {"}`,
	}, nil)
	if err != nil {
		t.Fatalf("ResolveTool: %v", err)
	}
	result := onlyResult(t, dispatch)
	if !strings.Contains(result.Output, "wrote bad.go\ngo syntax:") {
		t.Fatalf("unexpected error output: %q", result.Output)
	}
	if strings.Contains(result.Output, "golangprocessor") || strings.Contains(result.Output, "bad.go:2:11") {
		t.Fatalf("error output should use concise goimports detail without repeating path: %q", result.Output)
	}
	if result.Data["error"] != result.Output {
		t.Fatalf("expected model-visible error data, got %#v", result.Data)
	}
	assertFile(t, path, "package old\n")
	if dispatch.Recovery != threads.ToolRecoveryUnsafe {
		t.Fatalf("Recovery = %q, want unsafe", dispatch.Recovery)
	}
}

func TestWriteCustomProcessorFailureLeavesExistingFileUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.txt")
	mustWriteFile(t, path, "old\n")
	boom := FilePostprocessorFunc(func(context.Context, FilePostprocessRequest) (FilePostprocessResult, error) {
		return FilePostprocessResult{}, fmt.Errorf("boom")
	})
	dispatch, err := NewWriteTool(WriteConfig{
		CWD: dir,
		Postprocess: PostprocessConfig{
			BeforeDefault: []FilePostprocessor{boom},
		},
	}).ResolveTool(context.Background(), nil, threads.ToolCall{
		CallID:  "c1",
		Name:    "write",
		Payload: `{"path":"notes.txt","content":"new\n"}`,
	}, nil)
	if err != nil {
		t.Fatalf("ResolveTool: %v", err)
	}
	result := onlyResult(t, dispatch)
	if !strings.Contains(result.Output, "wrote notes.txt\nboom") {
		t.Fatalf("unexpected output %q", result.Output)
	}
	assertFile(t, path, "old\n")
}

func TestWriteMissingPathAndDecodeErrorsAreModelVisible(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    string
	}{
		{name: "missing path", payload: `{"content":"x"}`, want: "write: path is required"},
		{name: "decode", payload: `{`, want: `tool "write" payload:`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dispatch, err := NewWriteTool(WriteConfig{CWD: t.TempDir()}).ResolveTool(context.Background(), nil, threads.ToolCall{
				CallID:  "c1",
				Name:    "write",
				Payload: tt.payload,
			}, nil)
			if err != nil {
				t.Fatalf("ResolveTool: %v", err)
			}
			result := onlyResult(t, dispatch)
			if !strings.Contains(result.Output, tt.want) {
				t.Fatalf("unexpected output %q, want to contain %q", result.Output, tt.want)
			}
			if result.Data["error"] != result.Output {
				t.Fatalf("expected model-visible error data, got %#v", result.Data)
			}
			if dispatch.Recovery != threads.ToolRecoveryUnsafe {
				t.Fatalf("Recovery = %q, want unsafe", dispatch.Recovery)
			}
		})
	}
}

func TestWriteWrongToolNameIsInfrastructureError(t *testing.T) {
	_, err := NewWriteTool(WriteConfig{}).ResolveTool(context.Background(), nil, threads.ToolCall{
		CallID: "c1",
		Name:   "read",
	}, nil)
	if err == nil {
		t.Fatal("expected missing tool infrastructure error")
	}
}

func assertWriteResult(t *testing.T, dispatch threads.ToolDispatch, want string) {
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
	if result.Data != nil {
		t.Fatalf("text result should not have data: %#v", result.Data)
	}
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	buf, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(buf) != want {
		t.Fatalf("unexpected file contents for %s:\n got: %q\nwant: %q", path, string(buf), want)
	}
}

func reportProcessors(reports []FilePostprocessReport) []string {
	processors := make([]string, 0, len(reports))
	for _, report := range reports {
		processors = append(processors, report.Processor)
	}
	return processors
}
