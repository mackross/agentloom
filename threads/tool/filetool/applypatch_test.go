package filetool

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	gschema "github.com/google/jsonschema-go/jsonschema"

	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/tool"
)

func TestAddApplyPatchSnapshotSpecDefaultsToLark(t *testing.T) {
	cat := tool.NewCatalog()
	if got := AddApplyPatch(cat, ApplyPatchConfig{}); got != cat {
		t.Fatalf("AddApplyPatch returned a different catalog")
	}
	snap := cat.Snapshot()
	if len(snap.Offered) != 1 {
		t.Fatalf("expected one offered tool, got %#v", snap.Offered)
	}
	spec := snap.Offered[0]
	if spec.Name != "apply_patch" {
		t.Fatalf("unexpected tool name: %q", spec.Name)
	}
	if !strings.Contains(spec.Description, "stripped-down, file-oriented diff format") {
		t.Fatalf("unexpected description: %q", spec.Description)
	}
	if payload, ok := spec.Payload.(tool.PayloadLark); !ok || !strings.Contains(string(payload), `begin_patch: "*** Begin Patch" LF`) {
		t.Fatalf("expected lark payload, got %#v", spec.Payload)
	}
	if want := []threads.ToolHandlerBinding{{Name: "apply_patch"}}; !reflect.DeepEqual(cat.ToolsSnapshot(nil).Handlers, want) {
		t.Fatalf("unexpected handler bindings: %#v", cat.ToolsSnapshot(nil).Handlers)
	}
}

func TestAddApplyPatchJSONModeSnapshotSpec(t *testing.T) {
	cat := AddApplyPatch(tool.NewCatalog(), ApplyPatchConfig{Mode: ApplyPatchModeJSON})
	spec := cat.Snapshot().Offered[0]
	if _, ok := spec.Payload.(tool.PayloadJSONSchema); !ok {
		t.Fatalf("expected JSON schema payload, got %T", spec.Payload)
	}
}

func TestApplyPatchConfigDescriptionOverrides(t *testing.T) {
	toolDescription := "custom patch"
	patchTextDescription := "custom patch text"
	cat := AddApplyPatch(tool.NewCatalog(), ApplyPatchConfig{
		ToolDescription:      &toolDescription,
		PatchTextDescription: &patchTextDescription,
		Mode:                 ApplyPatchModeJSON,
	})
	spec := cat.Snapshot().Offered[0]
	if spec.Description != toolDescription {
		t.Fatalf("tool description = %q, want %q", spec.Description, toolDescription)
	}
	schema := gschema.Schema(spec.Payload.(tool.PayloadJSONSchema))
	if schema.Properties["patchText"].Description != patchTextDescription {
		t.Fatalf("unexpected property descriptions: %#v", schema.Properties)
	}
}

func TestNewApplyPatchToolDirectProviderResolver(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "notes.txt"), "alpha\n")
	patchTool := NewApplyPatchTool(ApplyPatchConfig{CWD: dir})
	var _ threads.ToolProvider = patchTool
	var _ threads.ToolResolver = patchTool

	dispatch, err := patchTool.ResolveTool(context.Background(), nil, threads.ToolCall{
		CallID:  "c1",
		Name:    "apply_patch",
		Payload: `*** Begin Patch
*** Update File: notes.txt
@@
-alpha
+ALPHA
*** End Patch`,
	}, nil)
	if err != nil {
		t.Fatalf("ResolveTool: %v", err)
	}
	result := assertApplyPatchResult(t, dispatch, "Success. Updated the following files:\nM notes.txt")
	assertFile(t, filepath.Join(dir, "notes.txt"), "ALPHA\n")
	if result.Data["diff"] == nil {
		t.Fatalf("expected diff data, got %#v", result.Data)
	}
}

func TestApplyPatchJSONPayload(t *testing.T) {
	dir := t.TempDir()
	patchTool := NewApplyPatchTool(ApplyPatchConfig{CWD: dir, Mode: ApplyPatchModeJSON})
	dispatch, err := patchTool.ResolveTool(context.Background(), nil, threads.ToolCall{
		CallID: "c1",
		Name:   "apply_patch",
		Payload: `{"patchText":"*** Begin Patch\n*** Add File: notes.txt\n+hello\n*** End Patch"}`,
	}, nil)
	if err != nil {
		t.Fatalf("ResolveTool: %v", err)
	}
	assertApplyPatchResult(t, dispatch, "Success. Updated the following files:\nA notes.txt")
	assertFile(t, filepath.Join(dir, "notes.txt"), "hello\n")
}

