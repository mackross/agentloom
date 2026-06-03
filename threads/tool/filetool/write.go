package filetool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	gschema "github.com/google/jsonschema-go/jsonschema"

	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/tool"
)

const (
	writeToolName        = "write"
	writeToolDescription = "Create or overwrite a file"
	writePathDescription = "Relative or absolute file path"
	writeContentDescription = "Exact text content for entire file"
)

// WriteConfig configures the write tool.
//
// Zero-value config runs the default postprocess pipeline, including Go
// formatting/import fixing for .go files. If postprocessing fails, the previous
// file contents are left unchanged and the failure is returned as a
// model-visible tool error.
type WriteConfig struct {
	// ToolDescription overrides the model-visible tool description when non-nil.
	ToolDescription *string
	// PathDescription overrides the model-visible path field description when non-nil.
	PathDescription *string
	// ContentDescription overrides the model-visible content field description when non-nil.
	ContentDescription *string
	// CWD resolves relative paths. Absolute paths are used as provided.
	CWD string
	// Postprocess configures the file-content processing pipeline.
	Postprocess PostprocessConfig
	// ResultBuilder customizes the returned tool item. When nil, the stable text
	// output is "Wrote N bytes to <path>.".
	ResultBuilder WriteResultBuilder
	// MutationQueue serializes overlapping file mutations. When nil,
	// DefaultMutationQueue is used.
	MutationQueue *MutationQueue
}

type WriteResult struct {
	Path        string
	DisplayPath string
	Bytes       int

	Postprocess []FilePostprocessReport
}

type WriteResultBuilder func(tool.Call, WriteResult) tool.Item

type writeArgs struct {
	Path    string `json:"path" jsonschema:"Relative or absolute file path"`
	Content string `json:"content" jsonschema:"Exact text content for entire file"`
}

// WriteTool is the AgentLoom-native single-tool provider and resolver returned
// by NewWriteTool.
type WriteTool struct {
	catalog *tool.Catalog
}

var (
	_ threads.ToolProvider = (*WriteTool)(nil)
	_ threads.ToolResolver = (*WriteTool)(nil)
)

// AddWrite adds the write tool to c and returns c for fluent catalog setup.
func AddWrite(c *tool.Catalog, cfg WriteConfig) *tool.Catalog {
	if c == nil {
		c = tool.NewCatalog()
	}
	spec := tool.Spec{
		Name:        writeToolName,
		Description: configString(cfg.ToolDescription, writeToolDescription),
		Payload:     writePayload(cfg),
	}
	return c.AddFunc(spec, writeHandler(cfg))
}

// NewWriteTool creates a single write tool that can be installed directly as a
// threads.ToolProvider and threads.ToolResolver.
func NewWriteTool(cfg WriteConfig) *WriteTool {
	return &WriteTool{catalog: AddWrite(tool.NewCatalog(), cfg)}
}

// ToolsSnapshot implements threads.ToolProvider.
func (w *WriteTool) ToolsSnapshot(thread threads.Thread) threads.ToolsSnapshot {
	return w.catalog.ToolsSnapshot(thread)
}

// ResolveTool implements threads.ToolResolver.
func (w *WriteTool) ResolveTool(ctx context.Context, thread threads.Thread, call threads.ToolCall, load json.RawMessage) (threads.ToolDispatch, error) {
	return w.catalog.ResolveTool(ctx, thread, call, load)
}

func writeHandler(cfg WriteConfig) tool.HandlerFunc {
	return func(ctx context.Context, _ threads.Thread, call tool.Call, ret tool.ReturnItem) (tool.Handling, error) {
		var args writeArgs
		if err := call.UnmarshalJSON(&args); err != nil {
			return tool.Handling{Recovery: threads.ToolRecoveryUnsafe}, ret(tool.ResultError(call, fmt.Errorf("tool %q payload: %w", call.Name, err)))
		}
		res, err := writeFile(ctx, cfg, args)
		if err != nil {
			return tool.Handling{Recovery: threads.ToolRecoveryUnsafe}, ret(tool.ResultError(call, err))
		}
		builder := cfg.ResultBuilder
		if builder == nil {
			builder = defaultWriteResultBuilder
		}
		return tool.Handling{Recovery: threads.ToolRecoveryUnsafe}, ret(builder(call, res))
	}
}

