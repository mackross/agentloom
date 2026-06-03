package filetool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	gschema "github.com/google/jsonschema-go/jsonschema"

	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/tool"
)

const (
	editToolName           = "edit"
	editToolDescription    = "Edit a file using exact text replacement."
	editPathDescription    = "Relative or absolute file path"
	editEditsDescription   = "Exact text replacements"
	editOldTextDescription = "Exact text to replace"
	editNewTextDescription = "Replacement text"
)

// EditConfig configures the edit tool.
//
// Zero-value config runs the default postprocess pipeline, including Go
// formatting/import fixing for .go files. If verification or postprocessing
// fails, the file is left unchanged and the failure is returned as a
// model-visible tool error.
type EditConfig struct {
	ToolDescription    *string
	PathDescription    *string
	EditsDescription   *string
	OldTextDescription *string
	NewTextDescription *string

	CWD string

	Postprocess PostprocessConfig

	ResultBuilder EditResultBuilder
	// MutationQueue serializes overlapping file mutations. When nil,
	// DefaultMutationQueue is used.
	MutationQueue *MutationQueue
}

type EditResult struct {
	Path        string
	DisplayPath string

	Replacements int

	OldContent []byte
	NewContent []byte
	Diff       string

	Postprocess []FilePostprocessReport
}

type EditResultBuilder func(tool.Call, EditResult) tool.Item

type editReplacement struct {
	OldText string `json:"oldText" jsonschema:"Exact text to replace"`
	NewText string `json:"newText" jsonschema:"Replacement text"`
}

type editArgs struct {
	Path  string            `json:"path" jsonschema:"Relative or absolute file path"`
	Edits []editReplacement `json:"edits" jsonschema:"Exact text replacements"`
}

type editMatch struct{ i, start, end int }

// EditTool is the AgentLoom-native single-tool provider and resolver returned
// by NewEditTool.
type EditTool struct {
	catalog *tool.Catalog
}

var (
	_ threads.ToolProvider = (*EditTool)(nil)
	_ threads.ToolResolver = (*EditTool)(nil)
)

// AddEdit adds the edit tool to c and returns c for fluent catalog setup.
func AddEdit(c *tool.Catalog, cfg EditConfig) *tool.Catalog {
	if c == nil {
		c = tool.NewCatalog()
	}
	spec := tool.Spec{
		Name:        editToolName,
		Description: configString(cfg.ToolDescription, editToolDescription),
		Payload:     editPayload(cfg),
	}
	return c.AddFunc(spec, editHandler(cfg))
}

// NewEditTool creates a single edit tool that can be installed directly as a
// threads.ToolProvider and threads.ToolResolver.
func NewEditTool(cfg EditConfig) *EditTool {
	return &EditTool{catalog: AddEdit(tool.NewCatalog(), cfg)}
}

// ToolsSnapshot implements threads.ToolProvider.
func (e *EditTool) ToolsSnapshot(thread threads.Thread) threads.ToolsSnapshot {
	return e.catalog.ToolsSnapshot(thread)
}

// ResolveTool implements threads.ToolResolver.
func (e *EditTool) ResolveTool(ctx context.Context, thread threads.Thread, call threads.ToolCall, load json.RawMessage) (threads.ToolDispatch, error) {
	return e.catalog.ResolveTool(ctx, thread, call, load)
}

func editHandler(cfg EditConfig) tool.HandlerFunc {
	return func(ctx context.Context, _ threads.Thread, call tool.Call, ret tool.ReturnItem) (tool.Handling, error) {
		var args editArgs
		if err := call.UnmarshalJSON(&args); err != nil {
			return unsafeHandling(), ret(tool.ResultError(call, fmt.Errorf("tool %q payload: %w", call.Name, err)))
		}
		res, err := editFile(ctx, cfg, args)
		if err != nil {
			return unsafeHandling(), ret(tool.ResultError(call, err))
		}
		builder := cfg.ResultBuilder
		if builder == nil {
			builder = defaultEditResultBuilder
		}
		return unsafeHandling(), ret(builder(call, res))
	}
}

