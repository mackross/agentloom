package filetool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	gschema "github.com/google/jsonschema-go/jsonschema"

	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/tool"
)

const (
	applyPatchToolName = "apply_patch"
)

const applyPatchLarkGrammar = `start: begin_patch hunk+ end_patch
begin_patch: "*** Begin Patch" LF
end_patch: "*** End Patch" LF?

hunk: add_hunk | delete_hunk | update_hunk
add_hunk: "*** Add File: " filename LF add_line+
delete_hunk: "*** Delete File: " filename LF
update_hunk: "*** Update File: " filename LF change_move? change?

filename: /(.+)/
add_line: "+" /(.*)/ LF -> line

change_move: "*** Move to: " filename LF
change: (change_context | change_line)+ eof_line?
change_context: ("@@" | "@@ " /(.+)/) LF
change_line: ("+" | "-" | " ") /(.*)/ LF
eof_line: "*** End of File" LF

%import common.LF
`

const applyPatchToolDescription = `Use the apply_patch tool to edit files. The patch language is a stripped-down, file-oriented diff format:

*** Begin Patch
[ one or more file sections ]
*** End Patch

Operations:
*** Add File: <path> creates a new file; following lines must be prefixed with +.
*** Update File: <path> patches an existing file, optionally followed by *** Move to: <path>.
*** Delete File: <path> removes an existing file.
Update chunks start with @@, then lines prefixed with space, -, or +.`

// ApplyPatchMode selects the model-facing payload form for apply_patch.
type ApplyPatchMode string

const (
	// ApplyPatchModeLark exposes the Codex/Weaver patch grammar directly.
	ApplyPatchModeLark ApplyPatchMode = ""
	// ApplyPatchModeJSON wraps the patch text in a JSON object.
	ApplyPatchModeJSON ApplyPatchMode = "json"
)

// ApplyPatchConfig configures the apply_patch tool.
type ApplyPatchConfig struct {
	// ToolDescription overrides the model-visible tool description when non-nil.
	ToolDescription *string
	// PatchTextDescription overrides the JSON-mode patchText field description when non-nil.
	PatchTextDescription *string
	// CWD resolves relative paths. Absolute paths are used as provided.
	CWD string
	// Mode controls whether the model sees a Lark/freeform or JSON payload.
	Mode ApplyPatchMode
	// Postprocess configures the file-content processing pipeline.
	Postprocess PostprocessConfig
	// ResultBuilder customizes the returned tool item. When nil, the stable text
	// output remains "Success. Updated the following files:\n...".
	ResultBuilder ApplyPatchResultBuilder
	// MutationQueue serializes overlapping file mutations. When nil,
	// DefaultMutationQueue is used.
	MutationQueue *MutationQueue
}

type ApplyPatchChangeResult struct {
	Operation FileOperation

	Path        string
	DisplayPath string

	OldPath        string
	OldDisplayPath string

	Diff string

	Postprocess []FilePostprocessReport
	// PostprocessError is a non-fatal postprocess failure. apply_patch still
	// applies the requested file content for these cases so the model can inspect
	// and fix all affected files in a follow-up patch.
	PostprocessError string
}

type ApplyPatchResult struct {
	Changes []ApplyPatchChangeResult

	// Diff is the combined diff across all changed files, after postprocessing.
	Diff string

	Postprocess []FilePostprocessReport
}

type ApplyPatchResultBuilder func(tool.Call, ApplyPatchResult) tool.Item

type applyPatchArgs struct {
	PatchText string `json:"patchText" jsonschema:"The full patch text that describes all changes to be made"`
}

// ApplyPatchTool is the AgentLoom-native single-tool provider and resolver
// returned by NewApplyPatchTool.
type ApplyPatchTool struct {
	catalog *tool.Catalog
}

var (
	_ threads.ToolProvider = (*ApplyPatchTool)(nil)
	_ threads.ToolResolver = (*ApplyPatchTool)(nil)
)

// AddApplyPatch adds the apply_patch tool to c and returns c for fluent catalog
// setup.
func AddApplyPatch(c *tool.Catalog, cfg ApplyPatchConfig) *tool.Catalog {
	if c == nil {
		c = tool.NewCatalog()
	}
	spec := tool.Spec{
		Name:        applyPatchToolName,
		Description: configString(cfg.ToolDescription, applyPatchToolDescription),
		Payload:     applyPatchPayload(cfg),
	}
	return c.AddFunc(spec, applyPatchHandler(cfg))
}

