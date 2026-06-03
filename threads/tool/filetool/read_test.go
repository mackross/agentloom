package filetool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	gschema "github.com/google/jsonschema-go/jsonschema"

	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/tool"
)

func TestAddReadSnapshotSpec(t *testing.T) {
	cat := tool.NewCatalog()
	if got := AddRead(cat, ReadConfig{}); got != cat {
		t.Fatalf("AddRead returned a different catalog")
	}

	snap := cat.Snapshot()
	if len(snap.Offered) != 1 {
		t.Fatalf("expected one offered tool, got %#v", snap.Offered)
	}
	spec := snap.Offered[0]
	if spec.Name != "read" {
		t.Fatalf("unexpected tool name: %q", spec.Name)
	}
	if spec.Description != "Read text contents of a file" {
		t.Fatalf("unexpected tool description: %q", spec.Description)
	}
	schema, ok := spec.Payload.(tool.PayloadJSONSchema)
	if !ok {
		t.Fatalf("expected JSON schema payload, got %T", spec.Payload)
	}
	got := gschema.Schema(schema)
	if got.Type != "object" {
		t.Fatalf("schema type = %q, want object", got.Type)
	}
	if !reflect.DeepEqual(got.PropertyOrder, []string{"path", "offset", "limit"}) {
		t.Fatalf("unexpected property order: %#v", got.PropertyOrder)
	}
	wantDescriptions := map[string]string{
		"path":   "Relative or absolute file path",
		"offset": "Read from line (1-indexed).",
		"limit":  "Lines to read. 0 = default limit.",
	}
	for name, want := range wantDescriptions {
		prop := got.Properties[name]
		if prop == nil {
			t.Fatalf("missing schema property %q in %#v", name, got.Properties)
		}
		if prop.Description != want {
			t.Fatalf("property %q description = %q, want %q", name, prop.Description, want)
		}
	}
	tools := cat.ToolsSnapshot(nil)
	if !reflect.DeepEqual(tools.Snapshot, snap) {
		t.Fatalf("ToolsSnapshot snapshot mismatch:\n got: %#v\nwant: %#v", tools.Snapshot, snap)
	}
	if want := []threads.ToolHandlerBinding{{Name: "read"}}; !reflect.DeepEqual(tools.Handlers, want) {
		t.Fatalf("unexpected handler bindings: %#v", tools.Handlers)
	}
}

func TestReadConfigDescriptionOverrides(t *testing.T) {
	toolDescription := "custom read"
	pathDescription := "custom path"
	offsetDescription := "custom offset"
	limitDescription := "custom limit"
	cat := AddRead(tool.NewCatalog(), ReadConfig{
		ToolDescription:   &toolDescription,
		PathDescription:   &pathDescription,
		OffsetDescription: &offsetDescription,
		LimitDescription:  &limitDescription,
	})
	spec := cat.Snapshot().Offered[0]
	if spec.Description != toolDescription {
		t.Fatalf("tool description = %q, want %q", spec.Description, toolDescription)
	}
	schema := gschema.Schema(spec.Payload.(tool.PayloadJSONSchema))
	if schema.Properties["path"].Description != pathDescription ||
		schema.Properties["offset"].Description != offsetDescription ||
		schema.Properties["limit"].Description != limitDescription {
		t.Fatalf("unexpected property descriptions: %#v", schema.Properties)
	}
}

func TestNewReadToolDirectProviderResolver(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "notes.txt"), "one\ntwo\n")

	readTool := NewReadTool(ReadConfig{CWD: dir})
	var _ threads.ToolProvider = readTool
	var _ threads.ToolResolver = readTool

	snap := readTool.ToolsSnapshot(nil)
	if len(snap.Snapshot.Offered) != 1 || snap.Snapshot.Offered[0].Name != "read" {
		t.Fatalf("unexpected direct tool snapshot: %#v", snap)
	}
	if len(snap.Handlers) != 1 || snap.Handlers[0].Name != "read" {
		t.Fatalf("unexpected direct handler bindings: %#v", snap.Handlers)
	}

	dispatch, err := readTool.ResolveTool(context.Background(), nil, threads.ToolCall{
		CallID:  "c1",
		Name:    "read",
		Payload: `{"path":"notes.txt"}`,
	}, nil)
	if err != nil {
		t.Fatalf("ResolveTool: %v", err)
	}
	assertTextResult(t, dispatch, "one\ntwo\n")
}