func TestApplyPatchAddGoAppliesDefaultPostprocessing(t *testing.T) {
	dir := t.TempDir()
	result := resolveApplyPatch(t, NewApplyPatchTool(ApplyPatchConfig{CWD: dir}), `*** Begin Patch
*** Add File: main.go
+package sample
+
+func main(){fmt.Println("hi")}
*** End Patch`)
	if result.Output != "Success. Updated the following files:\nA main.go\n\tImports:\n\t+ fmt\n\tgofmt: updated 5 lines" {
		t.Fatalf("unexpected output: %q", result.Output)
	}
	want := `package sample

import "fmt"

func main() { fmt.Println("hi") }
`
	assertFile(t, filepath.Join(dir, "main.go"), want)
	diff := result.Data["diff"].(string)
	for _, want := range []string{`+import "fmt"`, `+func main() { fmt.Println("hi") }`} {
		if !strings.Contains(diff, want) {
			t.Fatalf("postprocessed diff missing %q:\n%s", want, diff)
		}
	}
	if got := len(result.Data["postprocess"].([]FilePostprocessReport)); got != 1 {
		t.Fatalf("postprocess reports = %d, want 1", got)
	}
}

func TestApplyPatchUpdateGoAppliesDefaultPostprocessingAndFinalDiff(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "main.go"), "package sample\n\nfunc main() { println(\"old\") }\n")
	result := resolveApplyPatch(t, NewApplyPatchTool(ApplyPatchConfig{CWD: dir}), `*** Begin Patch
*** Update File: main.go
@@
-func main() { println("old") }
+func main(){fmt.Println("hi")}
*** End Patch`)
	assertFile(t, filepath.Join(dir, "main.go"), `package sample

import "fmt"

func main() { fmt.Println("hi") }
`)
	diff := result.Data["diff"].(string)
	for _, want := range []string{`+import "fmt"`, `+func main() { fmt.Println("hi") }`} {
		if !strings.Contains(diff, want) {
			t.Fatalf("postprocessed diff missing %q:\n%s", want, diff)
		}
	}
}

func TestApplyPatchCanDisableDefaultPostprocessing(t *testing.T) {
	dir := t.TempDir()
	resolveApplyPatch(t, NewApplyPatchTool(ApplyPatchConfig{
		CWD: dir,
		Postprocess: PostprocessConfig{
			DisableDefault: true,
		},
	}), `*** Begin Patch
*** Add File: main.go
+package sample
+
+func add(a int,b int)int{return a+b}
*** End Patch`)
	assertFile(t, filepath.Join(dir, "main.go"), "package sample\n\nfunc add(a int,b int)int{return a+b}\n")
}

func TestApplyPatchCustomResultBuilderReceivesPostprocessReports(t *testing.T) {
	dir := t.TempDir()
	var builderResult ApplyPatchResult
	patchTool := NewApplyPatchTool(ApplyPatchConfig{
		CWD: dir,
		ResultBuilder: func(call tool.Call, res ApplyPatchResult) tool.Item {
			builderResult = res
			return tool.ResultJSON(call, map[string]any{
				"changes":     len(res.Changes),
				"postprocess": res.Postprocess,
			})
		},
	})
	result := resolveApplyPatch(t, patchTool, `*** Begin Patch
*** Add File: main.go
+package sample
+
+func main(){fmt.Println("hi")}
*** End Patch`)
	if !strings.Contains(result.Output, `"changes":1`) {
		t.Fatalf("unexpected custom output: %q", result.Output)
	}
	if got := reportProcessors(builderResult.Postprocess); !reflect.DeepEqual(got, []string{"goimports"}) {
		t.Fatalf("builder reports = %#v", got)
	}
	if len(builderResult.Changes) != 1 || len(builderResult.Changes[0].Postprocess) != 1 {
		t.Fatalf("builder change result missing reports: %#v", builderResult)
	}
}