// NewApplyPatchTool creates a single apply_patch tool that can be installed
// directly as a threads.ToolProvider and threads.ToolResolver.
func NewApplyPatchTool(cfg ApplyPatchConfig) *ApplyPatchTool {
	return &ApplyPatchTool{catalog: AddApplyPatch(tool.NewCatalog(), cfg)}
}

// ToolsSnapshot implements threads.ToolProvider.
func (a *ApplyPatchTool) ToolsSnapshot(thread threads.Thread) threads.ToolsSnapshot {
	return a.catalog.ToolsSnapshot(thread)
}

// ResolveTool implements threads.ToolResolver.
func (a *ApplyPatchTool) ResolveTool(ctx context.Context, thread threads.Thread, call threads.ToolCall, load json.RawMessage) (threads.ToolDispatch, error) {
	return a.catalog.ResolveTool(ctx, thread, call, load)
}

func applyPatchHandler(cfg ApplyPatchConfig) tool.HandlerFunc {
	queue := mutationQueueOrDefault(cfg.MutationQueue)
	return func(ctx context.Context, _ threads.Thread, call tool.Call, ret tool.ReturnItem) (tool.Handling, error) {
		patchText, err := decodePatchPayload(call, cfg.Mode)
		if err != nil {
			return unsafeHandling(), ret(tool.ResultError(call, err))
		}
		hunks, err := parsePatch(patchText)
		if err != nil {
			return unsafeHandling(), ret(tool.ResultError(call, fmt.Errorf("apply_patch verification failed: %w", err)))
		}
		var changes []fileChange
		err = queue.withLocks(hunkPaths(cfg.CWD, hunks), func() error {
			var err error
			changes, err = planPatch(cfg.CWD, hunks)
			if err != nil {
				return err
			}
			postprocessPatchChanges(ctx, cfg.Postprocess, changes)
			return applyPatch(changes)
		})
		if err != nil {
			return unsafeHandling(), ret(tool.ResultError(call, fmt.Errorf("apply_patch verification failed: %w", err)))
		}
		result := buildApplyPatchResult(changes)
		builder := cfg.ResultBuilder
		if builder == nil {
			builder = defaultApplyPatchResultBuilder
		}
		return unsafeHandling(), ret(builder(call, result))
	}
}

func decodePatchPayload(call tool.Call, mode ApplyPatchMode) (string, error) {
	if mode == ApplyPatchModeJSON {
		var args applyPatchArgs
		if err := call.UnmarshalJSON(&args); err != nil {
			return "", fmt.Errorf("tool %q payload: %w", call.Name, err)
		}
		if strings.TrimSpace(args.PatchText) == "" {
			return "", fmt.Errorf("patchText or raw patch payload is required")
		}
		return args.PatchText, nil
	}
	var args applyPatchArgs
	if err := call.UnmarshalJSON(&args); err == nil && strings.TrimSpace(args.PatchText) != "" {
		return args.PatchText, nil
	}
	if strings.TrimSpace(call.Payload) != "" {
		return call.Payload, nil
	}
	return "", fmt.Errorf("patchText or raw patch payload is required")
}

func postprocessPatchChanges(ctx context.Context, cfg PostprocessConfig, changes []fileChange) {
	for i := range changes {
		change := &changes[i]
		if change.typ == "delete" {
			continue
		}
		targetPath := change.path
		displayPath := change.relative
		if change.movePath != "" {
			targetPath = change.movePath
			displayPath = change.moveRel
		}
		operation := FileOperationPatchUpdate
		switch change.typ {
		case "add":
			operation = FileOperationPatchAdd
		case "move":
			operation = FileOperationPatchMove
		}
		processed, reports, err := runPostprocess(ctx, cfg, FilePostprocessRequest{
			Tool:                applyPatchToolName,
			Operation:           operation,
			Path:                targetPath,
			DisplayPath:         displayPath,
			Content:             []byte(change.newContent),
			OldPath:             changeRequestOldPath(*change),
			OldDisplayPath:      changeRequestOldDisplayPath(*change),
			OldContent:          []byte(change.oldContent),
			OldContentAvailable: change.typ != "add",
			PureMove:            change.pureMove,
		})
		if err != nil {
			change.postprocessError = err.Error()
			continue
		}
		if processed.ContentChanged {
			change.newContent = string(processed.Content)
			oldName, newName := patchDiffNames(*change)
			change.diff = unifiedContext(oldName, newName, change.oldContent, change.newContent, 3)
		}
		change.postprocess = reports
	}
}