func TestReadCatalogDispatchWorks(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "notes.txt"), "hello\n")
	cat := AddRead(tool.NewCatalog(), ReadConfig{CWD: dir})

	dispatch, err := cat.Dispatch(context.Background(), nil, tool.Call{
		CallID:  "c1",
		Name:    "read",
		Payload: `{"path":"notes.txt"}`,
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !dispatch.Started {
		t.Fatalf("expected dispatch to be marked started")
	}
	assertTextResult(t, dispatch, "hello\n")
}

func TestReadEndToEndThroughThreadRuntime(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "notes.txt"), "from runtime\n")

	thread := threads.New()
	readTool := NewReadTool(ReadConfig{CWD: dir})
	thread.SetToolProvider(readTool)
	thread.SetToolResolver(readTool)
	thread.SetExecutor(threads.NewThreadExecutor(&readTestStreamer{call: threads.ToolCall{
		CallID:  "c1",
		Name:    "read",
		Payload: `{"path":"notes.txt"}`,
	}}))

	thread.QueueItem(threads.UserText("please read"))
	thread.QueueItem(threads.SendItem{})

	snap, err := thread.Snapshot()
	if err != nil {
		t.Fatalf("snapshot thread: %v", err)
	}
	var found bool
	for _, item := range snap.Items {
		if item.ID == "c1" && item.Type == "tool_result" && item.Output == "from runtime\n" {
			found = true
		}
	}
	if !found {
		t.Fatalf("thread snapshot did not contain read result: %#v", snap.Items)
	}
}

func TestReadOutputGolden(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "multiline.txt"), "alpha\nbeta\ngamma\ndelta\n")
	absPath := filepath.Join(dir, "multiline.txt")

	tests := []struct {
		name    string
		cfg     ReadConfig
		payload string
		golden  string
	}{
		{
			name:    "whole file",
			cfg:     ReadConfig{CWD: dir},
			payload: `{"path":"multiline.txt"}`,
			golden:  "whole_file.txt",
		},
		{
			name:    "absolute path",
			cfg:     ReadConfig{CWD: filepath.Join(dir, "unused")},
			payload: `{"path":` + jsonString(absPath) + `}`,
			golden:  "whole_file.txt",
		},
		{
			name:    "offset limit",
			cfg:     ReadConfig{CWD: dir},
			payload: `{"path":"multiline.txt","offset":2,"limit":2}`,
			golden:  "offset_limit.txt",
		},
		{
			name:    "missing path",
			cfg:     ReadConfig{CWD: dir},
			payload: `{}`,
			golden:  "missing_path.txt",
		},
		{
			name:    "missing file",
			cfg:     ReadConfig{CWD: dir},
			payload: `{"path":"missing.txt"}`,
			golden:  "missing_file.txt",
		},
		{
			name:    "offset beyond eof",
			cfg:     ReadConfig{CWD: dir},
			payload: `{"path":"multiline.txt","offset":99}`,
			golden:  "offset_beyond_eof.txt",
		},
		{
			name:    "max lines",
			cfg:     ReadConfig{CWD: dir, MaxLines: 2},
			payload: `{"path":"multiline.txt"}`,
			golden:  "truncated_lines.txt",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dispatch, err := AddRead(tool.NewCatalog(), tt.cfg).Dispatch(context.Background(), nil, tool.Call{
				CallID:  "c1",
				Name:    "read",
				Payload: tt.payload,
			})
			if err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			result := onlyResult(t, dispatch)
			assertGolden(t, tt.golden, result.Output)
		})
	}
}

