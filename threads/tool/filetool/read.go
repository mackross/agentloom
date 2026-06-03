package filetool

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	gschema "github.com/google/jsonschema-go/jsonschema"

	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/tool"
)

const (
	readToolName          = "read"
	readToolDescription   = "Read text contents of a file"
	readPathDescription   = "Relative or absolute file path"
	readOffsetDescription = "Read from line (1-indexed)."
	readLimitDescription  = "Lines to read. 0 = default limit."

	defaultReadMaxBytes      = 128 * 1024
	defaultBinaryReadLimit   = 512
	maxBinaryReadLimit       = 4096
	defaultTextByteReadLimit = 16 * 1024
	maxTextByteReadLimit     = 64 * 1024
)

// ReadConfig configures the read tool.
type ReadConfig struct {
	// ToolDescription overrides the model-visible tool description when non-nil.
	ToolDescription *string
	// PathDescription overrides the model-visible path field description when non-nil.
	PathDescription *string
	// OffsetDescription overrides the model-visible offset field description when non-nil.
	OffsetDescription *string
	// LimitDescription overrides the model-visible limit field description when non-nil.
	LimitDescription *string
	// CWD resolves relative paths. Absolute paths are used as provided.
	CWD string
	// PathRestrictions optionally restricts readable paths.
	PathRestrictions *PathRestrictionConfig
	// MaxLines limits successful output by line count when positive.
	MaxLines int
	// MaxBytes is a safety cap to prevent very large files from being dropped
	// into model context all at once. Zero uses the package default; negative
	// disables the byte cap. When ordinary line-based output exceeds this cap,
	// read returns a text byte-window view with a continuation path.
	MaxBytes int
}

type readArgs struct {
	Path   string `json:"path" jsonschema:"Relative or absolute file path"`
	Offset int    `json:"offset" jsonschema:"Read from line (1-indexed)."`
	Limit  int    `json:"limit" jsonschema:"Lines to read. 0 = default limit."`
}

type readPathOptions struct {
	Mode       string `json:"mode"`
	ByteOffset int    `json:"byteOffset,omitempty"`
	ByteLimit  int    `json:"byteLimit,omitempty"`
}

// ReadTool is the AgentLoom-native single-tool provider and resolver returned
// by NewReadTool.
type ReadTool struct {
	catalog *tool.Catalog
}

var (
	_ threads.ToolProvider = (*ReadTool)(nil)
	_ threads.ToolResolver = (*ReadTool)(nil)
)

// AddRead adds the read tool to c and returns c for fluent catalog setup.
func AddRead(c *tool.Catalog, cfg ReadConfig) *tool.Catalog {
	if c == nil {
		c = tool.NewCatalog()
	}
	spec := tool.Spec{
		Name:        readToolName,
		Description: configString(cfg.ToolDescription, readToolDescription),
		Payload:     readPayload(cfg),
	}
	return c.AddFunc(spec, readHandler(cfg))
}

// NewReadTool creates a single read tool that can be installed directly as a
// threads.ToolProvider and threads.ToolResolver.
func NewReadTool(cfg ReadConfig) *ReadTool {
	return &ReadTool{catalog: AddRead(tool.NewCatalog(), cfg)}
}

// ToolsSnapshot implements threads.ToolProvider.
func (r *ReadTool) ToolsSnapshot(thread threads.Thread) threads.ToolsSnapshot {
	return r.catalog.ToolsSnapshot(thread)
}

// ResolveTool implements threads.ToolResolver.
func (r *ReadTool) ResolveTool(ctx context.Context, thread threads.Thread, call threads.ToolCall, load json.RawMessage) (threads.ToolDispatch, error) {
	return r.catalog.ResolveTool(ctx, thread, call, load)
}

func readHandler(cfg ReadConfig) tool.HandlerFunc {
	return func(_ context.Context, _ threads.Thread, call tool.Call, ret tool.ReturnItem) (tool.Handling, error) {
		var args readArgs
		if err := call.UnmarshalJSON(&args); err != nil {
			return tool.Handling{}, ret(tool.ResultError(call, fmt.Errorf("tool %q payload: %w", call.Name, err)))
		}
		out, err := readFile(cfg, args)
		if err != nil {
			return tool.Handling{}, ret(tool.ResultError(call, err))
		}
		return tool.Handling{}, ret(tool.ResultText(call, out))
	}
}