func TestApplyPatchSyntaxErrorIsModelVisibleAndPatchStillApplies(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "keep.txt"), "keep\n")
	result := resolveApplyPatch(t, NewApplyPatchTool(ApplyPatchConfig{CWD: dir}), `*** Begin Patch
*** Update File: keep.txt
@@
-keep
+changed
*** Add File: bad.go
+package bad
+func main {
*** End Patch`)
	if !strings.Contains(result.Output, "A bad.go\n\tgo syntax: 2:11: expected '(', found '{'") {
		t.Fatalf("unexpected syntax error output: %q", result.Output)
	}
	if strings.Contains(result.Output, "golangprocessor") || strings.Contains(result.Output, "bad.go:2:11") {
		t.Fatalf("syntax error should not expose internal package name or repeat path: %q", result.Output)
	}
	assertFile(t, filepath.Join(dir, "keep.txt"), "changed\n")
	assertFile(t, filepath.Join(dir, "bad.go"), "package bad\nfunc main {\n")
}

func TestApplyPatchPostprocessFailureIsModelVisibleAndPatchStillApplies(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "notes.txt"), "old\n")
	patchTool := NewApplyPatchTool(ApplyPatchConfig{
		CWD: dir,
		Postprocess: PostprocessConfig{
			BeforeDefault: []FilePostprocessor{
				FilePostprocessorFunc(func(context.Context, FilePostprocessRequest) (FilePostprocessResult, error) {
					return FilePostprocessResult{}, errors.New("lint failed")
				}),
			},
			DisableDefault: true,
		},
	})
	result := resolveApplyPatch(t, patchTool, `*** Begin Patch
*** Update File: notes.txt
@@
-old
+new
*** Add File: other.txt
+other
*** End Patch`)
	want := "Success. Updated the following files:\nM notes.txt\n\tlint failed\nA other.txt\n\tlint failed"
	if result.Output != want {
		t.Fatalf("unexpected output:\n got: %q\nwant: %q", result.Output, want)
	}
	if result.Data["error"] != nil {
		t.Fatalf("postprocess failure should not be a tool error: %#v", result.Data)
	}
	assertFile(t, filepath.Join(dir, "notes.txt"), "new\n")
	assertFile(t, filepath.Join(dir, "other.txt"), "other\n")
}

func TestApplyPatchPureMoveGoIsNotModifiedByDefault(t *testing.T) {
	dir := t.TempDir()
	unformatted := "package sample\n\nfunc add(a int,b int)int{return a+b}\n"
	mustWriteFile(t, filepath.Join(dir, "old.go"), unformatted)
	resolveApplyPatch(t, NewApplyPatchTool(ApplyPatchConfig{CWD: dir}), `*** Begin Patch
*** Update File: old.go
*** Move to: moved.go
*** End Patch`)
	assertFile(t, filepath.Join(dir, "moved.go"), unformatted)
	if _, err := os.Stat(filepath.Join(dir, "old.go")); !os.IsNotExist(err) {
		t.Fatalf("old.go still exists or stat failed with non-not-exist error: %v", err)
	}
}

func TestParsePatchAddUpdateDeleteMove(t *testing.T) {
	hunks, err := parsePatch(`*** Begin Patch
*** Add File: hello.txt
+Hello
*** Update File: old.txt
*** Move to: new.txt
@@ greet
-old
+new
*** Delete File: gone.txt
*** End Patch`)
	if err != nil {
		t.Fatal(err)
	}
	if len(hunks) != 3 {
		t.Fatalf("got %d hunks", len(hunks))
	}
	if h := hunks[0].(addHunk); h.path != "hello.txt" || h.content != "Hello\n" {
		t.Fatalf("bad add %#v", h)
	}
	if h := hunks[1].(updateHunk); h.path != "old.txt" || h.movePath != "new.txt" || len(h.chunks) != 1 || h.chunks[0].changeContext != "greet" {
		t.Fatalf("bad update %#v", h)
	}
	if h := hunks[2].(deleteHunk); h.path != "gone.txt" {
		t.Fatalf("bad delete %#v", h)
	}
}

func TestParsePatchFailures(t *testing.T) {
	tests := []string{
		"",
		"*** Begin Patch\n*** Add File: x\n+x",
		"*** Begin Patch\n*** End Patch",
		"*** Begin Patch\n*** Add File: x\nnope\n*** End Patch",
	}
	for _, text := range tests {
		t.Run(strings.ReplaceAll(text, "\n", `\n`), func(t *testing.T) {
			if hunks, err := parsePatch(text); err == nil && len(hunks) > 0 {
				t.Fatalf("expected failure/empty for %q, got %#v", text, hunks)
			}
		})
	}
}

