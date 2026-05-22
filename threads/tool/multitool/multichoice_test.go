package multitool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/tool"
)

func TestGoldenMultipleChoiceYesNo(t *testing.T) {
	mt := New(Setup{
		Name:        "answer",
		Description: "Answer the multiple choice question.",
		Mode:        ModeLark,
		Required:    true,
	}, MultipleChoice("Is the sky blue on a clear day?").
		Choice("yes", "the answer is yes").
		Choice("no", "the answer is no").
		Handle(func(_ context.Context, _ *threads.Thread, answer string, ret tool.ReturnItem) (tool.Handling, error) {
			return tool.Handling{Continue: threads.ToolContinueManual}, ret(threads.ToolCallResult{Output: "choice: " + answer})
		}).
		Config())
	for _, tc := range []goldenCase{
		{Name: "empty", File: "multichoice_yesno_empty.golden", Payload: ""},
		{Name: "yes", File: "multichoice_yesno_yes.golden", Payload: "yes"},
		{Name: "no", File: "multichoice_yesno_no.golden", Payload: "no"},
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

func TestMultipleChoiceStoreAnswer(t *testing.T) {
	var answer string
	mt := New(Setup{}, MultipleChoice("Pick one.").
		Choice("yes", "yes").
		Choice("no", "no").
		StoreAnswer(&answer).
		ReturnStatic("ok").
		Config())
	dispatch, err := mt.ResolveTool(context.Background(), nil, threads.ToolCall{CallID: "c1", Name: mt.Setup().Name, Payload: "no"}, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if answer != "no" {
		t.Fatalf("unexpected answer: %q", answer)
	}
	if got := dispatch.Items[0].(threads.ToolCallResult).Output; got != "ok" {
		t.Fatalf("unexpected output: %q", got)
	}
}

func TestMultipleChoiceReturnStatic(t *testing.T) {
	mt := New(Setup{}, MultipleChoice("Pick one.").
		Choice("yes", "yes").
		ReturnStatic("ok").
		Config())
	dispatch, err := mt.ResolveTool(context.Background(), nil, threads.ToolCall{CallID: "c1", Name: mt.Setup().Name, Payload: "yes"}, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	result := dispatch.Items[0].(threads.ToolCallResult)
	if result.CallID != "c1" || result.Output != "ok" {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestMultipleChoiceDefaultReturnsChoiceCommand(t *testing.T) {
	mt := New(Setup{}, MultipleChoiceConfig("Pick one.", []Choice{{Command: "yes"}}, nil))
	dispatch, err := mt.ResolveTool(context.Background(), nil, threads.ToolCall{CallID: "c1", Name: mt.Setup().Name, Payload: "yes"}, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := strings.TrimSpace(dispatch.Items[0].(threads.ToolCallResult).Output); got != "yes" {
		t.Fatalf("unexpected output: %q", got)
	}
	if dispatch.Items[0].(threads.ToolCallResult).CallID != "c1" {
		t.Fatalf("missing call id: %#v", dispatch.Items[0])
	}
	if dispatch.Continue != threads.ToolContinueManual {
		t.Fatalf("expected manual continuation, got %#v", dispatch.Continue)
	}
}