func TestReadOffsetAndLimitSemantics(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "lines.txt"), "one\ntwo\nthree\nfour\n")
	mustWriteFile(t, filepath.Join(dir, "empty.txt"), "")

	tests := []struct {
		name    string
		payload string
		want    string
	}{
		{
			name:    "offset less than one starts at beginning",
			payload: `{"path":"lines.txt","offset":0,"limit":2}`,
			want:    "one\ntwo\n",
		},
		{
			name:    "negative offset starts at beginning",
			payload: `{"path":"lines.txt","offset":-10,"limit":1}`,
			want:    "one\n",
		},
		{
			name:    "limit zero means unlimited",
			payload: `{"path":"lines.txt","offset":3,"limit":0}`,
			want:    "three\nfour\n",
		},
		{
			name:    "negative limit means unlimited",
			payload: `{"path":"lines.txt","offset":4,"limit":-2}`,
			want:    "four\n",
		},
		{
			name:    "offset is one indexed",
			payload: `{"path":"lines.txt","offset":2,"limit":1}`,
			want:    "two\n",
		},
		{
			name:    "empty file at start succeeds",
			payload: `{"path":"empty.txt"}`,
			want:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dispatch, err := NewReadTool(ReadConfig{CWD: dir}).ResolveTool(context.Background(), nil, threads.ToolCall{
				CallID:  "c1",
				Name:    "read",
				Payload: tt.payload,
			}, nil)
			if err != nil {
				t.Fatalf("ResolveTool: %v", err)
			}
			assertTextResult(t, dispatch, tt.want)
		})
	}
}

func TestReadEmptyFileOffsetBeyondEOF(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "empty.txt"), "")

	dispatch, err := NewReadTool(ReadConfig{CWD: dir}).ResolveTool(context.Background(), nil, threads.ToolCall{
		CallID:  "c1",
		Name:    "read",
		Payload: `{"path":"empty.txt","offset":2}`,
	}, nil)
	if err != nil {
		t.Fatalf("ResolveTool: %v", err)
	}
	result := onlyResult(t, dispatch)
	if result.Output != "read: offset 2 is beyond end of file (0 lines)" {
		t.Fatalf("unexpected output: %q", result.Output)
	}
	if result.Data["error"] != result.Output {
		t.Fatalf("expected model-visible error data, got %#v", result.Data)
	}
}

func TestReadCallerLimitAppliesBeforeConfiguredLineTruncation(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "lines.txt"), "one\ntwo\nthree\nfour\n")

	dispatch, err := NewReadTool(ReadConfig{CWD: dir, MaxLines: 3}).ResolveTool(context.Background(), nil, threads.ToolCall{
		CallID:  "c1",
		Name:    "read",
		Payload: `{"path":"lines.txt","limit":2}`,
	}, nil)
	if err != nil {
		t.Fatalf("ResolveTool: %v", err)
	}
	assertTextResult(t, dispatch, "one\ntwo\n")
}

func TestReadBinaryFileOmittedByDefaultWithReadOptionsHint(t *testing.T) {
	dir := t.TempDir()
	png := mustReadFixture(t, filepath.Join("testdata", "read", "fixtures", "tiny.png"))
	path := filepath.Join(dir, "tiny.png")
	if err := os.WriteFile(path, png, 0o644); err != nil {
		t.Fatalf("write png fixture: %v", err)
	}

	dispatch, err := NewReadTool(ReadConfig{CWD: dir}).ResolveTool(context.Background(), nil, threads.ToolCall{
		CallID:  "c1",
		Name:    "read",
		Payload: `{"path":"tiny.png"}`,
	}, nil)
	if err != nil {
		t.Fatalf("ResolveTool: %v", err)
	}
	result := onlyResult(t, dispatch)
	assertGolden(t, "binary_png_omitted.txt", result.Output)
}

func TestReadBinaryModeUsesValidReadOptionsSuffix(t *testing.T) {
	dir := t.TempDir()
	png := mustReadFixture(t, filepath.Join("testdata", "read", "fixtures", "tiny.png"))
	if err := os.WriteFile(filepath.Join(dir, "tiny.png"), png, 0o644); err != nil {
		t.Fatalf("write png fixture: %v", err)
	}

	dispatch, err := NewReadTool(ReadConfig{CWD: dir}).ResolveTool(context.Background(), nil, threads.ToolCall{
		CallID:  "c1",
		Name:    "read",
		Payload: `{"path":"tiny.png?read_options={\"mode\":\"binary\"}"}`,
	}, nil)
	if err != nil {
		t.Fatalf("ResolveTool: %v", err)
	}
	result := onlyResult(t, dispatch)
	assertGolden(t, "binary_png_hexdump.txt", result.Output)
}

