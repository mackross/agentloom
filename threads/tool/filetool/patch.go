package filetool

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

type patchHunk interface{ isPatchHunk() }

type addHunk struct {
	path    string
	content string
}

func (addHunk) isPatchHunk() {}

type deleteHunk struct {
	path string
}

func (deleteHunk) isPatchHunk() {}

type updateHunk struct {
	path     string
	movePath string
	chunks   []updateChunk
}

func (updateHunk) isPatchHunk() {}

type updateChunk struct {
	oldLines      []string
	newLines      []string
	changeContext string
	endOfFile     bool
}

// fileChange is a fully planned filesystem mutation.
//
// planPatch fills oldContent/newContent before applyPatch writes anything, so
// applyPatch can roll back already-applied changes if a later write/remove fails
// mid-patch.
type fileChange struct {
	path     string
	movePath string

	relative string
	moveRel  string
	typ      string

	oldContent string
	newContent string
	diff       string

	postprocess      []FilePostprocessReport
	postprocessError string
	pureMove         bool
}

func parsePatch(text string) ([]patchHunk, error) {
	cleaned := strings.TrimSpace(stripPatchEnvelope(text))
	if cleaned == "" {
		return nil, fmt.Errorf("invalid patch format: missing Begin/End markers")
	}
	lines := strings.Split(normalizeNewlines(cleaned), "\n")
	begin, end := -1, -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "*** Begin Patch" && begin < 0 {
			begin = i
		}
		if strings.TrimSpace(line) == "*** End Patch" {
			end = i
		}
	}
	if begin < 0 || end < 0 || begin >= end {
		return nil, fmt.Errorf("invalid patch format: missing Begin/End markers")
	}
	var hunks []patchHunk
	for i := begin + 1; i < end; {
		line := lines[i]
		switch {
		case strings.HasPrefix(line, "*** Add File:"):
			hunk, next, err := parseAddHunk(lines, i, end)
			if err != nil {
				return nil, err
			}
			hunks = append(hunks, hunk)
			i = next
		case strings.HasPrefix(line, "*** Delete File:"):
			path := strings.TrimSpace(strings.TrimPrefix(line, "*** Delete File:"))
			if path == "" {
				return nil, fmt.Errorf("missing delete file path")
			}
			hunks = append(hunks, deleteHunk{path: path})
			i++
		case strings.HasPrefix(line, "*** Update File:"):
			hunk, next, err := parseUpdateHunk(lines, i, end)
			if err != nil {
				return nil, err
			}
			hunks = append(hunks, hunk)
			i = next
		default:
			if strings.TrimSpace(line) != "" {
				return nil, fmt.Errorf("unexpected patch line: %s", line)
			}
			i++
		}
	}
	if len(hunks) == 0 {
		return nil, fmt.Errorf("no hunks found")
	}
	return hunks, nil
}

func parseAddHunk(lines []string, start, end int) (addHunk, int, error) {
	path := strings.TrimSpace(strings.TrimPrefix(lines[start], "*** Add File:"))
	if path == "" {
		return addHunk{}, start, fmt.Errorf("missing add file path")
	}
	var b strings.Builder
	i := start + 1
	if i >= end || isPatchSectionStart(lines[i]) {
		return addHunk{}, start, fmt.Errorf("add file requires at least one content line")
	}
	for ; i < end && !isPatchSectionStart(lines[i]); i++ {
		if !strings.HasPrefix(lines[i], "+") {
			return addHunk{}, start, fmt.Errorf("add file lines must start with +")
		}
		b.WriteString(lines[i][1:])
		b.WriteByte('\n')
	}
	return addHunk{path: path, content: b.String()}, i, nil
}

func parseUpdateHunk(lines []string, start, end int) (updateHunk, int, error) {
	path := strings.TrimSpace(strings.TrimPrefix(lines[start], "*** Update File:"))
	if path == "" {
		return updateHunk{}, start, fmt.Errorf("missing update file path")
	}
	i := start + 1
	var movePath string
	if i < end && strings.HasPrefix(lines[i], "*** Move to:") {
		movePath = strings.TrimSpace(strings.TrimPrefix(lines[i], "*** Move to:"))
		if movePath == "" {
			return updateHunk{}, start, fmt.Errorf("missing move path")
		}
		i++
	}
	var chunks []updateChunk
	for i < end && !isPatchSectionStart(lines[i]) {
		if strings.TrimSpace(lines[i]) == "" {
			i++
			continue
		}
		if !strings.HasPrefix(lines[i], "@@") {
			return updateHunk{}, start, fmt.Errorf("update chunks must start with @@")
		}
		ch, next, err := parseUpdateChunk(lines, i, end)
		if err != nil {
			return updateHunk{}, start, err
		}
		chunks = append(chunks, ch)
		i = next
	}
	return updateHunk{path: path, movePath: movePath, chunks: chunks}, i, nil
}

