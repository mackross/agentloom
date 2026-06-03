package golangprocessor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mackross/agentloom/threads/tool/filetool/fileprocess"
)

func TestGoldenProcessing(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		golden string
	}{
		{name: "auto import add remove", input: "auto_import_add_remove.go", golden: "auto_import_add_remove.go"},
		{name: "format only", input: "format_only.go", golden: "format_only.go"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := readTestdata(t, "input", tt.input)
			want := readTestdata(t, "golden", tt.golden)
			path := filepath.Join(t.TempDir(), tt.input)
			res, err := Default().ProcessFile(context.Background(), fileprocess.Request{
				Tool:        "write",
				Operation:   fileprocess.OperationWrite,
				Path:        path,
				DisplayPath: tt.input,
				Content:     input,
			})
			if err != nil {
				t.Fatalf("ProcessFile: %v", err)
			}
			if !res.ContentChanged {
				t.Fatal("ContentChanged = false, want true")
			}
			if string(res.Content) != string(want) {
				t.Fatalf("golden mismatch for %s:\n--- got ---\n%s\n--- want ---\n%s", tt.input, res.Content, want)
			}
			if res.Report == nil || res.Report.Processor != "goimports" || res.Report.Summary != "formatted Go and updated imports" {
				t.Fatalf("unexpected report %#v", res.Report)
			}
			if res.Report.Operation != fileprocess.OperationWrite || res.Report.DisplayPath != tt.input || res.Report.Path != path {
				t.Fatalf("report did not mirror request: %#v", res.Report)
			}
			if tt.input == "auto_import_add_remove.go" {
				assertImportDiff(t, res.Report, []string{"+ fmt", "- strings"})
				assertFormattedLines(t, res.Report, "5")
			}
			if tt.input == "format_only.go" {
				assertFormattedLines(t, res.Report, "3")
				if _, ok := res.Report.Data["importDiff"]; ok {
					t.Fatalf("format-only report should not include import diff: %#v", res.Report.Data)
				}
			}
		})
	}
}