func readFile(cfg ReadConfig, args readArgs) (string, error) {
	modelPath := args.Path
	if modelPath == "" {
		return "", fmt.Errorf("read: path is required")
	}
	displayPath, opts := splitReadPathOptions(modelPath)
	modelPath = displayPath
	if err := checkPathAllowed(cfg.CWD, modelPath, cfg.PathRestrictions); err != nil {
		return "", fmt.Errorf("read: %w", err)
	}
	fullPath := resolvePath(cfg.CWD, modelPath)
	buf, err := os.ReadFile(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("read: open %s: no such file or directory", modelPath)
		}
		return "", fmt.Errorf("read: open %s: %w", modelPath, err)
	}
	if opts.Mode == "binary" {
		return readBinary(displayPath, buf, opts), nil
	}
	if !utf8.Valid(buf) {
		return binaryTextOmittedNotice(displayPath, buf), nil
	}
	if opts.Mode == "text" {
		return readTextByteWindow(displayPath, buf, opts), nil
	}
	text := string(buf)
	offset := args.Offset
	if offset <= 0 {
		offset = 1
	}
	sliced, startByte, _, totalLines, err := applyOffsetAndLimit(text, offset, args.Limit)
	if err != nil {
		return "", err
	}
	if offset > 1 && totalLines == 0 {
		return "", fmt.Errorf("read: offset %d is beyond end of file (0 lines)", offset)
	}
	lineLimited := applyConfiguredTruncation(sliced, cfg.MaxLines, offset)
	if maxBytes := readMaxBytes(cfg); maxBytes > 0 && len(lineLimited) > maxBytes {
		limit := defaultTextByteReadLimit
		if maxBytes < limit {
			limit = maxBytes
		}
		return readTextByteWindow(displayPath, buf, readPathOptions{
			Mode:       "text",
			ByteOffset: startByte,
			ByteLimit:  limit,
		}), nil
	}
	return lineLimited, nil
}

func readMaxBytes(cfg ReadConfig) int {
	if cfg.MaxBytes == 0 {
		return defaultReadMaxBytes
	}
	return cfg.MaxBytes
}

func splitReadPathOptions(path string) (string, readPathOptions) {
	const marker = "?read_options="
	i := strings.LastIndex(path, marker)
	if i < 0 {
		return path, readPathOptions{}
	}
	raw := path[i+len(marker):]
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var opts readPathOptions
	if err := dec.Decode(&opts); err != nil {
		return path, readPathOptions{}
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return path, readPathOptions{}
	}
	if opts.Mode != "binary" && opts.Mode != "text" {
		return path, readPathOptions{}
	}
	if opts.ByteOffset < 0 || opts.ByteLimit < 0 {
		return path, readPathOptions{}
	}
	return path[:i], opts
}

func binaryTextOmittedNotice(path string, buf []byte) string {
	const magicBytes = 64
	n := len(buf)
	if n > magicBytes {
		n = magicBytes
	}
	opening := strings.TrimSuffix(hex.Dump(buf[:n]), "\n")
	if opening != "" {
		opening = "\nOpening bytes:\n" + opening
	}
	return fmt.Sprintf("read: %s is not valid UTF-8 text%s\n[Output omitted because this appears to be a binary file. To inspect binary contents, call read with path %q. Optional binary read_options: byteOffset defaults to 0; byteLimit is file bytes to hexdump and defaults to %d, capped at %d.]", path, opening, path+fmt.Sprintf(`?read_options={"mode":"binary","byteOffset":0,"byteLimit":%d}`, defaultBinaryReadLimit), defaultBinaryReadLimit, maxBinaryReadLimit)
}

func readBinary(path string, buf []byte, opts readPathOptions) string {
	offset := opts.ByteOffset
	if offset > len(buf) {
		offset = len(buf)
	}
	limit := opts.ByteLimit
	if limit <= 0 {
		limit = defaultBinaryReadLimit
	}
	if limit > maxBinaryReadLimit {
		limit = maxBinaryReadLimit
	}
	remaining := len(buf) - offset
	if limit > remaining {
		limit = remaining
	}
	end := offset + limit
	data := buf[offset:end]
	out := hex.Dump(data)
	header := fmt.Sprintf("[Binary file: %d bytes. Showing bytes %d..%d.]\n", len(buf), offset, end)
	if end < len(buf) {
		next := fmt.Sprintf(`%s?read_options={"mode":"binary","byteOffset":%d,"byteLimit":%d}`, path, end, continuationBinaryLimit(opts, limit))
		out += fmt.Sprintf("\n[Binary output truncated after %d of %d bytes. Continue with path %q.]", end, len(buf), next)
	}
	return strings.TrimSuffix(header+out, "\n")
}

