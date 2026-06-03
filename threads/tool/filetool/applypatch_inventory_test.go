package filetool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mackross/agentloom/threads"
)

func TestApplyPatchBehaviorInventory(t *testing.T) {
	t.Run("raw payload may contain explanatory text around the patch envelope", func(t *testing.T) {
		dir := t.TempDir()
		mustWriteFile(t, filepath.Join(dir, "notes.txt"), "alpha\n")

		resolveApplyPatch(t, NewApplyPatchTool(ApplyPatchConfig{CWD: dir}), `Here is the patch:

*** Begin Patch
*** Update File: notes.txt
@@
-alpha
+ALPHA
*** End Patch

Done.`)

		assertFile(t, filepath.Join(dir, "notes.txt"), "ALPHA\n")
	})

	t.Run("update with move and no chunks is a pure move", func(t *testing.T) {
		dir := t.TempDir()
		mustWriteFile(t, filepath.Join(dir, "old.txt"), "same\n")

		result := resolveApplyPatch(t, NewApplyPatchTool(ApplyPatchConfig{CWD: dir}), `*** Begin Patch
*** Update File: old.txt
*** Move to: new.txt
*** End Patch`)

		if result.Output != "Success. Updated the following files:\nM new.txt" {
			t.Fatalf("unexpected output: %q", result.Output)
		}
		assertFile(t, filepath.Join(dir, "new.txt"), "same\n")
		if _, err := os.Stat(filepath.Join(dir, "old.txt")); !os.IsNotExist(err) {
			t.Fatalf("old.txt still exists or stat failed with non-not-exist error: %v", err)
		}
	})

	t.Run("end-of-file marker inserts after the last line", func(t *testing.T) {
		dir := t.TempDir()
		mustWriteFile(t, filepath.Join(dir, "notes.txt"), "alpha\n")

		resolveApplyPatch(t, NewApplyPatchTool(ApplyPatchConfig{CWD: dir}), `*** Begin Patch
*** Update File: notes.txt
@@
+omega
*** End of File
*** End Patch`)

		assertFile(t, filepath.Join(dir, "notes.txt"), "alpha\nomega\n")
	})

	t.Run("matching tolerates trailing spaces, indentation, and unicode punctuation", func(t *testing.T) {
		dir := t.TempDir()
		mustWriteFile(t, filepath.Join(dir, "notes.txt"), "say “hello”\ntrail   \n  spaced\n")

		resolveApplyPatch(t, NewApplyPatchTool(ApplyPatchConfig{CWD: dir}), `*** Begin Patch
*** Update File: notes.txt
@@
-say "hello"
+say "HELLO"
@@
-trail
+trimmed
@@
-spaced
+SPACED
*** End Patch`)

		assertFile(t, filepath.Join(dir, "notes.txt"), "say \"HELLO\"\ntrimmed\nSPACED\n")
	})

	t.Run("duplicate targets are rejected before any file is written", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "notes.txt")
		mustWriteFile(t, path, "alpha\n")

		result := resolveApplyPatch(t, NewApplyPatchTool(ApplyPatchConfig{CWD: dir}), `*** Begin Patch
*** Update File: notes.txt
@@
-alpha
+ALPHA
*** Update File: notes.txt
@@
-alpha
+BETA
*** End Patch`)

		if !strings.Contains(result.Output, "apply_patch verification failed: duplicate patch target notes.txt") {
			t.Fatalf("unexpected output: %q", result.Output)
		}
		if result.Data["error"] != result.Output {
			t.Fatalf("expected model-visible error data, got %#v", result.Data)
		}
		assertFile(t, path, "alpha\n")
	})

	t.Run("missing update path is model-visible and uses the model path", func(t *testing.T) {
		dir := t.TempDir()

		result := resolveApplyPatch(t, NewApplyPatchTool(ApplyPatchConfig{CWD: dir}), `*** Begin Patch
*** Update File: missing.txt
@@
-old
+new
*** End Patch`)

		if !strings.Contains(result.Output, "apply_patch verification failed: open missing.txt: no such file or directory") {
			t.Fatalf("unexpected output: %q", result.Output)
		}
		if result.Data["error"] != result.Output {
			t.Fatalf("expected model-visible error data, got %#v", result.Data)
		}
	})

	t.Run("missing delete path is model-visible and uses the model path", func(t *testing.T) {
		dir := t.TempDir()

		result := resolveApplyPatch(t, NewApplyPatchTool(ApplyPatchConfig{CWD: dir}), `*** Begin Patch
*** Delete File: missing.txt
*** End Patch`)

		if !strings.Contains(result.Output, "apply_patch verification failed: open missing.txt: no such file or directory") {
			t.Fatalf("unexpected output: %q", result.Output)
		}
		if result.Data["error"] != result.Output {
			t.Fatalf("expected model-visible error data, got %#v", result.Data)
		}
	})

	t.Run("add refuses to overwrite existing files", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "existing.txt")
		mustWriteFile(t, path, "old\n")

		result := resolveApplyPatch(t, NewApplyPatchTool(ApplyPatchConfig{CWD: dir}), `*** Begin Patch
*** Add File: existing.txt
+new
*** End Patch`)

		if !strings.Contains(result.Output, "apply_patch verification failed: file already exists: existing.txt") {
			t.Fatalf("unexpected output: %q", result.Output)
		}
		assertFile(t, path, "old\n")
	})

	t.Run("json mode requires patchText", func(t *testing.T) {
		dispatch, err := NewApplyPatchTool(ApplyPatchConfig{Mode: ApplyPatchModeJSON}).ResolveTool(context.Background(), nil, threads.ToolCall{
			CallID:  "c1",
			Name:    "apply_patch",
			Payload: `{}`,
		}, nil)
		if err != nil {
			t.Fatalf("ResolveTool: %v", err)
		}
		if dispatch.Recovery != threads.ToolRecoveryUnsafe {
			t.Fatalf("Recovery = %q, want unsafe", dispatch.Recovery)
		}
		result := onlyResult(t, dispatch)
		if result.Output != "patchText or raw patch payload is required" {
			t.Fatalf("unexpected output: %q", result.Output)
		}
		if result.Data["error"] != result.Output {
			t.Fatalf("expected model-visible error data, got %#v", result.Data)
		}
	})
}