func patchDiffNames(change fileChange) (string, string) {
	oldName := change.relative + " (before)"
	newName := change.relative + " (after)"
	if change.typ == "move" && change.moveRel != "" {
		newName = change.moveRel + " (after)"
	}
	return oldName, newName
}

func buildApplyPatchResult(changes []fileChange) ApplyPatchResult {
	var diff strings.Builder
	res := ApplyPatchResult{
		Changes: make([]ApplyPatchChangeResult, 0, len(changes)),
	}
	for _, change := range changes {
		diff.WriteString(change.diff)
		diff.WriteByte('\n')
		changeRes := ApplyPatchChangeResult{
			Operation:      changeOperation(change),
			Path:           changeOutputPath(change),
			DisplayPath:    changeOutputDisplayPath(change),
			OldPath:        changeRequestOldPath(change),
			OldDisplayPath: changeRequestOldDisplayPath(change),
			Diff:             change.diff,
			Postprocess:      change.postprocess,
			PostprocessError: change.postprocessError,
		}
		res.Changes = append(res.Changes, changeRes)
		res.Postprocess = append(res.Postprocess, change.postprocess...)
	}
	res.Diff = diff.String()
	return res
}

func defaultApplyPatchResultBuilder(call tool.Call, res ApplyPatchResult) tool.Item {
	files := make([]map[string]any, 0, len(res.Changes))
	lines := make([]string, 0, len(res.Changes))
	for _, change := range res.Changes {
		code := "M"
		typ := "update"
		switch change.Operation {
		case FileOperationPatchAdd:
			code = "A"
			typ = "add"
		case FileOperationPatchMove:
			typ = "move"
		case "":
			code = "D"
			typ = "delete"
		}
		lines = append(lines, applyPatchChangeOutputLines(code, change)...)
		file := map[string]any{
			"path": change.DisplayPath,
			"type": typ,
			"diff": change.Diff,
		}
		if change.PostprocessError != "" {
			file["postprocessError"] = change.PostprocessError
		}
		files = append(files, file)
	}
	output := "Success. Updated the following files:\n" + strings.Join(lines, "\n")
	data := map[string]any{
		"diff":  res.Diff,
		"files": files,
	}
	if len(res.Postprocess) > 0 {
		data["postprocess"] = res.Postprocess
	}
	return threads.ToolCallResult{
		CallID: call.CallID,
		Output: output,
		Data:   data,
	}
}

func applyPatchChangeOutputLines(code string, change ApplyPatchChangeResult) []string {
	lines := []string{code + " " + change.DisplayPath}
	if imports := postprocessImportDiffLines(change.Postprocess); len(imports) > 0 {
		lines = append(lines, "\tImports:")
		for _, line := range imports {
			lines = append(lines, "\t"+line)
		}
	}
	if formatted := postprocessFormattedLines(change.Postprocess); formatted != "" {
		lines = append(lines, "\tgofmt: updated "+formatted+" lines")
	}
	if change.PostprocessError != "" {
		for _, line := range strings.Split(change.PostprocessError, "\n") {
			lines = append(lines, "\t"+line)
		}
	}
	return lines
}

func changeOperation(change fileChange) FileOperation {
	switch change.typ {
	case "add":
		return FileOperationPatchAdd
	case "update":
		return FileOperationPatchUpdate
	case "move":
		return FileOperationPatchMove
	default:
		return ""
	}
}

func changeOutputPath(change fileChange) string {
	if change.movePath != "" {
		return change.movePath
	}
	return change.path
}

func changeOutputDisplayPath(change fileChange) string {
	if change.moveRel != "" {
		return change.moveRel
	}
	return change.relative
}

func changeRequestOldPath(change fileChange) string {
	if change.typ == "add" {
		return ""
	}
	return change.path
}

func changeRequestOldDisplayPath(change fileChange) string {
	if change.typ == "add" {
		return ""
	}
	return change.relative
}

func unsafeHandling() tool.Handling {
	return tool.Handling{Recovery: threads.ToolRecoveryUnsafe}
}

func applyPatchPayload(cfg ApplyPatchConfig) tool.Payload {
	if cfg.Mode == ApplyPatchModeJSON {
		return tool.PayloadJSONSchema(gschema.Schema{
			Type: "object",
			Properties: map[string]*gschema.Schema{
				"patchText": {Type: "string", Description: configString(cfg.PatchTextDescription, "The full patch text that describes all changes to be made")},
			},
			PropertyOrder: []string{"patchText"},
		})
	}
	return tool.PayloadLark(applyPatchLarkGrammar)
}
