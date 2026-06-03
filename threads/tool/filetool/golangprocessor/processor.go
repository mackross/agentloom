package golangprocessor

import (
	"bytes"
	"context"
	"fmt"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"strconv"

	"golang.org/x/tools/imports"

	"github.com/mackross/agentloom/threads/tool/filetool/fileprocess"
)

// Config configures the Go processor.
type Config struct {
	// Match overrides the default matcher. If nil, paths ending in .go match.
	Match func(fileprocess.Request) bool

	// Options are passed to imports.Process.
	Options *imports.Options

	// ProcessPureMoves controls whether pure patch moves are reformatted. Default
	// false preserves pure move semantics.
	ProcessPureMoves bool

	// ReportUnchanged controls whether a successful no-op processing pass emits a
	// report. Default false keeps default output quiet.
	ReportUnchanged bool
}

// New returns a Go imports/formatting processor configured by cfg.
func New(cfg Config) fileprocess.Processor {
	return processor{cfg: cfg}
}

// Default returns the default Go imports/formatting processor.
func Default() fileprocess.Processor {
	return New(Config{})
}

type processor struct {
	cfg Config
}

// ProcessFile implements fileprocess.Processor.
func (p processor) ProcessFile(ctx context.Context, req fileprocess.Request) (fileprocess.Result, error) {
	select {
	case <-ctx.Done():
		return fileprocess.Result{}, ctx.Err()
	default:
	}

	if req.PureMove && !p.cfg.ProcessPureMoves {
		return fileprocess.Result{}, nil
	}
	match := p.cfg.Match
	if match == nil {
		match = defaultMatch
	}
	if !match(req) {
		return fileprocess.Result{}, nil
	}

	formatted, err := imports.Process(req.Path, req.Content, p.cfg.Options)
	if err != nil {
		return fileprocess.Result{}, fmt.Errorf("go syntax: %s", displayError(err, req))
	}
	changed := !bytes.Equal(formatted, req.Content)
	if !changed && !p.cfg.ReportUnchanged {
		return fileprocess.Result{}, nil
	}

	res := fileprocess.Result{
		Report: &fileprocess.Report{
			Processor:   "goimports",
			Operation:   req.Operation,
			Path:        req.Path,
			DisplayPath: req.DisplayPath,
		},
	}
	if changed {
		res.Content = formatted
		res.ContentChanged = true
		res.Report.Summary = "formatted Go and updated imports"
		added, removed := importChanges(req.Path, req.Content, formatted)
		formatLines := formattedLineRanges(req.Path, req.Content, formatted)
		if len(added) > 0 || len(removed) > 0 {
			res.Report.Data = map[string]any{
				"imports": map[string]any{
					"added":   added,
					"removed": removed,
				},
				"importDiff": importDiffLines(added, removed),
			}
		}
		if formatLines != "" {
			if res.Report.Data == nil {
				res.Report.Data = map[string]any{}
			}
			res.Report.Data["formattedLines"] = formatLines
		}
	} else {
		res.Report.Summary = "Go formatting and imports already up to date"
	}
	return res, nil
}

func defaultMatch(req fileprocess.Request) bool {
	return strings.EqualFold(filepath.Ext(req.Path), ".go")
}

func displayError(err error, req fileprocess.Request) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	for _, prefix := range []string{req.Path, req.DisplayPath} {
		if prefix == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(msg, prefix+":"); ok {
			return strings.TrimPrefix(rest, ":")
		}
	}
	if req.Path != "" {
		if rest, ok := strings.CutPrefix(msg, filepath.Base(req.Path)+":"); ok {
			return rest
		}
	}
	return msg
}

func importChanges(path string, before, after []byte) ([]string, []string) {
	beforeImports, err := importSet(path, before)
	if err != nil {
		return nil, nil
	}
	afterImports, err := importSet(path, after)
	if err != nil {
		return nil, nil
	}
	added := setDifference(afterImports, beforeImports)
	removed := setDifference(beforeImports, afterImports)
	return added, removed
}