func parseUpdateChunk(lines []string, start, end int) (updateChunk, int, error) {
	ctx := strings.TrimSpace(strings.TrimPrefix(lines[start], "@@"))
	chunk := updateChunk{changeContext: ctx}
	i := start + 1
	for i < end {
		line := lines[i]
		if strings.HasPrefix(line, "@@") || isPatchSectionStart(line) {
			break
		}
		if line == "*** End of File" {
			chunk.endOfFile = true
			i++
			break
		}
		if line == "" {
			return updateChunk{}, start, fmt.Errorf("patch lines must be prefixed with space, -, or +")
		}
		switch line[0] {
		case ' ':
			chunk.oldLines = append(chunk.oldLines, line[1:])
			chunk.newLines = append(chunk.newLines, line[1:])
		case '-':
			chunk.oldLines = append(chunk.oldLines, line[1:])
		case '+':
			chunk.newLines = append(chunk.newLines, line[1:])
		default:
			return updateChunk{}, start, fmt.Errorf("patch lines must be prefixed with space, -, or +")
		}
		i++
	}
	return chunk, i, nil
}

func isPatchSectionStart(line string) bool {
	return strings.HasPrefix(line, "*** Add File:") ||
		strings.HasPrefix(line, "*** Delete File:") ||
		strings.HasPrefix(line, "*** Update File:") ||
		strings.HasPrefix(line, "*** End Patch")
}

func stripPatchEnvelope(text string) string {
	begin := strings.Index(text, "*** Begin Patch")
	end := strings.LastIndex(text, "*** End Patch")
	if begin >= 0 && end >= begin {
		return text[begin : end+len("*** End Patch")]
	}
	return text
}

func planPatch(root string, hunks []patchHunk) ([]fileChange, error) {
	if len(hunks) == 0 {
		return nil, fmt.Errorf("no hunks found")
	}
	seen := map[string]struct{}{}
	changes := make([]fileChange, 0, len(hunks))
	for _, hunk := range hunks {
		change, err := planPatchHunk(root, hunk)
		if err != nil {
			return nil, err
		}
		for _, target := range change.patchTargets() {
			if target.abs == "" {
				continue
			}
			if _, ok := seen[target.abs]; ok {
				return nil, fmt.Errorf("duplicate patch target %s", target.display)
			}
			seen[target.abs] = struct{}{}
		}
		changes = append(changes, change)
	}
	return changes, nil
}

func planPatchHunk(root string, hunk patchHunk) (fileChange, error) {
	switch hunk := hunk.(type) {
	case addHunk:
		path, err := safePatchPath(root, hunk.path)
		if err != nil {
			return fileChange{}, err
		}
		if _, err := os.Stat(path); err == nil {
			return fileChange{}, fmt.Errorf("file already exists: %s", hunk.path)
		} else if !os.IsNotExist(err) {
			return fileChange{}, err
		}
		content := ensureTrailingNewline(hunk.content)
		return fileChange{
			path:       path,
			relative:   hunk.path,
			typ:        "add",
			newContent: content,
			diff:       unifiedContext(hunk.path+" (before)", hunk.path+" (after)", "", content, 3),
		}, nil
	case deleteHunk:
		path, err := safePatchPath(root, hunk.path)
		if err != nil {
			return fileChange{}, err
		}
		old, err := readPatchFile(path, hunk.path)
		if err != nil {
			return fileChange{}, err
		}
		return fileChange{
			path:       path,
			relative:   hunk.path,
			typ:        "delete",
			oldContent: old,
			diff:       unifiedContext(hunk.path+" (before)", hunk.path+" (after)", old, "", 3),
		}, nil
	case updateHunk:
		path, err := safePatchPath(root, hunk.path)
		if err != nil {
			return fileChange{}, err
		}
		old, err := readPatchFile(path, hunk.path)
		if err != nil {
			return fileChange{}, err
		}
		next := old
		if len(hunk.chunks) > 0 {
			next, err = derivePatchContents(hunk.path, old, hunk.chunks)
			if err != nil {
				return fileChange{}, err
			}
		}
		change := fileChange{
			path:       path,
			relative:   hunk.path,
			typ:        "update",
			oldContent: old,
			newContent: next,
			diff:       unifiedContext(hunk.path+" (before)", hunk.path+" (after)", old, next, 3),
		}
		if hunk.movePath != "" {
			movePath, err := safePatchPath(root, hunk.movePath)
			if err != nil {
				return fileChange{}, err
			}
			if _, err := os.Stat(movePath); err == nil {
				return fileChange{}, fmt.Errorf("move destination already exists: %s", hunk.movePath)
			} else if !os.IsNotExist(err) {
				return fileChange{}, err
			}
			change.movePath = movePath
			change.moveRel = hunk.movePath
			change.typ = "move"
			change.pureMove = len(hunk.chunks) == 0
		}
		return change, nil
	default:
		return fileChange{}, fmt.Errorf("unsupported patch hunk %T", hunk)
	}
}

