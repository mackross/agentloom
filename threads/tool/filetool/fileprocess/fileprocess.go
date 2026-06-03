package fileprocess

import "context"

// Operation describes the kind of file mutation that produced the candidate
// content passed to a Processor.
type Operation string

const (
	OperationWrite       Operation = "write"
	OperationEdit        Operation = "edit"
	OperationPatchAdd    Operation = "patch_add"
	OperationPatchUpdate Operation = "patch_update"
	OperationPatchMove   Operation = "patch_move"
)

// Request is the complete context for one candidate file content produced by a
// file mutation tool.
type Request struct {
	// Tool is the tool invoking the processor: "write", "apply_patch", or "edit".
	Tool string

	// Operation describes the mutation that produced Content.
	Operation Operation

	// Path is the absolute resolved path whose content will be written.
	Path string

	// DisplayPath is the model-visible/user-supplied path.
	DisplayPath string

	// Content is the current candidate file content. Processors run sequentially,
	// so this includes changes from earlier processors.
	Content []byte

	// OldPath/OldDisplayPath identify the previous path when naturally known. For
	// patch moves, OldPath differs from Path. For writes, these can be empty.
	OldPath        string
	OldDisplayPath string

	// OldContent is populated when naturally available. Tools should not read old
	// content only to fill this for write; processing should stay cheap unless a
	// processor opts into its own reads.
	OldContent          []byte
	OldContentAvailable bool

	// PureMove is true for an apply_patch move that has no content-changing hunks.
	PureMove bool
}

// Result is returned by a Processor. Content is used only when ContentChanged is
// true so processors can intentionally replace content with an empty file.
type Result struct {
	Content        []byte
	ContentChanged bool

	// Report is optional structured data consumed by tool result builders and Data
	// maps.
	Report *Report
}

// Report is optional structured information about a processing pass.
type Report struct {
	Processor   string         `json:"processor,omitempty"`
	Operation   Operation      `json:"operation,omitempty"`
	Path        string         `json:"path,omitempty"`
	DisplayPath string         `json:"displayPath,omitempty"`
	Summary     string         `json:"summary,omitempty"`
	Data        map[string]any `json:"data,omitempty"`
}

// Processor mutates, validates, or reports on candidate file content.
type Processor interface {
	ProcessFile(context.Context, Request) (Result, error)
}

// ProcessorFunc adapts a function to Processor.
type ProcessorFunc func(context.Context, Request) (Result, error)

// ProcessFile implements Processor.
func (f ProcessorFunc) ProcessFile(ctx context.Context, req Request) (Result, error) {
	return f(ctx, req)
}

// Config configures the processor pipeline used by file tools.
type Config struct {
	// BeforeDefault runs before built-in processors such as Go formatting.
	BeforeDefault []Processor

	// DisableDefault disables built-in processors.
	DisableDefault bool

	// AfterDefault runs after built-in processors.
	AfterDefault []Processor
}

// Run executes processors in order against req.Content. The output of one
// processor becomes the input to the next. If a processor errors, Run stops and
// returns the original request content plus no reports, making it safe for tools
// to preserve no-write guarantees.
func Run(ctx context.Context, req Request, processors []Processor) (Result, []Report, error) {
	content := append([]byte(nil), req.Content...)
	reports := make([]Report, 0)
	changed := false
	for _, processor := range processors {
		if processor == nil {
			continue
		}
		nextReq := req
		nextReq.Content = content
		res, err := processor.ProcessFile(ctx, nextReq)
		if err != nil {
			return Result{Content: append([]byte(nil), req.Content...)}, nil, err
		}
		if res.ContentChanged {
			content = append([]byte(nil), res.Content...)
			changed = true
		}
		if res.Report != nil {
			reports = append(reports, *res.Report)
		}
	}
	return Result{
		Content:        content,
		ContentChanged: changed,
	}, reports, nil
}

// Pipeline returns the effective processor list for cfg and defaultProcessors.
// The returned slice is fresh and safe for the caller to mutate.
func Pipeline(cfg Config, defaultProcessors []Processor) []Processor {
	count := len(cfg.BeforeDefault) + len(cfg.AfterDefault)
	if !cfg.DisableDefault {
		count += len(defaultProcessors)
	}
	processors := make([]Processor, 0, count)
	processors = append(processors, cfg.BeforeDefault...)
	if !cfg.DisableDefault {
		processors = append(processors, defaultProcessors...)
	}
	processors = append(processors, cfg.AfterDefault...)
	return processors
}