func importSet(path string, content []byte) (map[string]struct{}, error) {
	file, err := parser.ParseFile(token.NewFileSet(), path, content, parser.ImportsOnly)
	if err != nil {
		return nil, err
	}
	imports := make(map[string]struct{}, len(file.Imports))
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		imports[path] = struct{}{}
	}
	return imports, nil
}

func setDifference(a, b map[string]struct{}) []string {
	diff := make([]string, 0)
	for value := range a {
		if _, ok := b[value]; !ok {
			diff = append(diff, value)
		}
	}
	sort.Strings(diff)
	return diff
}

func importDiffLines(added, removed []string) []string {
	lines := make([]string, 0, len(added)+len(removed))
	for _, path := range added {
		lines = append(lines, "+ "+path)
	}
	for _, path := range removed {
		lines = append(lines, "- "+path)
	}
	return lines
}

func formattedLineRanges(path string, before, after []byte) string {
	ignored, err := importLines(path, after)
	if err != nil {
		ignored = nil
	}
	lines := changedNewLines(before, after)
	filtered := lines[:0]
	for _, line := range lines {
		if _, ok := ignored[line]; !ok {
			filtered = append(filtered, line)
		}
	}
	return compactLineRanges(filtered)
}

func importLines(path string, content []byte) (map[int]struct{}, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, content, parser.ImportsOnly)
	if err != nil {
		return nil, err
	}
	lines := make(map[int]struct{})
	for _, spec := range file.Imports {
		start := fset.Position(spec.Pos()).Line
		end := fset.Position(spec.End()).Line
		if start > 1 {
			lines[start-1] = struct{}{}
		}
		for line := start; line <= end; line++ {
			lines[line] = struct{}{}
		}
		lines[end+1] = struct{}{}
	}
	return lines, nil
}

func changedNewLines(before, after []byte) []int {
	oldLines := splitLines(before)
	newLines := splitLines(after)
	if len(oldLines) == 0 && len(newLines) == 0 {
		return nil
	}
	if len(oldLines)*len(newLines) > 4_000_000 {
		lines := make([]int, 0, len(newLines))
		for i := range newLines {
			lines = append(lines, i+1)
		}
		return lines
	}
	dp := make([][]int, len(oldLines)+1)
	for i := range dp {
		dp[i] = make([]int, len(newLines)+1)
	}
	for i := len(oldLines) - 1; i >= 0; i-- {
		for j := len(newLines) - 1; j >= 0; j-- {
			if oldLines[i] == newLines[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	matched := make([]bool, len(newLines))
	for i, j := 0, 0; i < len(oldLines) && j < len(newLines); {
		if oldLines[i] == newLines[j] {
			matched[j] = true
			i++
			j++
		} else if dp[i+1][j] >= dp[i][j+1] {
			i++
		} else {
			j++
		}
	}
	changed := make([]int, 0)
	for i, ok := range matched {
		if !ok {
			changed = append(changed, i+1)
		}
	}
	return changed
}

func splitLines(content []byte) []string {
	normalized := strings.ReplaceAll(string(content), "\r\n", "\n")
	normalized = strings.TrimSuffix(normalized, "\n")
	if normalized == "" {
		return nil
	}
	return strings.Split(normalized, "\n")
}

func compactLineRanges(lines []int) string {
	if len(lines) == 0 {
		return ""
	}
	sort.Ints(lines)
	ranges := make([]string, 0)
	start, prev := lines[0], lines[0]
	for _, line := range lines[1:] {
		if line == prev || line == prev+1 {
			prev = line
			continue
		}
		ranges = append(ranges, formatRange(start, prev))
		start, prev = line, line
	}
	ranges = append(ranges, formatRange(start, prev))
	return strings.Join(ranges, ",")
}

func formatRange(start, end int) string {
	if start == end {
		return strconv.Itoa(start)
	}
	return strconv.Itoa(start) + "-" + strconv.Itoa(end)
}