func TestSyntaxErrorGolden(t *testing.T) {
	input := readTestdata(t, "input", "syntax_error.go")
	want := string(readTestdata(t, "golden", "syntax_error.txt"))
	res, err := Default().ProcessFile(context.Background(), fileprocess.Request{
		Tool:        "write",
		Operation:   fileprocess.OperationWrite,
		Path:        filepath.Join(t.TempDir(), "bad.go"),
		DisplayPath: "bad.go",
		Content:     input,
	})
	if err == nil {
		t.Fatalf("expected syntax error, got result %#v", res)
	}
	got := normalizeProcessorError(err.Error())
	if got != want {
		t.Fatalf("syntax error golden mismatch:\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
}

func TestNonGoFilesAreUnchangedAndQuiet(t *testing.T) {
	res, err := Default().ProcessFile(context.Background(), fileprocess.Request{
		Path:        filepath.Join(t.TempDir(), "notes.txt"),
		DisplayPath: "notes.txt",
		Content:     []byte("hello"),
	})
	if err != nil {
		t.Fatalf("ProcessFile: %v", err)
	}
	if res.ContentChanged || len(res.Content) != 0 || res.Report != nil {
		t.Fatalf("unexpected result %#v", res)
	}
}

func TestPurePatchMovesAreSkippedByDefault(t *testing.T) {
	content := []byte("package sample\n\nfunc add(a int,b int)int{return a+b}\n")
	res, err := Default().ProcessFile(context.Background(), fileprocess.Request{
		Operation: fileprocess.OperationPatchMove,
		Path:      filepath.Join(t.TempDir(), "moved.go"),
		Content:   content,
		PureMove:  true,
	})
	if err != nil {
		t.Fatalf("ProcessFile: %v", err)
	}
	if res.ContentChanged || res.Report != nil {
		t.Fatalf("pure move was processed by default: %#v", res)
	}
}

func TestProcessPureMovesOptsIn(t *testing.T) {
	content := []byte("package sample\n\nfunc add(a int,b int)int{return a+b}\n")
	res, err := New(Config{ProcessPureMoves: true}).ProcessFile(context.Background(), fileprocess.Request{
		Operation: fileprocess.OperationPatchMove,
		Path:      filepath.Join(t.TempDir(), "moved.go"),
		Content:   content,
		PureMove:  true,
	})
	if err != nil {
		t.Fatalf("ProcessFile: %v", err)
	}
	want := "package sample\n\nfunc add(a int, b int) int { return a + b }\n"
	if !res.ContentChanged || string(res.Content) != want {
		t.Fatalf("unexpected pure-move processing result %#v", res)
	}
}

func TestReportUnchanged(t *testing.T) {
	content := []byte("package sample\n\nfunc add(a int, b int) int { return a + b }\n")
	res, err := New(Config{ReportUnchanged: true}).ProcessFile(context.Background(), fileprocess.Request{
		Operation:   fileprocess.OperationPatchUpdate,
		Path:        filepath.Join(t.TempDir(), "same.go"),
		DisplayPath: "same.go",
		Content:     content,
	})
	if err != nil {
		t.Fatalf("ProcessFile: %v", err)
	}
	if res.ContentChanged || len(res.Content) != 0 {
		t.Fatalf("unexpected content change %#v", res)
	}
	if res.Report == nil || res.Report.Summary != "Go formatting and imports already up to date" {
		t.Fatalf("unexpected unchanged report %#v", res.Report)
	}
}

func TestCustomMatcherAndCaseInsensitiveDefault(t *testing.T) {
	content := []byte("package sample\n\nfunc add(a int,b int)int{return a+b}\n")
	res, err := Default().ProcessFile(context.Background(), fileprocess.Request{
		Path:    filepath.Join(t.TempDir(), "sample.GO"),
		Content: content,
	})
	if err != nil {
		t.Fatalf("ProcessFile uppercase extension: %v", err)
	}
	if !res.ContentChanged {
		t.Fatal("uppercase .GO did not match")
	}

	res, err = New(Config{Match: func(fileprocess.Request) bool { return false }}).ProcessFile(context.Background(), fileprocess.Request{
		Path:    filepath.Join(t.TempDir(), "sample.go"),
		Content: content,
	})
	if err != nil {
		t.Fatalf("ProcessFile custom matcher: %v", err)
	}
	if res.ContentChanged || res.Report != nil {
		t.Fatalf("custom matcher false should skip, got %#v", res)
	}
}

func readTestdata(t *testing.T, parts ...string) []byte {
	t.Helper()
	pathParts := append([]string{"testdata"}, parts...)
	buf, err := os.ReadFile(filepath.Join(pathParts...))
	if err != nil {
		t.Fatalf("read testdata %v: %v", parts, err)
	}
	return buf
}

func normalizeProcessorError(s string) string {
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	return s
}

func assertImportDiff(t *testing.T, report *fileprocess.Report, want []string) {
	t.Helper()
	if report == nil || report.Data == nil {
		t.Fatalf("missing import diff data in report %#v", report)
	}
	got, ok := report.Data["importDiff"].([]string)
	if !ok {
		t.Fatalf("importDiff = %#v, want []string", report.Data["importDiff"])
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("import diff = %#v, want %#v", got, want)
	}
}

func assertFormattedLines(t *testing.T, report *fileprocess.Report, want string) {
	t.Helper()
	if report == nil || report.Data == nil {
		t.Fatalf("missing formattedLines data in report %#v", report)
	}
	got, ok := report.Data["formattedLines"].(string)
	if !ok {
		t.Fatalf("formattedLines = %#v, want string", report.Data["formattedLines"])
	}
	if got != want {
		t.Fatalf("formattedLines = %q, want %q", got, want)
	}
}
