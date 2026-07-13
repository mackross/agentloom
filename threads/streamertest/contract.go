package streamertest

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	gschema "github.com/google/jsonschema-go/jsonschema"

	"github.com/mackross/agentloom/threads"
)

type Capabilities struct {
	ToolCallChunks      bool
	AssistantTextChunks bool
	// ParallelToolCalls means the provider can emit multiple tool calls in one
	// response when Tools.Parallel is true. This does not set Tools.Allowed.
	ParallelToolCalls bool
	// AllowedTools means the provider accepts Tools.Allowed (e.g. OpenAI
	// Responses tool_choice.allowed_tools). Independent of ParallelToolCalls.
	AllowedTools bool
}

type Harness interface {
	Capabilities() Capabilities
	Stream(t testing.TB, req threads.Req, events []Event, emit func(threads.Item) error) (ObservedRequest, error)
}

type Event struct {
	Item threads.Item
	Err  string
}

type ObservedRequest struct {
	Instruction string
	Items       []ObservedInputItem
	Tools       []ObservedTool
	ToolChoice  ObservedToolChoice
	Parallel    *bool
}

type ObservedInputItem struct {
	Kind    string
	Text    string
	CallID  string
	Name    string
	Payload string
	Output  string
}

type ObservedTool struct {
	Kind        string
	Name        string
	Description string
	SchemaType  string
}

type ObservedToolChoice struct {
	Mode    string
	Allowed []ObservedAllowedTool
}

type ObservedAllowedTool struct {
	Kind string
	Name string
}

func Emit(item threads.Item) Event {
	return Event{Item: item}
}

func Fail(message string) Event {
	return Event{Err: message}
}