func TestReadBinaryModeSupportsByteOffsetAndLimit(t *testing.T) {
	dir := t.TempDir()
	png := mustReadFixture(t, filepath.Join("testdata", "read", "fixtures", "tiny.png"))
	if err := os.WriteFile(filepath.Join(dir, "tiny.png"), png, 0o644); err != nil {
		t.Fatalf("write png fixture: %v", err)
	}

	dispatch, err := NewReadTool(ReadConfig{CWD: dir}).ResolveTool(context.Background(), nil, threads.ToolCall{
		CallID:  "c1",
		Name:    "read",
		Payload: `{"path":"tiny.png?read_options={\"mode\":\"binary\",\"byteOffset\":16,\"byteLimit\":16}"}`,
	}, nil)
	if err != nil {
		t.Fatalf("ResolveTool: %v", err)
	}
	result := onlyResult(t, dispatch)
	assertGolden(t, "binary_png_offset_limit.txt", result.Output)
}

func TestReadBinaryModeLimitUsesContinuationHint(t *testing.T) {
	dir := t.TempDir()
	png := mustReadFixture(t, filepath.Join("testdata", "read", "fixtures", "tiny.png"))
	if err := os.WriteFile(filepath.Join(dir, "tiny.png"), png, 0o644); err != nil {
		t.Fatalf("write png fixture: %v", err)
	}

	dispatch, err := NewReadTool(ReadConfig{CWD: dir}).ResolveTool(context.Background(), nil, threads.ToolCall{
		CallID:  "c1",
		Name:    "read",
		Payload: `{"path":"tiny.png?read_options={\"mode\":\"binary\",\"byteLimit\":16}"}`,
	}, nil)
	if err != nil {
		t.Fatalf("ResolveTool: %v", err)
	}
	result := onlyResult(t, dispatch)
	assertGolden(t, "binary_png_limit_continue.txt", result.Output)
}

func TestReadBinaryModeEOFHasNoContinuationHint(t *testing.T) {
	dir := t.TempDir()
	png := mustReadFixture(t, filepath.Join("testdata", "read", "fixtures", "tiny.png"))
	if err := os.WriteFile(filepath.Join(dir, "tiny.png"), png, 0o644); err != nil {
		t.Fatalf("write png fixture: %v", err)
	}

	dispatch, err := NewReadTool(ReadConfig{CWD: dir}).ResolveTool(context.Background(), nil, threads.ToolCall{
		CallID:  "c1",
		Name:    "read",
		Payload: `{"path":"tiny.png?read_options={\"mode\":\"binary\",\"byteOffset\":64,\"byteLimit\":512}"}`,
	}, nil)
	if err != nil {
		t.Fatalf("ResolveTool: %v", err)
	}
	result := onlyResult(t, dispatch)
	assertGolden(t, "binary_png_eof.txt", result.Output)
	if strings.Contains(result.Output, "Continue with path") {
		t.Fatalf("EOF binary read should not include continuation hint:\n%s", result.Output)
	}
}

func TestReadBinaryModeIgnoresTextMaxBytesAndCapsLimit(t *testing.T) {
	dir := t.TempDir()
	png := mustReadFixture(t, filepath.Join("testdata", "read", "fixtures", "tiny.png"))
	if err := os.WriteFile(filepath.Join(dir, "tiny.png"), png, 0o644); err != nil {
		t.Fatalf("write png fixture: %v", err)
	}

	dispatch, err := NewReadTool(ReadConfig{CWD: dir, MaxBytes: 1}).ResolveTool(context.Background(), nil, threads.ToolCall{
		CallID:  "c1",
		Name:    "read",
		Payload: `{"path":"tiny.png?read_options={\"mode\":\"binary\",\"byteLimit\":999999}"}`,
	}, nil)
	if err != nil {
		t.Fatalf("ResolveTool: %v", err)
	}
	result := onlyResult(t, dispatch)
	if !strings.Contains(result.Output, "Showing bytes 0..75") {
		t.Fatalf("binary mode should ignore text MaxBytes and read whole tiny fixture, got:\n%s", result.Output)
	}
}