func TestApplyPatchDiffShowsLimitedContext(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "notes.txt"), "one\ntwo\nthree\nfour\nfive\nsix\nseven\neight\nnine\n")
	res := resolveApplyPatch(t, NewApplyPatchTool(ApplyPatchConfig{CWD: dir}), `*** Begin Patch
*** Update File: notes.txt
@@
-five
+FIVE
*** End Patch`)
	diff, ok := res.Data["diff"].(string)
	if !ok {
		t.Fatalf("expected string diff, got %#v", res.Data["diff"])
	}
	for _, want := range []string{" two", " three", " four", "-five", "+FIVE", " six", " seven", " eight"} {
		if !strings.Contains(diff, want) {
			t.Fatalf("diff missing %q:\n%s", want, diff)
		}
	}
	for _, dontWant := range []string{" one", " nine"} {
		if strings.Contains(diff, dontWant) {
			t.Fatalf("diff included too much context %q:\n%s", dontWant, diff)
		}
	}
	assertApplyPatchGolden(t, "update_diff.txt", diff)
}

func TestApplyPatchValidationFailureDoesNotWrite(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "notes.txt"), "alpha\n")
	result := resolveApplyPatch(t, NewApplyPatchTool(ApplyPatchConfig{CWD: dir}), `*** Begin Patch
*** Update File: notes.txt
@@
-missing
+new
*** End Patch`)
	assertFile(t, filepath.Join(dir, "notes.txt"), "alpha\n")
	if result.Data["error"] == nil || !strings.Contains(result.Output, "apply_patch verification failed") {
		t.Fatalf("bad error %#v %q", result.Data, result.Output)
	}
	assertApplyPatchGolden(t, "missing_context_error.txt", result.Output)
}

func TestApplyPatchAllowsRelativePathsOutsideCWD(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "cwd")
	outside := filepath.Join(parent, "outside.txt")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, outside, "alpha\n")
	resolveApplyPatch(t, NewApplyPatchTool(ApplyPatchConfig{CWD: dir}), `*** Begin Patch
*** Update File: ../outside.txt
@@
-alpha
+ALPHA
*** End Patch`)
	assertFile(t, outside, "ALPHA\n")
}

func TestApplyPatchAllowsAbsolutePaths(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	mustWriteFile(t, outside, "alpha\n")
	resolveApplyPatch(t, NewApplyPatchTool(ApplyPatchConfig{CWD: dir}), `*** Begin Patch
*** Update File: `+outside+`
@@
-alpha
+ALPHA
*** End Patch`)
	assertFile(t, outside, "ALPHA\n")
}

func TestApplyPatchAddDeleteAndMove(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "old.txt"), "old\n")
	mustWriteFile(t, filepath.Join(dir, "gone.txt"), "bye\n")
	resolveApplyPatch(t, NewApplyPatchTool(ApplyPatchConfig{CWD: dir}), `*** Begin Patch
*** Add File: added.txt
+hello
*** Update File: old.txt
*** Move to: new.txt
@@
-old
+new
*** Delete File: gone.txt
*** End Patch`)
	assertFile(t, filepath.Join(dir, "added.txt"), "hello\n")
	assertFile(t, filepath.Join(dir, "new.txt"), "new\n")
	if _, err := os.Stat(filepath.Join(dir, "old.txt")); !os.IsNotExist(err) {
		t.Fatalf("old.txt still exists or stat failed with non-not-exist error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "gone.txt")); !os.IsNotExist(err) {
		t.Fatalf("gone.txt still exists or stat failed with non-not-exist error: %v", err)
	}
}

func TestApplyPatchPreservesCRLFAndCreatesParentDirectories(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "old.txt"), "alpha\r\nbeta\r\n")
	resolveApplyPatch(t, NewApplyPatchTool(ApplyPatchConfig{CWD: dir}), `*** Begin Patch
*** Add File: nested/dir/created.txt
+hello
*** Update File: old.txt
*** Move to: moved/new.txt
@@
-beta
+BETA
*** End Patch`)
	assertFile(t, filepath.Join(dir, "nested", "dir", "created.txt"), "hello\n")
	assertFile(t, filepath.Join(dir, "moved", "new.txt"), "alpha\r\nBETA\r\n")
	if _, err := os.Stat(filepath.Join(dir, "old.txt")); !os.IsNotExist(err) {
		t.Fatalf("old.txt still exists or stat failed with non-not-exist error: %v", err)
	}
}