func RunContractTests(t *testing.T, h Harness) {
	t.Helper()

	t.Run("forwards_request_items_and_instruction", func(t *testing.T) {
		req := threads.Req{
			Instruction: "be concise",
			Items: []threads.Item{
				threads.UserText("hello"),
				threads.AssistantText("thinking"),
				threads.ToolCall{CallID: "c1", Name: "calculator", Payload: `{"a":19,"b":23}`},
				threads.ToolCallResult{CallID: "c1", Output: "42"},
			},
		}

		var emitted []threads.Item
		got, err := h.Stream(t, req, []Event{Emit(threads.AssistantText("done"))}, func(item threads.Item) error {
			emitted = append(emitted, item)
			return nil
		})
		if err != nil {
			t.Fatalf("stream req: %v", err)
		}

		want := ObservedRequest{
			Instruction: "be concise",
			Items: []ObservedInputItem{
				{Kind: "user_text", Text: "hello"},
				{Kind: "assistant_text", Text: "thinking"},
				{Kind: "tool_call", CallID: "c1", Name: "calculator", Payload: `{"a":19,"b":23}`},
				{Kind: "tool_result", CallID: "c1", Output: "42"},
			},
		}
		assertObservedRequest(t, got, want)
		if !reflect.DeepEqual(emitted, []threads.Item{threads.AssistantText("done")}) {
			t.Fatalf("unexpected emitted items: %#v", emitted)
		}
	})

	t.Run("forwards_tool_offer_and_allowed_choice", func(t *testing.T) {
		parallel := true
		req := threads.Req{
			Tools: threads.ToolOfferSnapshot{
				Offered: []threads.ToolSpec{{
					Name:        "sum",
					Description: "add numbers",
					Payload:     threads.ToolPayloadJSONSchema(gschema.Schema{Type: "object"}),
				}},
				Allowed:  []string{"sum"},
				Parallel: &parallel,
			},
		}

		got, err := h.Stream(t, req, nil, func(threads.Item) error { return nil })
		if err != nil {
			t.Fatalf("stream req: %v", err)
		}

		want := ObservedRequest{
			Tools: []ObservedTool{{
				Kind:        "function",
				Name:        "sum",
				Description: "add numbers",
				SchemaType:  "object",
			}},
			ToolChoice: ObservedToolChoice{
				Mode: "allowed",
				Allowed: []ObservedAllowedTool{{
					Kind: "function",
					Name: "sum",
				}},
			},
			Parallel: boolRef(true),
		}
		assertObservedRequest(t, got, want)
	})

	t.Run("forwards_required_single_tool_choice", func(t *testing.T) {
		parallel := false
		req := threads.Req{
			Tools: threads.ToolOfferSnapshot{
				Offered: []threads.ToolSpec{{
					Name:        "submit",
					Description: "submit structured data",
					Payload:     threads.ToolPayloadJSONSchema(gschema.Schema{Type: "object"}),
				}},
				Allowed:  []string{"submit"},
				Parallel: &parallel,
				Required: true,
			},
		}

		got, err := h.Stream(t, req, nil, func(threads.Item) error { return nil })
		if err != nil {
			t.Fatalf("stream req: %v", err)
		}

		want := ObservedRequest{
			Tools: []ObservedTool{{
				Kind:        "function",
				Name:        "submit",
				Description: "submit structured data",
				SchemaType:  "object",
			}},
			ToolChoice: ObservedToolChoice{
				Mode: "required",
				Allowed: []ObservedAllowedTool{{
					Kind: "function",
					Name: "submit",
				}},
			},
			Parallel: boolRef(false),
		}
		assertObservedRequest(t, got, want)
	})

	t.Run("forwards_explicit_disable_all_tools", func(t *testing.T) {
		parallel := false
		req := threads.Req{
			Tools: threads.ToolOfferSnapshot{
				Offered: []threads.ToolSpec{{
					Name:        "sum",
					Description: "add numbers",
					Payload:     threads.ToolPayloadJSONSchema(gschema.Schema{Type: "object"}),
				}},
				Allowed:  []string{},
				Parallel: &parallel,
			},
		}

		got, err := h.Stream(t, req, nil, func(threads.Item) error { return nil })
		if err != nil {
			t.Fatalf("stream req: %v", err)
		}

		want := ObservedRequest{
			Tools: []ObservedTool{{
				Kind:        "function",
				Name:        "sum",
				Description: "add numbers",
				SchemaType:  "object",
			}},
			ToolChoice: ObservedToolChoice{Mode: "none"},
		}
		assertObservedRequest(t, got, want)
		if got.Parallel != nil && !sameBoolPtr(got.Parallel, boolRef(false)) {
			t.Fatalf("unexpected parallel flag when tools are disabled: got=%v want=false", boolPtrString(got.Parallel))
		}
	})

	t.Run("omits_tool_choice_when_allowed_is_unset", func(t *testing.T) {
		req := threads.Req{
			Tools: threads.ToolOfferSnapshot{
				Offered: []threads.ToolSpec{{
					Name:    "sum",
					Payload: threads.ToolPayloadJSONSchema(gschema.Schema{Type: "object"}),
				}},
			},
		}

		got, err := h.Stream(t, req, nil, func(threads.Item) error { return nil })
		if err != nil {
			t.Fatalf("stream req: %v", err)
		}

		want := ObservedRequest{
			Tools: []ObservedTool{{
				Kind:       "function",
				Name:       "sum",
				SchemaType: "object",
			}},
		}
		assertObservedRequest(t, got, want)
		if !reflect.DeepEqual(got.ToolChoice, ObservedToolChoice{}) {
			t.Fatalf("expected tool choice to be omitted, got %#v", got.ToolChoice)
		}
		if got.Parallel != nil {
			t.Fatalf("expected parallel flag to be omitted, got %v", boolPtrString(got.Parallel))
		}
	})

	t.Run("emits_assistant_text", func(t *testing.T) {
		req := threads.Req{Items: []threads.Item{threads.UserText("hello")}}
		events := []Event{
			Emit(threads.AssistantText("hel")),
			Emit(threads.AssistantText("lo")),
		}

		var emitted []threads.Item
		_, err := h.Stream(t, req, events, func(item threads.Item) error {
			emitted = append(emitted, item)
			return nil
		})
		if err != nil {
			t.Fatalf("stream req: %v", err)
		}
		if !reflect.DeepEqual(emitted, []threads.Item{
			threads.AssistantText("hel"),
			threads.AssistantText("lo"),
		}) {
			t.Fatalf("unexpected emitted items: %#v", emitted)
		}
	})

	t.Run("emits_multiple_tool_calls_in_one_stream", func(t *testing.T) {
		req := threads.Req{Items: []threads.Item{threads.UserText("hello")}}
		events := []Event{
			Emit(threads.ToolCall{CallID: "c1", Name: "sum", Payload: `{"a":1}`}),
			Emit(threads.ToolCall{CallID: "c2", Name: "sum", Payload: `{"a":2}`}),
		}

		var emitted []threads.Item
		_, err := h.Stream(t, req, events, func(item threads.Item) error {
			emitted = append(emitted, item)
			return nil
		})
		if err != nil {
			t.Fatalf("stream req: %v", err)
		}
		if !reflect.DeepEqual(emitted, []threads.Item{
			threads.ToolCall{CallID: "c1", Name: "sum", Payload: `{"a":1}`},
			threads.ToolCall{CallID: "c2", Name: "sum", Payload: `{"a":2}`},
		}) {
			t.Fatalf("unexpected emitted items: %#v", emitted)
		}
	})

	if h.Capabilities().ToolCallChunks {
		t.Run("emits_tool_call_chunks_before_final_call", func(t *testing.T) {
			req := threads.Req{Items: []threads.Item{threads.UserText("hello")}}
			events := []Event{
				Emit(threads.ToolCallChunk{CallID: "c1", Name: "sum", PayloadDelta: `{"a":`}),
				Emit(threads.ToolCallChunk{CallID: "c1", Name: "sum", PayloadDelta: `1}`}),
				Emit(threads.ToolCall{CallID: "c1", Name: "sum", Payload: `{"a":1}`}),
			}

			var emitted []threads.Item
			_, err := h.Stream(t, req, events, func(item threads.Item) error {
				emitted = append(emitted, item)
				return nil
			})
			if err != nil {
				t.Fatalf("stream req: %v", err)
			}
			if len(emitted) != 3 {
				t.Fatalf("expected 3 emitted items, got %#v", emitted)
			}

			first, ok := emitted[0].(threads.ToolCallChunk)
			if !ok {
				t.Fatalf("expected first item to be ToolCallChunk, got %T", emitted[0])
			}
			if first.CallID != "c1" || first.Name != "sum" || first.PayloadDelta != `{"a":` {
				t.Fatalf("unexpected first tool chunk: %#v", first)
			}

			second, ok := emitted[1].(threads.ToolCallChunk)
			if !ok {
				t.Fatalf("expected second item to be ToolCallChunk, got %T", emitted[1])
			}
			if second.CallID != "c1" || second.PayloadDelta != `1}` {
				t.Fatalf("unexpected second tool chunk: %#v", second)
			}
			if second.Name != "" && second.Name != "sum" {
				t.Fatalf("unexpected second tool chunk name: %#v", second)
			}

			if got := emitted[2]; got != (threads.ToolCall{CallID: "c1", Name: "sum", Payload: `{"a":1}`}) {
				t.Fatalf("unexpected final tool call: %#v", got)
			}
		})
	}

	t.Run("propagates_backend_error", func(t *testing.T) {
		req := threads.Req{Items: []threads.Item{threads.UserText("hello")}}
		_, err := h.Stream(t, req, []Event{Fail("boom")}, func(threads.Item) error { return nil })
		if err == nil {
			t.Fatal("expected stream error")
		}
		if !strings.Contains(err.Error(), "boom") {
			t.Fatalf("expected error to mention boom, got %v", err)
		}
	})

	t.Run("propagates_emit_error", func(t *testing.T) {
		req := threads.Req{Items: []threads.Item{threads.UserText("hello")}}
		sentinel := errors.New("emit failed")
		_, err := h.Stream(t, req, []Event{Emit(threads.AssistantText("x"))}, func(threads.Item) error {
			return sentinel
		})
		if !errors.Is(err, sentinel) {
			t.Fatalf("expected emit error, got %v", err)
		}
	})
}