func TestReadTextByteWindowForLongLines(t *testing.T) {
	dir := t.TempDir()
	content := strings.Repeat("a", 20) + "\n" + strings.Repeat("b", 20) + "\n" + strings.Repeat("c", 20) + "\n"
	mustWriteFile(t, filepath.Join(dir, "long.txt"), content)

	dispatch, err := NewReadTool(ReadConfig{CWD: dir, MaxBytes: 16}).ResolveTool(context.Background(), nil, threads.ToolCall{
		CallID:  "c1",
		Name:    "read",
		Payload: `{"path":"long.txt?read_options={\"mode\":\"text\",\"byteOffset\":21,\"byteLimit\":10}"}`,
	}, nil)
	if err != nil {
		t.Fatalf("ResolveTool: %v", err)
	}
	assertGolden(t, "text_byte_window_long_lines.txt", onlyResult(t, dispatch).Output)
}

func TestReadTextByteWindowEOF(t *testing.T) {
	dir := t.TempDir()
	content := strings.Repeat("a", 20) + "\n" + strings.Repeat("b", 20) + "\n" + strings.Repeat("c", 20) + "\n"
	mustWriteFile(t, filepath.Join(dir, "long.txt"), content)

	dispatch, err := NewReadTool(ReadConfig{CWD: dir, MaxBytes: 16}).ResolveTool(context.Background(), nil, threads.ToolCall{
		CallID:  "c1",
		Name:    "read",
		Payload: `{"path":"long.txt?read_options={\"mode\":\"text\",\"byteOffset\":42,\"byteLimit\":100}"}`,
	}, nil)
	if err != nil {
		t.Fatalf("ResolveTool: %v", err)
	}
	result := onlyResult(t, dispatch)
	assertGolden(t, "text_byte_window_eof.txt", result.Output)
	if strings.Contains(result.Output, "Continue with path") {
		t.Fatalf("EOF text byte window should not include continuation hint:\n%s", result.Output)
	}
}

func TestReadTextByteWindowRejectsInvalidUTF8(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bin.dat"), []byte{0x89, 'P', 'N', 'G', '\n'}, 0o644); err != nil {
		t.Fatalf("write binary fixture: %v", err)
	}

	dispatch, err := NewReadTool(ReadConfig{CWD: dir}).ResolveTool(context.Background(), nil, threads.ToolCall{
		CallID:  "c1",
		Name:    "read",
		Payload: `{"path":"bin.dat?read_options={\"mode\":\"text\",\"byteOffset\":1,\"byteLimit\":3}"}`,
	}, nil)
	if err != nil {
		t.Fatalf("ResolveTool: %v", err)
	}
	result := onlyResult(t, dispatch)
	if !strings.Contains(result.Output, "read: bin.dat is not valid UTF-8 text") ||
		!strings.Contains(result.Output, `?read_options={\"mode\":\"binary\"`) {
		t.Fatalf("expected invalid UTF-8 notice with binary follow-up, got:\n%s", result.Output)
	}
}

func TestReadInvalidReadOptionsSuffixIsLiteralPath(t *testing.T) {
	dir := t.TempDir()
	name := `literal?read_options={"mode":"text","unknown":true}`
	mustWriteFile(t, filepath.Join(dir, name), "literal\n")

	dispatch, err := NewReadTool(ReadConfig{CWD: dir}).ResolveTool(context.Background(), nil, threads.ToolCall{
		CallID:  "c1",
		Name:    "read",
		Payload: `{"path":` + jsonString(name) + `}`,
	}, nil)
	if err != nil {
		t.Fatalf("ResolveTool: %v", err)
	}
	assertTextResult(t, dispatch, "literal\n")
}

func TestReadDefaultMaxBytesPreventsHugeFilesByDefault(t *testing.T) {
	dir := t.TempDir()
	content := strings.Repeat("x", defaultReadMaxBytes+1)
	mustWriteFile(t, filepath.Join(dir, "huge.txt"), content)

	dispatch, err := NewReadTool(ReadConfig{CWD: dir}).ResolveTool(context.Background(), nil, threads.ToolCall{
		CallID:  "c1",
		Name:    "read",
		Payload: `{"path":"huge.txt"}`,
	}, nil)
	if err != nil {
		t.Fatalf("ResolveTool: %v", err)
	}
	result := onlyResult(t, dispatch)
	if !strings.Contains(result.Output, "[Text byte window:") ||
		!strings.Contains(result.Output, `?read_options={\"mode\":\"text\",\"byteOffset\":`) {
		t.Fatalf("expected text byte-window fallback, got %q", result.Output)
	}
}

