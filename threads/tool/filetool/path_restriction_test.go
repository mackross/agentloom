package filetool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPathRestrictionsNilAllowsAnyPath(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := checkPathAllowed(dir, outside, nil); err != nil {
		t.Fatalf("checkPathAllowed with nil restrictions: %v", err)
	}
}

func TestPathRestrictionsMatchDoublestarRelativeToCWD(t *testing.T) {
	dir := t.TempDir()
	restrictions := &PathRestrictionConfig{RestrictToGlobs: []string{"src/**/*.go", "README.md"}}

	allowed := []string{
		"README.md",
		"src/main.go",
		"src/nested/deep/file.go",
		filepath.Join(dir, "src", "abs.go"),
	}
	for _, path := range allowed {
		if err := checkPathAllowed(dir, path, restrictions); err != nil {
			t.Fatalf("path %q should be allowed: %v", path, err)
		}
	}

	denied := []string{
		"src/main.txt",
		"other/main.go",
		filepath.Join(t.TempDir(), "src", "outside.go"),
	}
	for _, path := range denied {
		if err := checkPathAllowed(dir, path, restrictions); err == nil {
			t.Fatalf("path %q should be denied", path)
		}
	}
}

func TestReadFileHonorsPathRestrictions(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "allowed", "file.txt"), "ok")
	mustWriteFile(t, filepath.Join(dir, "denied", "file.txt"), "no")
	cfg := ReadConfig{CWD: dir, PathRestrictions: &PathRestrictionConfig{RestrictToGlobs: []string{"allowed/**"}}}

	got, err := readFile(cfg, readArgs{Path: "allowed/file.txt"})
	if err != nil {
		t.Fatalf("read allowed path: %v", err)
	}
	if got != "ok" {
		t.Fatalf("read allowed content = %q, want ok", got)
	}
	if _, err := readFile(cfg, readArgs{Path: "denied/file.txt"}); !isAccessDenied(err, "denied/file.txt") {
		t.Fatalf("read denied path error = %v, want access denied", err)
	}
}

func TestWriteFileHonorsPathRestrictions(t *testing.T) {
	dir := t.TempDir()
	cfg := WriteConfig{CWD: dir, PathRestrictions: &PathRestrictionConfig{RestrictToGlobs: []string{"allowed/**"}}}

	if _, err := writeFile(t.Context(), cfg, writeArgs{Path: "allowed/file.txt", Content: "ok"}); err != nil {
		t.Fatalf("write allowed path: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "allowed", "file.txt")); err != nil || string(got) != "ok" {
		t.Fatalf("allowed file content = %q, %v", got, err)
	}
	if _, err := writeFile(t.Context(), cfg, writeArgs{Path: "denied/file.txt", Content: "no"}); !isAccessDenied(err, "denied/file.txt") {
		t.Fatalf("write denied path error = %v, want access denied", err)
	}
}

func TestEditFileHonorsPathRestrictions(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "allowed", "file.txt"), "old")
	mustWriteFile(t, filepath.Join(dir, "denied", "file.txt"), "old")
	cfg := EditConfig{CWD: dir, PathRestrictions: &PathRestrictionConfig{RestrictToGlobs: []string{"allowed/**"}}}

	if _, err := editFile(t.Context(), cfg, editArgs{Path: "allowed/file.txt", Edits: []editReplacement{{OldText: "old", NewText: "new"}}}); err != nil {
		t.Fatalf("edit allowed path: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "allowed", "file.txt")); err != nil || string(got) != "new" {
		t.Fatalf("allowed file content = %q, %v", got, err)
	}
	if _, err := editFile(t.Context(), cfg, editArgs{Path: "denied/file.txt", Edits: []editReplacement{{OldText: "old", NewText: "new"}}}); !isAccessDenied(err, "denied/file.txt") {
		t.Fatalf("edit denied path error = %v, want access denied", err)
	}
}

func TestApplyPatchHonorsPathRestrictions(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "allowed", "file.txt"), "old\n")
	mustWriteFile(t, filepath.Join(dir, "denied", "file.txt"), "old\n")
	cfg := ApplyPatchConfig{CWD: dir, PathRestrictions: &PathRestrictionConfig{RestrictToGlobs: []string{"allowed/**"}}}

	allowedPatch, err := parsePatch(`*** Begin Patch
*** Update File: allowed/file.txt
@@
-old
+new
*** End Patch`)
	if err != nil {
		t.Fatalf("parse allowed patch: %v", err)
	}
	allowedChanges, err := planPatch(dir, allowedPatch)
	if err != nil {
		t.Fatalf("plan allowed patch: %v", err)
	}
	if err := checkPatchChangesAllowed(cfg.CWD, allowedChanges, cfg.PathRestrictions); err != nil {
		t.Fatalf("check allowed patch: %v", err)
	}

	deniedPatch, err := parsePatch(`*** Begin Patch
*** Update File: denied/file.txt
@@
-old
+new
*** End Patch`)
	if err != nil {
		t.Fatalf("parse denied patch: %v", err)
	}
	deniedChanges, err := planPatch(dir, deniedPatch)
	if err != nil {
		t.Fatalf("plan denied patch: %v", err)
	}
	if err := checkPatchChangesAllowed(cfg.CWD, deniedChanges, cfg.PathRestrictions); !isAccessDenied(err, "denied/file.txt") {
		t.Fatalf("check denied patch error = %v, want access denied", err)
	}
}

func isAccessDenied(err error, path string) bool {
	return err != nil && strings.Contains(err.Error(), "access denied: "+path)
}