func assertObservedRequest(t *testing.T, got, want ObservedRequest) {
	t.Helper()
	if want.Instruction != "" && got.Instruction != want.Instruction {
		t.Fatalf("unexpected instruction: got=%q want=%q", got.Instruction, want.Instruction)
	}
	if want.Items != nil && !reflect.DeepEqual(got.Items, want.Items) {
		t.Fatalf("unexpected request items:\n got=%#v\nwant=%#v", got.Items, want.Items)
	}
	if want.Tools != nil && !reflect.DeepEqual(got.Tools, want.Tools) {
		t.Fatalf("unexpected request tools:\n got=%#v\nwant=%#v", got.Tools, want.Tools)
	}
	if want.ToolChoice.Mode != "" || want.ToolChoice.Allowed != nil {
		if !reflect.DeepEqual(got.ToolChoice, want.ToolChoice) {
			t.Fatalf("unexpected tool choice:\n got=%#v\nwant=%#v", got.ToolChoice, want.ToolChoice)
		}
	}
	if want.Parallel != nil {
		if !sameBoolPtr(got.Parallel, want.Parallel) {
			t.Fatalf("unexpected parallel flag: got=%v want=%v", boolPtrString(got.Parallel), boolPtrString(want.Parallel))
		}
	}
}

func boolRef(v bool) *bool {
	return &v
}

func sameBoolPtr(left, right *bool) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func boolPtrString(v *bool) string {
	if v == nil {
		return "<nil>"
	}
	if *v {
		return "true"
	}
	return "false"
}
