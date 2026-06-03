package filetool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mackross/agentloom/threads"
)

func TestWriteGoPostprocessIntegrationGoldens(t *testing.T) {
	t.Run("auto import add remove", func(t *testing.T) {
		dir := t.TempDir()
		content := `package sample

import "strings"

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
		result := onlyResult(t, dispatch)
		assertPostprocessGolden(t, "write_auto_import_add_remove_result.txt", result.Output)
		assertFileGolden(t, filepath.Join(dir, "sample", "main.go"), "write_auto_import_add_remove_file.go")
	})

	t.Run("format only", func(t *testing.T) {
		dir := t.TempDir()
		content := "package sample\n\nfunc add(a int,b int)int{return a+b}\n"
		dispatch, err := NewWriteTool(WriteConfig{CWD: dir}).ResolveTool(context.Background(), nil, threads.ToolCall{
			CallID:  "c1",
			Name:    "write",
			Payload: `{"path":"sample/format.go","content":` + jsonString(content) + `}`,
		}, nil)
		if err != nil {
			t.Fatalf("ResolveTool: %v", err)
		}
		result := onlyResult(t, dispatch)
		assertPostprocessGolden(t, "write_format_result.txt", result.Output)
		assertFileGolden(t, filepath.Join(dir, "sample", "format.go"), "write_format_file.go")
	})

	t.Run("syntax error", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "bad.go")
		mustWriteFile(t, path, "package old\n")
		dispatch, err := NewWriteTool(WriteConfig{CWD: dir}).ResolveTool(context.Background(), nil, threads.ToolCall{
			CallID:  "c1",
			Name:    "write",
			Payload: `{"path":"bad.go","content":"package bad\nfunc main {\n"}`,
		}, nil)
		if err != nil {
			t.Fatalf("ResolveTool: %v", err)
		}
		result := onlyResult(t, dispatch)
		assertPostprocessGolden(t, "write_syntax_error.txt", result.Output)
		assertFile(t, path, "package old\n")
	})
}

func TestApplyPatchGoPostprocessIntegrationGoldens(t *testing.T) {
	t.Run("auto import add remove", func(t *testing.T) {
		dir := t.TempDir()
		result := resolveApplyPatch(t, NewApplyPatchTool(ApplyPatchConfig{CWD: dir}), `*** Begin Patch
*** Add File: main.go
+package sample
+
+import "strings"
+
+func main(){fmt.Println("hi")}
*** End Patch`)
		assertPostprocessGolden(t, "patch_auto_import_add_remove_output.txt", result.Output)
		assertPostprocessGolden(t, "patch_auto_import_add_remove_diff.txt", result.Data["diff"].(string))
		assertFileGolden(t, filepath.Join(dir, "main.go"), "patch_auto_import_add_remove_file.go")
	})

	t.Run("format update", func(t *testing.T) {
		dir := t.TempDir()
		mustWriteFile(t, filepath.Join(dir, "format.go"), "package sample\n\nfunc add(a int, b int) int { return a - b }\n")
		mustWriteFile(t, filepath.Join(dir, "extra.go"), "package sample\n\nfunc sub(a int, b int) int { return a + b }\n")
		result := resolveApplyPatch(t, NewApplyPatchTool(ApplyPatchConfig{CWD: dir}), `*** Begin Patch
*** Update File: format.go
@@
-func add(a int, b int) int { return a - b }
+func add(a int,b int)int{return a+b}
*** Update File: extra.go
@@
-func sub(a int, b int) int { return a + b }
+func sub(a int,b int)int{return a-b}
*** End Patch`)
		assertPostprocessGolden(t, "patch_format_output.txt", result.Output)
		assertPostprocessGolden(t, "patch_format_diff.txt", result.Data["diff"].(string))
		assertFileGolden(t, filepath.Join(dir, "format.go"), "patch_format_file.go")
		assertFileGolden(t, filepath.Join(dir, "extra.go"), "patch_format_extra_file.go")
	})

	t.Run("syntax error", func(t *testing.T) {
		dir := t.TempDir()
		result := resolveApplyPatch(t, NewApplyPatchTool(ApplyPatchConfig{CWD: dir}), `*** Begin Patch
*** Add File: good.go
+package sample
+
+func ok(){fmt.Println("ok")}
*** Add File: bad.go
+package bad
+func main {
*** Add File: also_bad.go
+package bad
+func broken {
*** End Patch`)
		assertPostprocessGolden(t, "patch_syntax_error.txt", result.Output)
		assertFile(t, filepath.Join(dir, "good.go"), "package sample\n\nimport \"fmt\"\n\nfunc ok() { fmt.Println(\"ok\") }\n")
		assertFile(t, filepath.Join(dir, "bad.go"), "package bad\nfunc main {\n")
		assertFile(t, filepath.Join(dir, "also_bad.go"), "package bad\nfunc broken {\n")
	})
}

func assertPostprocessGolden(t *testing.T, name string, got string) {
	t.Helper()
	path := filepath.Join("testdata", "postprocess", "golang", "golden", name)
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v", path, err)
	}
	wantString := string(want)
	if !strings.Contains(name, "_diff") {
		wantString = strings.TrimSuffix(wantString, "\n")
	}
	if got != wantString {
		t.Fatalf("golden mismatch for %s:\n--- got ---\n%s\n--- want ---\n%s", name, got, wantString)
	}
}

func assertFileGolden(t *testing.T, path, golden string) {
	t.Helper()
	want, err := os.ReadFile(filepath.Join("testdata", "postprocess", "golang", "golden", golden))
	if err != nil {
		t.Fatalf("read golden %s: %v", golden, err)
	}
	assertFile(t, path, string(want))
}