func editFile(ctx context.Context, cfg EditConfig, args editArgs) (EditResult, error) {
	modelPath := args.Path
	if modelPath == "" || len(args.Edits) == 0 {
		return EditResult{}, fmt.Errorf("edit: path and edits are required")
	}
	fullPath, err := safePatchPath(cfg.CWD, modelPath)
	if err != nil {
		return EditResult{}, fmt.Errorf("edit: %w", err)
	}
	var res EditResult
	err = mutationQueueOrDefault(cfg.MutationQueue).withLocks([]string{fullPath}, func() error {
		raw, err := os.ReadFile(fullPath)
		if err != nil {
			return fmt.Errorf("edit: read %s: %w", modelPath, err)
		}
		edited, ending, err := applyExactEdits(string(raw), args.Edits, modelPath)
		if err != nil {
			return err
		}
		content := []byte(restoreLineEndings(edited, ending))
		processed, reports, err := runPostprocess(ctx, cfg.Postprocess, FilePostprocessRequest{
			Tool:                editToolName,
			Operation:           FileOperationEdit,
			Path:                fullPath,
			DisplayPath:         modelPath,
			Content:             content,
			OldContent:          raw,
			OldContentAvailable: true,
		})
		if err != nil {
			return fmt.Errorf("edited %s\n%w", modelPath, err)
		}
		if processed.ContentChanged {
			content = processed.Content
		}
		if err := os.WriteFile(fullPath, content, 0o644); err != nil {
			return fmt.Errorf("edit: write %s: %w", modelPath, err)
		}
		res = EditResult{
			Path:         fullPath,
			DisplayPath:  modelPath,
			Replacements: len(args.Edits),
			OldContent:   raw,
			NewContent:   content,
			Diff:         unifiedContext(modelPath+" (before)", modelPath+" (after)", string(raw), string(content), 3),
			Postprocess:  reports,
		}
		return nil
	})
	if err != nil {
		return EditResult{}, err
	}
	return res, nil
}

func applyExactEdits(raw string, edits []editReplacement, path string) (string, string, error) {
	// Exact edit matching is intentionally line-ending tolerant: models usually
	// send LF snippets even when the file is CRLF. We normalize only for match
	// verification/replacement coordinates, then restore the file's original
	// dominant ending before post-processing. apply_patch does not need this
	// compatibility layer because its hunks are line-oriented and parsed by the
	// patch engine rather than matched as one opaque exact string.
	ending := lineEnding(raw)
	base := normalizeLineEndings(raw)
	matches := make([]editMatch, len(edits))
	for i, e := range edits {
		oldText, newText := normalizeLineEndings(e.OldText), normalizeLineEndings(e.NewText)
		if oldText == "" {
			return "", ending, fmt.Errorf("edits[%d].oldText must not be empty", i)
		}
		if oldText == newText {
			return "", ending, fmt.Errorf("no changes made to %s", path)
		}
		if n := strings.Count(base, oldText); n != 1 {
			return "", ending, editOccurrenceError(path, i, n)
		}
		start := strings.Index(base, oldText)
		matches[i] = editMatch{i: i, start: start, end: start + len(oldText)}
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].start < matches[j].start })
	for i := 1; i < len(matches); i++ {
		if matches[i-1].end > matches[i].start {
			return "", ending, fmt.Errorf("edits overlap in %s", path)
		}
	}
	edited := base
	for i := len(matches) - 1; i >= 0; i-- {
		m := matches[i]
		edited = edited[:m.start] + normalizeLineEndings(edits[m.i].NewText) + edited[m.end:]
	}
	return edited, ending, nil
}

func editOccurrenceError(path string, i, n int) error {
	if n == 0 {
		return fmt.Errorf("could not find edits[%d] in %s", i, path)
	}
	return fmt.Errorf("found %d occurrences of edits[%d] in %s; oldText must be unique", n, i, path)
}

func defaultEditResultBuilder(call tool.Call, res EditResult) tool.Item {
	output := fmt.Sprintf("Edited %s", res.DisplayPath)
	if lines := postprocessImportDiffLines(res.Postprocess); len(lines) > 0 {
		output += "\nImports:\n" + strings.Join(lines, "\n")
	}
	if lines := postprocessFormattedLines(res.Postprocess); lines != "" {
		output += "\ngofmt: updated " + lines + " lines"
	}
	return threads.ToolCallResult{
		CallID: call.CallID,
		Output: output,
		Data:   map[string]any{"diff": res.Diff},
	}
}

func normalizeLineEndings(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
}

func restoreLineEndings(s, ending string) string {
	if ending == "\r\n" {
		return strings.ReplaceAll(s, "\n", "\r\n")
	}
	return s
}

func lineEnding(s string) string {
	crlf, lf := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\r':
			if i+1 < len(s) && s[i+1] == '\n' {
				crlf++
				i++
			} else {
				lf++
			}
		case '\n':
			lf++
		}
	}
	if crlf > lf {
		return "\r\n"
	}
	return "\n"
}

func editPayload(cfg EditConfig) tool.Payload {
	return tool.PayloadJSONSchema(gschema.Schema{
		Type: "object",
		Properties: map[string]*gschema.Schema{
			"path": {Type: "string", Description: configString(cfg.PathDescription, editPathDescription)},
			"edits": {
				Type:        "array",
				Description: configString(cfg.EditsDescription, editEditsDescription),
				Items: &gschema.Schema{
					Type: "object",
					Properties: map[string]*gschema.Schema{
						"oldText": {Type: "string", Description: configString(cfg.OldTextDescription, editOldTextDescription)},
						"newText": {Type: "string", Description: configString(cfg.NewTextDescription, editNewTextDescription)},
					},
					PropertyOrder: []string{"oldText", "newText"},
				},
			},
		},
		PropertyOrder: []string{"path", "edits"},
	})
}
