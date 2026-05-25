package multitool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/tool"
)

func TestGoldenLark(t *testing.T) { testGolden(t, ModeLark, "lark") }
func TestGoldenJSON(t *testing.T) { testGolden(t, ModeJSON, "json") }

func TestGoldenJSONFailure(t *testing.T) {
	type args struct {
		Value string `json:"value" jsonschema:"value to echo"`
	}
	mt := New(Setup{
		Name:               "multi",
		Description:        "Stable multitool for tests.",
		Mode:               ModeLark,
		CommandDescription: "Command line for tests.",
		InputDescription:   "Input body for tests.",
	}, Config{Subtools: []Subtool{
		JSONHandler[args](SubtoolSpec{
			Command:     "echo-json",
			Description: "Echo a JSON value.",
			Usage:       "<json object>",
		}, "inner_echo", tool.HandlerFunc(func(context.Context, threads.Thread, tool.Call, tool.ReturnItem) (tool.Handling, error) {
			t.Fatal("handler should not run")
			return tool.Handling{}, nil
		})),
	}})

	got := renderGolden(t, mt, goldenCase{Name: "json failure", Payload: "echo-json\n\n{"})
	want, err := os.ReadFile(filepath.Join("testdata", "json_failure.golden"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if got != string(want) {
		t.Fatalf("golden mismatch for json_failure.golden\n--- got ---\n%s\n--- want ---\n%s", got, string(want))
	}
}

var _ tool.Handler = (*Tool)(nil)

func TestDefaultDescriptionDependsOnMode(t *testing.T) {
	lark := New(Setup{Mode: ModeLark}, Config{})
	if got := lark.Setup().Description; got != DefaultLarkDescription {
		t.Fatalf("lark default description = %q, want %q", got, DefaultLarkDescription)
	}

	implicitLark := New(Setup{}, Config{})
	if got := implicitLark.Setup().Description; got != DefaultLarkDescription {
		t.Fatalf("implicit lark default description = %q, want %q", got, DefaultLarkDescription)
	}

	jsonTool := New(Setup{Mode: ModeJSON}, Config{})
	if got := jsonTool.Setup().Description; got != DefaultDescription {
		t.Fatalf("json default description = %q, want %q", got, DefaultDescription)
	}
}

func TestSnapshotCanBeRequired(t *testing.T) {
	mt := New(Setup{Required: true}, Config{})
	snap := mt.Snapshot()
	if !snap.Required {
		t.Fatalf("expected required snapshot")
	}
}

func TestSubtoolHandlingPassesThrough(t *testing.T) {
	mt := New(Setup{}, Config{Subtools: []Subtool{
		Func(SubtoolSpec{Command: "safe"}, func(_ context.Context, _ threads.Thread, call ToolCall, ret tool.ReturnItem) (tool.Handling, error) {
			if err := ret(threads.ToolCallResult{CallID: call.CallID, Output: "ok"}); err != nil {
				return tool.Handling{}, err
			}
			return tool.Handling{Continue: threads.ToolContinueManual, Recovery: threads.ToolRecoverySafe}, nil
		}),
	}})
	dispatch, err := mt.ResolveTool(context.Background(), nil, threads.ToolCall{CallID: "c1", Name: mt.Setup().Name, Payload: "safe"}, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if dispatch.Continue != threads.ToolContinueManual || dispatch.Recovery != threads.ToolRecoverySafe {
		t.Fatalf("unexpected handling: %#v", dispatch)
	}
}

func TestJSONSubtoolValidatesAndBuildsThreadToolCall(t *testing.T) {
	type args struct {
		Value string `json:"value"`
	}
	mt := New(Setup{}, Config{Subtools: []Subtool{
		JSONHandler[args](SubtoolSpec{Command: "echo-json"}, "inner_echo", tool.HandlerFunc(func(_ context.Context, _ threads.Thread, inner tool.Call, ret tool.ReturnItem) (tool.Handling, error) {
			if inner.Name != "inner_echo" || inner.CallID != "c1" || inner.Payload != `{"value":"ok"}` {
				t.Fatalf("unexpected inner call: %#v", inner)
			}
			var got args
			if err := inner.UnmarshalJSON(&got); err != nil {
				t.Fatalf("unmarshal inner: %v", err)
			}
			return tool.Handling{}, ret(threads.ToolCallResult{CallID: inner.CallID, Output: got.Value})
		})),
	}})
	dispatch, err := mt.ResolveTool(context.Background(), nil, threads.ToolCall{CallID: "c1", Name: mt.Setup().Name, Payload: "echo-json\n\n{\"value\":\"ok\"}"}, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := dispatch.Items[0].(threads.ToolCallResult).Output; got != "ok" {
		t.Fatalf("unexpected output: %q", got)
	}
}

func TestJSONSubtoolReturnsUsefulParseError(t *testing.T) {
	type args struct {
		Value string `json:"value" jsonschema:"value to echo"`
	}
	mt := New(Setup{}, Config{Subtools: []Subtool{
		JSONHandler[args](SubtoolSpec{
			Command:     "echo-json",
			Description: "Echo a JSON value.",
			Usage:       "<json object>",
		}, "inner_echo", tool.HandlerFunc(func(context.Context, threads.Thread, tool.Call, tool.ReturnItem) (tool.Handling, error) {
			t.Fatal("handler should not run")
			return tool.Handling{}, nil
		})),
	}})
	dispatch, err := mt.ResolveTool(context.Background(), nil, threads.ToolCall{CallID: "c1", Name: mt.Setup().Name, Payload: "echo-json\n\n{"}, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	got := dispatch.Items[0].(threads.ToolCallResult).Output
	if !strings.Contains(got, `invalid JSON input for command "echo-json"`) {
		t.Fatalf("unexpected output: %q", got)
	}
	result := dispatch.Items[0].(threads.ToolCallResult)
	if result.SafeRollback == nil {
		t.Fatalf("expected rollbackable JSON parse error: %#v", result)
	}
	for _, want := range []string{
		`<tool_call_hint tool="tool" command="echo-json">`,
		`invalid JSON input for command "echo-json"`,
		`Echo a JSON value.`,
		`Usage: <json object>`,
		`"value to echo"`,
		`"value"`,
	} {
		if !strings.Contains(result.SafeRollback.SteeringHint, want) {
			t.Fatalf("steering hint missing %q:\n%s", want, result.SafeRollback.SteeringHint)
		}
	}
}

func TestJSONSubtoolHintDoesNotDuplicateSchemaFromDescriptionOrUsage(t *testing.T) {
	type args struct {
		Value string `json:"value"`
	}
	schema := jsonInputSchema[args]()
	for _, spec := range []SubtoolSpec{
		{Command: "echo-json", Description: "Use this schema:\n" + schema},
		{Command: "echo-json", Usage: "Use this schema:\n" + schema},
	} {
		hint := jsonInputErrorHint[args](ToolCall{Name: "multi", Command: &spec.Command}, spec, "invalid JSON")
		if got := strings.Count(hint, schema); got != 1 {
			t.Fatalf("schema appeared %d times in hint:\n%s", got, hint)
		}
	}
}

func testGolden(t *testing.T, mode Mode, prefix string) {
	t.Helper()
	mt := newGoldenTool(mode)
	for _, tc := range []goldenCase{
		{Name: "empty", File: prefix + "_empty.golden", Payload: emptyPayload(mode)},
		{Name: "known command", File: prefix + "_known.golden", Payload: payload(mode, "echo --upper 'two words'", "hello world")},
		{Name: "unknown command", File: prefix + "_unknown.golden", Payload: payload(mode, "missing --flag", "ignored")},
	} {
		t.Run(tc.Name, func(t *testing.T) {
			got := renderGolden(t, mt, tc)
			want, err := os.ReadFile(filepath.Join("testdata", tc.File))
			if err != nil {
				t.Fatalf("read golden: %v", err)
			}
			if got != string(want) {
				t.Fatalf("golden mismatch for %s\n--- got ---\n%s\n--- want ---\n%s", tc.File, got, string(want))
			}
		})
	}
}

type goldenCase struct {
	Name    string
	File    string
	Payload string
}

func newGoldenTool(mode Mode) *Tool {
	return New(Setup{
		Name:               "multi",
		Description:        "Stable multitool for tests.",
		Mode:               mode,
		CommandDescription: "Command line for tests.",
		InputDescription:   "Input body for tests.",
	}, Config{Subtools: []Subtool{
		Func(SubtoolSpec{
			Command:     "echo",
			Usage:       "[--upper] <label>",
			Description: "echoes parsed input",
		}, func(_ context.Context, _ threads.Thread, call ToolCall, ret tool.ReturnItem) (tool.Handling, error) {
			return tool.Handling{}, ret(threads.ToolCallResult{CallID: call.CallID, Output: stableCall(call.Call())})
		}),
		Func(SubtoolSpec{
			Command:     "count",
			Usage:       "",
			Description: "counts input bytes",
		}, func(_ context.Context, _ threads.Thread, call ToolCall, ret tool.ReturnItem) (tool.Handling, error) {
			input := ""
			if call.Input != nil {
				input = *call.Input
			}
			return tool.Handling{}, ret(threads.ToolCallResult{CallID: call.CallID, Output: fmt.Sprintf("bytes=%d", len(input))})
		}),
	}})
}

func renderGolden(t *testing.T, mt *Tool, tc goldenCase) string {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "SETUP: %#v\n", stableSetup(mt.Setup()))
	fmt.Fprintf(&b, "CONFIG: %#v\n", stableConfig(mt.Config()))
	snap := mt.ToolsSnapshot(nil)
	fmt.Fprintf(&b, "SNAPSHOT: offered=%d name=%q description=%q payload=%T required=%v handlers=%d\n", len(snap.Snapshot.Offered), snap.Snapshot.Offered[0].Name, snap.Snapshot.Offered[0].Description, snap.Snapshot.Offered[0].Payload, snap.Snapshot.Required, len(snap.Handlers))
	dispatch, err := mt.ResolveTool(context.Background(), nil, threads.ToolCall{CallID: "call-1", Name: mt.Setup().Name, Payload: tc.Payload}, nil)
	if err != nil {
		t.Fatalf("resolve %s: %v", tc.Name, err)
	}
	fmt.Fprintf(&b, "\nCASE %s\n", tc.Name)
	fmt.Fprintf(&b, "payload: %q\n", tc.Payload)
	fmt.Fprintf(&b, "started: %v items: %d\n", dispatch.Started, len(dispatch.Items))
	for _, item := range dispatch.Items {
		result := item.(threads.ToolCallResult)
		fmt.Fprintf(&b, "result call_id=%q:\n%s\n", result.CallID, result.Output)
		if result.SafeRollback != nil {
			fmt.Fprintf(&b, "safe_rollback: true\n")
			fmt.Fprintf(&b, "steering_hint:\n%s\n", result.SafeRollback.SteeringHint)
		}
	}
	return b.String()
}

type setupView struct {
	Name               string
	Description        string
	Mode               Mode
	CommandDescription string
	InputDescription   string
	Required           bool
}

func stableSetup(s Setup) setupView {
	return setupView(s)
}

type configView struct {
	Subtools []subtoolSpecView
	Fallback string
}

type subtoolSpecView struct {
	Command     string
	Description string
	Usage       string
}

func stableConfig(c Config) configView {
	out := configView{Subtools: make([]subtoolSpecView, 0, len(c.Subtools)), Fallback: "default"}
	if c.Fallback != nil {
		out.Fallback = "custom"
	}
	for _, st := range c.Subtools {
		spec := st.SubtoolSpec()
		out.Subtools = append(out.Subtools, subtoolSpecView{
			Command:     spec.Command,
			Description: spec.Description,
			Usage:       spec.Usage,
		})
	}
	return out
}

func stableCall(call Call) string {
	command := "<nil>"
	if call.Command != nil {
		command = fmt.Sprintf("%q", *call.Command)
	}
	input := "<nil>"
	if call.Input != nil {
		input = fmt.Sprintf("%q", *call.Input)
	}
	return fmt.Sprintf("command=%s args=%#v input=%s", command, call.Args, input)
}

func emptyPayload(mode Mode) string {
	if mode == ModeJSON {
		return `{}`
	}
	return ""
}

func payload(mode Mode, command, input string) string {
	if mode == ModeJSON {
		buf, err := json.Marshal(jsonPayload{Command: &command, Input: &input})
		if err != nil {
			panic(err)
		}
		return string(buf)
	}
	return command + "\n\n" + input
}