func TestApplyPatchRollbackOnMidApplyFailure(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.txt")
	second := filepath.Join(dir, "second.txt")
	mustWriteFile(t, first, "one\n")
	mustWriteFile(t, second, "two\n")
	changes, err := planPatch(dir, mustParsePatch(t, `*** Begin Patch
*** Update File: first.txt
@@
-one
+ONE
*** Update File: second.txt
@@
-two
+TWO
*** End Patch`))
	if err != nil {
		t.Fatalf("planPatch: %v", err)
	}
	err = applyPatchWith(changes, patchFileOps{
		mkdirAll: os.MkdirAll,
		remove:   os.Remove,
		writeFile: func(path string, content []byte, mode os.FileMode) error {
			if path == second {
				return os.ErrPermission
			}
			return os.WriteFile(path, content, mode)
		},
	})
	if err == nil {
		t.Fatal("expected apply error")
	}
	assertFile(t, first, "one\n")
	assertFile(t, second, "two\n")
}

func TestApplyPatchRollbackCoversPartiallyAppliedMove(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "old.txt")
	movedPath := filepath.Join(dir, "moved.txt")
	mustWriteFile(t, oldPath, "old\n")
	fail := os.ErrPermission
	err := applyPatchWith([]fileChange{{
		path:       oldPath,
		movePath:   movedPath,
		relative:   "old.txt",
		moveRel:    "moved.txt",
		typ:        "move",
		oldContent: "old\n",
		newContent: "new\n",
	}}, patchFileOps{
		mkdirAll: os.MkdirAll,
		writeFile: func(path string, data []byte, mode os.FileMode) error {
			return os.WriteFile(path, data, mode)
		},
		remove: func(path string) error {
			if path == oldPath {
				return fail
			}
			return os.Remove(path)
		},
	})
	if err == nil {
		t.Fatal("expected move remove failure")
	}
	assertFile(t, oldPath, "old\n")
	if _, err := os.Stat(movedPath); !os.IsNotExist(err) {
		t.Fatalf("moved target exists or stat failed with non-not-exist error: %v", err)
	}
}

func TestApplyPatchWrongToolNameIsInfrastructureError(t *testing.T) {
	_, err := NewApplyPatchTool(ApplyPatchConfig{}).ResolveTool(context.Background(), nil, threads.ToolCall{
		CallID: "c1",
		Name:   "read",
	}, nil)
	if err == nil {
		t.Fatal("expected missing tool infrastructure error")
	}
}

func resolveApplyPatch(t *testing.T, patchTool *ApplyPatchTool, payload string) threads.ToolCallResult {
	t.Helper()
	dispatch, err := patchTool.ResolveTool(context.Background(), nil, threads.ToolCall{
		CallID:  "c1",
		Name:    "apply_patch",
		Payload: payload,
	}, nil)
	if err != nil {
		t.Fatalf("ResolveTool: %v", err)
	}
	return onlyResult(t, dispatch)
}

func assertApplyPatchResult(t *testing.T, dispatch threads.ToolDispatch, wantOutput string) threads.ToolCallResult {
	t.Helper()
	result := onlyResult(t, dispatch)
	if !dispatch.Started {
		t.Fatalf("dispatch not started: %#v", dispatch)
	}
	if dispatch.Recovery != threads.ToolRecoveryUnsafe {
		t.Fatalf("Recovery = %q, want unsafe", dispatch.Recovery)
	}
	if result.Output != wantOutput {
		t.Fatalf("unexpected output:\n got: %q\nwant: %q", result.Output, wantOutput)
	}
	return result
}

func mustParsePatch(t *testing.T, text string) []patchHunk {
	t.Helper()
	hunks, err := parsePatch(text)
	if err != nil {
		t.Fatalf("parsePatch: %v", err)
	}
	return hunks
}

func assertApplyPatchGolden(t *testing.T, name string, got string) {
	t.Helper()
	path := filepath.Join("testdata", "applypatch", "golden", name)
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v", path, err)
	}
	wantText := string(want)
	if !strings.Contains(name, "diff") {
		wantText = strings.TrimSuffix(wantText, "\n")
	}
	if got != wantText {
		t.Fatalf("golden mismatch for %s:\n--- got ---\n%q\n--- want ---\n%q", name, got, wantText)
	}
}