type patchTarget struct {
	abs     string
	display string
}

func (change fileChange) patchTargets() []patchTarget {
	targets := []patchTarget{{abs: change.path, display: change.relative}}
	if change.movePath != "" {
		targets = append(targets, patchTarget{abs: change.movePath, display: change.moveRel})
	}
	return targets
}

func readPatchFile(path, display string) (string, error) {
	buf, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("open %s: no such file or directory", display)
		}
		return "", fmt.Errorf("open %s: %w", display, err)
	}
	return string(buf), nil
}

func applyPatch(changes []fileChange) error {
	return applyPatchWith(changes, patchFileOps{
		writeFile: os.WriteFile,
		mkdirAll:  os.MkdirAll,
		remove:    os.Remove,
	})
}

type patchFileOps struct {
	writeFile func(string, []byte, os.FileMode) error
	mkdirAll  func(string, os.FileMode) error
	remove    func(string) error
}

func applyPatchWith(changes []fileChange, ops patchFileOps) error {
	applied := make([]fileChange, 0, len(changes))
	rollback := func() {
		for i := len(applied) - 1; i >= 0; i-- {
			_ = rollbackPatchChange(applied[i], ops)
		}
	}
	for _, change := range changes {
		if err := applyPatchChange(change, ops); err != nil {
			_ = rollbackPatchChange(change, ops)
			rollback()
			return err
		}
		applied = append(applied, change)
	}
	return nil
}

func applyPatchChange(change fileChange, ops patchFileOps) error {
	target := change.path
	if change.movePath != "" {
		target = change.movePath
	}
	switch change.typ {
	case "add", "update", "move":
		if err := ops.mkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := ops.writeFile(target, []byte(change.newContent), 0o644); err != nil {
			return err
		}
		if change.typ == "move" {
			if err := ops.remove(change.path); err != nil {
				return err
			}
		}
	case "delete":
		if err := ops.remove(change.path); err != nil {
			return err
		}
	}
	return nil
}

func rollbackPatchChange(change fileChange, ops patchFileOps) error {
	switch change.typ {
	case "add":
		return ops.remove(change.path)
	case "update":
		if err := ops.mkdirAll(filepath.Dir(change.path), 0o755); err != nil {
			return err
		}
		return ops.writeFile(change.path, []byte(change.oldContent), 0o644)
	case "delete":
		if err := ops.mkdirAll(filepath.Dir(change.path), 0o755); err != nil {
			return err
		}
		return ops.writeFile(change.path, []byte(change.oldContent), 0o644)
	case "move":
		if change.movePath != "" {
			_ = ops.remove(change.movePath)
		}
		if err := ops.mkdirAll(filepath.Dir(change.path), 0o755); err != nil {
			return err
		}
		return ops.writeFile(change.path, []byte(change.oldContent), 0o644)
	default:
		return nil
	}
}

func safePatchPath(root, rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("path is required")
	}
	if filepath.IsAbs(rel) {
		return filepath.Clean(rel), nil
	}
	if root == "" {
		root = "."
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	return filepath.Clean(filepath.Join(rootAbs, rel)), nil
}