func TestReadNegativeMaxBytesDisablesDefaultByteCap(t *testing.T) {
	dir := t.TempDir()
	content := strings.Repeat("x", defaultReadMaxBytes+1)
	mustWriteFile(t, filepath.Join(dir, "huge.txt"), content)

	dispatch, err := NewReadTool(ReadConfig{CWD: dir, MaxBytes: -1}).ResolveTool(context.Background(), nil, threads.ToolCall{
		CallID:  "c1",
		Name:    "read",
		Payload: `{"path":"huge.txt"}`,
	}, nil)
	if err != nil {
		t.Fatalf("ResolveTool: %v", err)
	}
	assertTextResult(t, dispatch, content)
}

func TestReadPayloadDecodeErrorIsModelVisible(t *testing.T) {
	dispatch, err := NewReadTool(ReadConfig{}).ResolveTool(context.Background(), nil, threads.ToolCall{
		CallID:  "c1",
		Name:    "read",
		Payload: `{`,
	}, nil)
	if err != nil {
		t.Fatalf("ResolveTool: %v", err)
	}
	result := onlyResult(t, dispatch)
	if result.Data["error"] == nil {
		t.Fatalf("expected model-visible error data, got %#v", result.Data)
	}
	if !strings.Contains(result.Output, `tool "read" payload`) {
		t.Fatalf("unexpected decode error output: %q", result.Output)
	}
}

func TestReadWrongToolNameIsInfrastructureError(t *testing.T) {
	_, err := NewReadTool(ReadConfig{}).ResolveTool(context.Background(), nil, threads.ToolCall{
		CallID: "c1",
		Name:   "write",
	}, nil)
	if err == nil {
		t.Fatal("expected missing tool infrastructure error")
	}
}

func mustWriteFile(t *testing.T, path string, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create parent directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write fixture %s: %v", path, err)
	}
}

func mustReadFixture(t *testing.T, path string) []byte {
	t.Helper()
	buf, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	return buf
}

func jsonString(s string) string {
	buf, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(buf)
}

func onlyResult(t *testing.T, dispatch threads.ToolDispatch) threads.ToolCallResult {
	t.Helper()
	if !dispatch.Started {
		t.Fatalf("dispatch not marked started: %#v", dispatch)
	}
	if got := len(dispatch.Items); got != 1 {
		t.Fatalf("expected one dispatch item, got %d: %#v", got, dispatch.Items)
	}
	result, ok := dispatch.Items[0].(threads.ToolCallResult)
	if !ok {
		t.Fatalf("expected ToolCallResult, got %T", dispatch.Items[0])
	}
	if result.CallID != "c1" {
		t.Fatalf("result CallID = %q, want c1", result.CallID)
	}
	return result
}

func assertTextResult(t *testing.T, dispatch threads.ToolDispatch, want string) {
	t.Helper()
	result := onlyResult(t, dispatch)
	if result.Output != want {
		t.Fatalf("unexpected output:\n got: %q\nwant: %q", result.Output, want)
	}
	if result.Data != nil {
		t.Fatalf("text result should not have data: %#v", result.Data)
	}
}

func assertGolden(t *testing.T, name string, got string) {
	t.Helper()
	path := filepath.Join("testdata", "read", "golden", name)
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v", path, err)
	}
	wantText := strings.TrimSuffix(string(want), "\n")
	if strings.HasSuffix(got, "\n") {
		wantText += "\n"
	}
	if got != wantText {
		t.Fatalf("golden mismatch for %s:\n--- got ---\n%s\n--- want ---\n%s", name, got, wantText)
	}
}

type readTestStreamer struct {
	calls int
	call threads.ToolCall
}

func (s *readTestStreamer) Capabilities() threads.StreamerCapabilities {
	return threads.StreamerCapabilities{AssistantPrefix: true}
}

func (s *readTestStreamer) RegisterToolNormalizer(string, threads.ToolNormalizer) {}

func (s *readTestStreamer) UnregisterToolNormalizer(string) {}

func (s *readTestStreamer) StreamReq(_ threads.Req, emit func(threads.Item) error) error {
	s.calls++
	if s.calls == 1 {
		return emit(s.call)
	}
	return emit(threads.AssistantText("done"))
}