func writeFile(ctx context.Context, cfg WriteConfig, args writeArgs) (WriteResult, error) {
	modelPath := args.Path
	if modelPath == "" {
		return WriteResult{}, fmt.Errorf("write: path is required")
	}
	fullPath, err := safePatchPath(cfg.CWD, modelPath)
	if err != nil {
		return WriteResult{}, fmt.Errorf("write: %w", err)
	}
	var res WriteResult
	err = mutationQueueOrDefault(cfg.MutationQueue).withLocks([]string{fullPath}, func() error {
		content := []byte(args.Content)
		processed, reports, err := runPostprocess(ctx, cfg.Postprocess, FilePostprocessRequest{
			Tool:        writeToolName,
			Operation:   FileOperationWrite,
			Path:        fullPath,
			DisplayPath: modelPath,
			Content:     content,
		})
		if err != nil {
			return fmt.Errorf("wrote %s\n%w", modelPath, err)
		}
		if processed.ContentChanged {
			content = processed.Content
		}
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			return fmt.Errorf("write: create parent directories for %s: %w", modelPath, err)
		}
		if err := os.WriteFile(fullPath, content, 0o644); err != nil {
			return fmt.Errorf("write: write %s: %w", modelPath, err)
		}
		res = WriteResult{
			Path:        fullPath,
			DisplayPath: modelPath,
			Bytes:       len(content),
			Postprocess: reports,
		}
		return nil
	})
	if err != nil {
		return WriteResult{}, err
	}
	return res, nil
}

func defaultWriteResultBuilder(call tool.Call, res WriteResult) tool.Item {
	output := "Wrote " + res.DisplayPath
	if lines := postprocessImportDiffLines(res.Postprocess); len(lines) > 0 {
		output += "\nImports:\n" + strings.Join(lines, "\n")
	}
	if lines := postprocessFormattedLines(res.Postprocess); lines != "" {
		output += "\ngofmt: updated " + lines + " lines"
	}
	return tool.ResultText(call, output)
}

func postprocessImportDiffLines(reports []FilePostprocessReport) []string {
	lines := make([]string, 0)
	for _, report := range reports {
		if report.Data == nil {
			continue
		}
		raw, ok := report.Data["importDiff"]
		if !ok {
			continue
		}
		switch diff := raw.(type) {
		case []string:
			lines = append(lines, diff...)
		case []any:
			for _, line := range diff {
				if s, ok := line.(string); ok {
					lines = append(lines, s)
				}
			}
		}
	}
	return lines
}

func postprocessFormattedLines(reports []FilePostprocessReport) string {
	formatted := postprocessFormattedLineReports(reports)
	if len(formatted) == 0 {
		return ""
	}
	return formatted[0].lines
}

type formattedLineReport struct {
	path  string
	lines string
}

func postprocessFormattedLineReports(reports []FilePostprocessReport) []formattedLineReport {
	formatted := make([]formattedLineReport, 0)
	for _, report := range reports {
		if report.Data == nil {
			continue
		}
		if lines, ok := report.Data["formattedLines"].(string); ok && lines != "" {
			path := report.DisplayPath
			if path == "" {
				path = report.Path
			}
			formatted = append(formatted, formattedLineReport{path: path, lines: lines})
		}
	}
	return formatted
}

func writePayload(cfg WriteConfig) tool.Payload {
	return tool.PayloadJSONSchema(gschema.Schema{
		Type: "object",
		Properties: map[string]*gschema.Schema{
			"path":    {Type: "string", Description: configString(cfg.PathDescription, writePathDescription)},
			"content": {Type: "string", Description: configString(cfg.ContentDescription, writeContentDescription)},
		},
		PropertyOrder: []string{"path", "content"},
	})
}