func continuationBinaryLimit(opts readPathOptions, visible int) int {
	if opts.ByteLimit > 0 {
		if opts.ByteLimit > maxBinaryReadLimit {
			return maxBinaryReadLimit
		}
		return opts.ByteLimit
	}
	return visible
}

func readTextByteWindow(path string, buf []byte, opts readPathOptions) string {
	offset := opts.ByteOffset
	if offset > len(buf) {
		offset = len(buf)
	}
	start := utf8SafeStart(buf, offset)
	limit := opts.ByteLimit
	if limit <= 0 {
		limit = defaultTextByteReadLimit
	}
	if limit > maxTextByteReadLimit {
		limit = maxTextByteReadLimit
	}
	remaining := len(buf) - start
	if limit > remaining {
		limit = remaining
	}
	end := utf8SafeEnd(buf, start, start+limit)
	text := string(buf[start:end])
	header := fmt.Sprintf("[Text byte window: %d bytes. Showing bytes %d..%d.]\n", len(buf), start, end)
	if end < len(buf) {
		next := fmt.Sprintf(`%s?read_options={"mode":"text","byteOffset":%d,"byteLimit":%d}`, path, end, continuationTextByteLimit(opts, end-start))
		text += fmt.Sprintf("\n[Text byte window truncated after byte %d of %d. Continue with path %q.]", end, len(buf), next)
	}
	return header + text
}

func continuationTextByteLimit(opts readPathOptions, visible int) int {
	if opts.ByteLimit > 0 {
		if opts.ByteLimit > maxTextByteReadLimit {
			return maxTextByteReadLimit
		}
		return opts.ByteLimit
	}
	return visible
}

func utf8SafeStart(buf []byte, offset int) int {
	if offset <= 0 {
		return 0
	}
	if offset >= len(buf) {
		return len(buf)
	}
	for offset < len(buf) && !utf8.RuneStart(buf[offset]) {
		offset++
	}
	return offset
}

func utf8SafeEnd(buf []byte, start, end int) int {
	if end >= len(buf) {
		end = len(buf)
	}
	if end < start {
		return start
	}
	for end > start && !utf8.Valid(buf[start:end]) {
		end--
	}
	return end
}

func applyOffsetAndLimit(text string, offset, limit int) (string, int, int, int, error) {
	if offset <= 0 {
		offset = 1
	}
	spans := lineSpans(text)
	total := len(spans)
	if total == 0 {
		if offset > 1 {
			return "", 0, 0, total, fmt.Errorf("read: offset %d is beyond end of file (%d lines)", offset, total)
		}
		return "", 0, 0, total, nil
	}
	if offset > total {
		return "", 0, 0, total, fmt.Errorf("read: offset %d is beyond end of file (%d lines)", offset, total)
	}
	start := spans[offset-1].start
	endIndex := total
	if limit > 0 && offset-1+limit < endIndex {
		endIndex = offset - 1 + limit
	}
	end := spans[endIndex-1].end
	return text[start:end], start, end, total, nil
}

type lineSpan struct {
	start int
	end   int
}

func lineSpans(text string) []lineSpan {
	if text == "" {
		return nil
	}
	spans := []lineSpan{}
	start := 0
	for i, r := range text {
		if r != '\n' {
			continue
		}
		spans = append(spans, lineSpan{start: start, end: i + 1})
		start = i + 1
	}
	if start < len(text) {
		spans = append(spans, lineSpan{start: start, end: len(text)})
	}
	return spans
}

func readPayload(cfg ReadConfig) tool.Payload {
	return tool.PayloadJSONSchema(gschema.Schema{
		Type: "object",
		Properties: map[string]*gschema.Schema{
			"path":   {Type: "string", Description: configString(cfg.PathDescription, readPathDescription)},
			"offset": {Type: "integer", Description: configString(cfg.OffsetDescription, readOffsetDescription)},
			"limit":  {Type: "integer", Description: configString(cfg.LimitDescription, readLimitDescription)},
		},
		PropertyOrder: []string{"path", "offset", "limit"},
	})
}
