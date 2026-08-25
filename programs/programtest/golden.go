// Package programtest provides test helpers for executable programs.
package programtest

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// Golden executes setup, renders its result, and compares a documented
// invocation and the rendered result with path.
//
// setup must be a function literal passed directly to Golden. Golden extracts
// that literal from the calling Go source file, along with package-level type
// declarations it references, so the documented setup is the setup that the
// test actually executes. Its final return statement is transport to render and
// is omitted from the documented invocation.
//
// Set PROGRAMTEST_UPDATE_GOLDEN=1 to replace path with the rendered result.
func Golden[T any](t testing.TB, path string, setup func() (T, error), render func(T) string) {
	t.Helper()
	if setup == nil {
		t.Fatal("programtest.Golden requires setup")
	}
	if render == nil {
		t.Fatal("programtest.Golden requires render")
	}

	_, file, line, ok := runtime.Caller(1)
	if !ok {
		t.Fatal("programtest.Golden could not locate caller")
	}
	source, err := extractSetupSource(file, line)
	if err != nil {
		t.Fatalf("programtest.Golden extract setup: %v", err)
	}

	value, err := setup()
	if err != nil {
		t.Fatalf("programtest.Golden setup: %v", err)
	}
	got := goldenDocument(source, render(value))
	if os.Getenv("PROGRAMTEST_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("programtest.Golden update %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("programtest.Golden read %s: %v", path, err)
	}
	if got != string(want) {
		t.Fatalf("golden mismatch for %s\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}

func goldenDocument(setup, rendered string) string {
	var b strings.Builder
	b.WriteString("INVOKING SETUP\n==============\n\n```go\n")
	b.WriteString(strings.TrimSpace(setup))
	b.WriteString("\n```\n\nCONVERSATION\n============\n\n")
	b.WriteString(strings.TrimSpace(rendered))
	b.WriteByte('\n')
	return b.String()
}

type sourceType struct {
	spec *ast.TypeSpec
	pos  token.Pos
}

func extractSetupSource(filename string, callerLine int) (string, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return "", fmt.Errorf("read caller: %w", err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, data, parser.ParseComments)
	if err != nil {
		return "", fmt.Errorf("parse caller: %w", err)
	}

	var setup *ast.FuncLit
	bestSpan := int(^uint(0) >> 1)
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || calledName(call.Fun) != "Golden" || len(call.Args) < 3 {
			return true
		}
		start := fset.Position(call.Pos()).Line
		end := fset.Position(call.End()).Line
		if callerLine < start || callerLine > end {
			return true
		}
		lit, ok := call.Args[2].(*ast.FuncLit)
		if !ok {
			return true
		}
		if span := end - start; span < bestSpan {
			setup = lit
			bestSpan = span
		}
		return true
	})
	if setup == nil {
		return "", fmt.Errorf("caller line %d is not inside Golden(t, path, func() {...}, render)", callerLine)
	}

	decls := packageTypes(file)
	selected := referencedTypes(setup.Body, decls)

	var b strings.Builder
	for _, decl := range selected {
		var formatted bytes.Buffer
		gen := &ast.GenDecl{Tok: token.TYPE, Specs: []ast.Spec{decl.spec}}
		if err := format.Node(&formatted, fset, gen); err != nil {
			return "", fmt.Errorf("format type %s: %w", decl.spec.Name.Name, err)
		}
		b.Write(formatted.Bytes())
		b.WriteString("\n\n")
	}

	start := fset.Position(setup.Body.Lbrace).Offset + 1
	end := fset.Position(setup.Body.Rbrace).Offset
	if n := len(setup.Body.List); n > 0 {
		if _, ok := setup.Body.List[n-1].(*ast.ReturnStmt); ok {
			end = fset.Position(setup.Body.List[n-1].Pos()).Offset
		}
	}
	if start < 0 || end < start || end > len(data) {
		return "", fmt.Errorf("invalid setup source offsets %d:%d", start, end)
	}
	b.WriteString(dedent(string(data[start:end])))
	return strings.TrimSpace(b.String()), nil
}

func calledName(expr ast.Expr) string {
	switch x := expr.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return x.Sel.Name
	case *ast.IndexExpr:
		return calledName(x.X)
	case *ast.IndexListExpr:
		return calledName(x.X)
	default:
		return ""
	}
}

func packageTypes(file *ast.File) map[string]sourceType {
	out := map[string]sourceType{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, raw := range gen.Specs {
			spec, ok := raw.(*ast.TypeSpec)
			if !ok {
				continue
			}
			out[spec.Name.Name] = sourceType{spec: spec, pos: spec.Pos()}
		}
	}
	return out
}

func referencedTypes(root ast.Node, decls map[string]sourceType) []sourceType {
	selected := map[string]sourceType{}
	var visit func(ast.Node)
	visit = func(node ast.Node) {
		ast.Inspect(node, func(child ast.Node) bool {
			ident, ok := child.(*ast.Ident)
			if !ok {
				return true
			}
			decl, found := decls[ident.Name]
			if !found {
				return true
			}
			if _, seen := selected[ident.Name]; seen {
				return true
			}
			selected[ident.Name] = decl
			visit(decl.spec)
			return true
		})
	}
	visit(root)

	out := make([]sourceType, 0, len(selected))
	for _, decl := range selected {
		out = append(out, decl)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].pos < out[j].pos })
	return out
}

func dedent(source string) string {
	lines := strings.Split(strings.Trim(source, "\n"), "\n")
	indent := -1
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		n := 0
		for n < len(line) && (line[n] == ' ' || line[n] == '\t') {
			n++
		}
		if indent < 0 || n < indent {
			indent = n
		}
	}
	if indent <= 0 {
		return strings.Join(lines, "\n")
	}
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			lines[i] = ""
			continue
		}
		if len(line) >= indent {
			lines[i] = line[indent:]
		}
	}
	return strings.Join(lines, "\n")
}