func hunkPaths(root string, hunks []patchHunk) []string {
	paths := make([]string, 0, len(hunks)*2)
	for _, hunk := range hunks {
		switch hunk := hunk.(type) {
		case addHunk:
			if p, err := safePatchPath(root, hunk.path); err == nil {
				paths = append(paths, p)
			}
		case deleteHunk:
			if p, err := safePatchPath(root, hunk.path); err == nil {
				paths = append(paths, p)
			}
		case updateHunk:
			if p, err := safePatchPath(root, hunk.path); err == nil {
				paths = append(paths, p)
			}
			if hunk.movePath != "" {
				if p, err := safePatchPath(root, hunk.movePath); err == nil {
					paths = append(paths, p)
				}
			}
		}
	}
	if len(paths) == 0 {
		paths = append(paths, root)
	}
	return paths
}

func derivePatchContents(path, old string, chunks []updateChunk) (string, error) {
	ending := patchLineEnding(old)
	lines := strings.Split(normalizeNewlines(old), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	replacements, err := computePatchReplacements(lines, path, chunks)
	if err != nil {
		return "", err
	}
	out := append([]string(nil), lines...)
	sort.Slice(replacements, func(i, j int) bool { return replacements[i].start > replacements[j].start })
	for _, repl := range replacements {
		out = append(out[:repl.start], append(repl.newLines, out[repl.start+repl.oldLen:]...)...)
	}
	next := strings.Join(out, "\n")
	if next != "" || strings.HasSuffix(old, "\n") || len(replacements) > 0 {
		next += "\n"
	}
	if ending == "\r\n" {
		next = strings.ReplaceAll(next, "\n", "\r\n")
	}
	return next, nil
}

type patchReplacement struct {
	start    int
	oldLen   int
	newLines []string
}

// computePatchReplacements searches forward through the file for each update
// chunk. Matching intentionally mirrors forgiving apply_patch behavior: exact
// first, then trim-right, trim-space, and normalized Unicode punctuation.
func computePatchReplacements(lines []string, path string, chunks []updateChunk) ([]patchReplacement, error) {
	var replacements []patchReplacement
	index := 0
	for _, chunk := range chunks {
		if chunk.changeContext != "" {
			contextIndex := seekPatchLines(lines, []string{chunk.changeContext}, index, false)
			if contextIndex < 0 {
				return nil, fmt.Errorf("failed to find context %q in %s", chunk.changeContext, path)
			}
			index = contextIndex + 1
		}
		if len(chunk.oldLines) == 0 {
			insertAt := index
			if chunk.endOfFile {
				insertAt = len(lines)
			}
			replacements = append(replacements, patchReplacement{start: insertAt, oldLen: 0, newLines: chunk.newLines})
			index = insertAt + len(chunk.newLines)
			continue
		}
		found := seekPatchLines(lines, chunk.oldLines, index, chunk.endOfFile)
		if found < 0 {
			return nil, fmt.Errorf("failed to find expected lines in %s:\n%s", path, strings.Join(chunk.oldLines, "\n"))
		}
		replacements = append(replacements, patchReplacement{start: found, oldLen: len(chunk.oldLines), newLines: chunk.newLines})
		index = found + len(chunk.oldLines)
	}
	return replacements, nil
}

func seekPatchLines(lines, pattern []string, start int, eof bool) int {
	if len(pattern) == 0 {
		return -1
	}
	if eof {
		i := len(lines) - len(pattern)
		if i >= start && matchPatchAt(lines, pattern, i) {
			return i
		}
	}
	for i := start; i <= len(lines)-len(pattern); i++ {
		if matchPatchAt(lines, pattern, i) {
			return i
		}
	}
	return -1
}

func matchPatchAt(lines, pattern []string, index int) bool {
	normalizers := []func(string) string{
		func(s string) string { return s },
		func(s string) string { return strings.TrimRight(s, " \t") },
		strings.TrimSpace,
		normalizePatchUnicode,
	}
	for _, normalize := range normalizers {
		ok := true
		for i := range pattern {
			if normalize(lines[index+i]) != normalize(pattern[i]) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func normalizePatchUnicode(s string) string {
	s = strings.TrimSpace(s)
	replaced := strings.NewReplacer(
		"‘", "'", "’", "'",
		"“", "\"", "”", "\"",
		"–", "-", "—", "-",
		"…", "...",
		"\u00a0", " ",
	).Replace(s)
	if !utf8.ValidString(replaced) {
		return s
	}
	return replaced
}

func normalizeNewlines(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
}

func ensureTrailingNewline(s string) string {
	if s == "" || strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}

func patchLineEnding(s string) string {
	if strings.Contains(s, "\r\n") {
		return "\r\n"
	}
	return "\n"
}
