package openai

import (
	"encoding/json"
	"testing"

	gschema "github.com/google/jsonschema-go/jsonschema"
	openaiapi "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"

	"github.com/mackross/agentloom/threads"
)

func TestRequestToolsMapsFunctionAndCustomPayloads(t *testing.T) {
	snap := threads.ToolOfferSnapshot{Offered: []threads.ToolSpec{
		{Name: "sum", Description: "Add numbers", Payload: threads.ToolPayloadJSONSchema(gschema.Schema{Type: "object"})},
		{Name: "free", Description: "Free text", Payload: threads.ToolPayloadText()},
		{Name: "regex", Description: "Regex input", Payload: threads.ToolPayloadRegexp("^[a-z]+$")},
	}}

	tools, err := requestTools(snap)
	if err != nil {
		t.Fatalf("request tools: %v", err)
	}
	if len(tools) != 3 {
		t.Fatalf("expected 3 tools, got %#v", tools)
	}
	if got := tools[0].OfFunction; got == nil || got.Name != "sum" || got.Parameters["type"] != "object" {
		t.Fatalf("unexpected function tool: %#v", tools[0])
	}
	if got := tools[1].OfCustom; got == nil || got.Name != "free" || got.Format.GetType() != nil {
		t.Fatalf("unexpected text custom tool: %#v", tools[1])
	}
	if got := tools[2].OfCustom; got == nil || got.Name != "regex" || valueOrEmpty(got.Format.GetSyntax()) != "regex" || valueOrEmpty(got.Format.GetDefinition()) != "^[a-z]+$" {
		t.Fatalf("unexpected grammar custom tool: %#v", tools[2])
	}
}

func TestRequestToolsAddsAdditionalPropertiesFalseWhenMissing(t *testing.T) {
	snap := threads.ToolOfferSnapshot{Offered: []threads.ToolSpec{{
		Name: "sum",
		Payload: threads.ToolPayloadJSONSchema(gschema.Schema{
			Type: "object",
			Properties: map[string]*gschema.Schema{
				"a": {Type: "integer"},
				"b": {Type: "integer"},
			},
			Required: []string{"a", "b"},
		}),
	}}}

	tools, err := requestTools(snap)
	if err != nil {
		t.Fatalf("request tools: %v", err)
	}
	if got := tools[0].OfFunction.Parameters["additionalProperties"]; got != false {
		t.Fatalf("expected additionalProperties=false, got %#v", got)
	}
}

func TestRequestInputItemsMapsToolCallsAndResults(t *testing.T) {
	items, err := requestInputItems(threads.Req{Items: []threads.Item{
		threads.UserText("hello"),
		threads.AssistantText("thinking"),
		threads.ToolCall{CallID: "c1", Name: "calculator", Payload: `{"a":19,"b":23}`},
		threads.ToolCallResult{CallID: "c1", Output: "42"},
	}})
	if err != nil {
		t.Fatalf("request input items: %v", err)
	}
	if len(items) != 4 {
		t.Fatalf("expected 4 items, got %#v", items)
	}
	if items[2].OfFunctionCall == nil {
		t.Fatalf("unexpected tool call input item: %#v", items[2])
	}
	if got := items[2].OfFunctionCall.CallID; got != "c1" {
		t.Fatalf("unexpected tool call id: %#v", items[2])
	}
	if got := items[2].OfFunctionCall.Name; got != "calculator" {
		t.Fatalf("unexpected tool call name: %#v", items[2])
	}
	if got := items[2].OfFunctionCall.Arguments; got != `{"a":19,"b":23}` {
		t.Fatalf("unexpected tool call args: %#v", items[2])
	}
	if items[3].OfFunctionCallOutput == nil {
		t.Fatalf("unexpected tool result input item: %#v", items[3])
	}
	if got := items[3].OfFunctionCallOutput.CallID; got != "c1" {
		t.Fatalf("unexpected tool result call id: %#v", items[3])
	}
	buf, err := json.Marshal(items[3].OfFunctionCallOutput)
	if err != nil {
		t.Fatalf("marshal tool result input item: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf, &got); err != nil {
		t.Fatalf("unmarshal tool result input item: %v", err)
	}
	if got["call_id"] != "c1" || got["output"] != "42" || got["type"] != "function_call_output" {
		t.Fatalf("unexpected tool result json: %#v", got)
	}
}

func TestResolveFunctionCallUsesOutputItemMetadata(t *testing.T) {
	functionCalls := map[string]functionCallMeta{}

	rememberFunctionCall(functionCalls, responses.ResponseOutputItemUnion{
		Type:   "function_call",
		ID:     "fc_1",
		CallID: "call_1",
		Name:   "calculator",
	})

	callID, name := resolveFunctionCall(functionCalls, "fc_1", "")
	if callID != "call_1" || name != "calculator" {
		t.Fatalf("unexpected resolved function call metadata: callID=%q name=%q", callID, name)
	}
}

func TestResponsesStreamerReportsAssistantPrefixCapability(t *testing.T) {
	streamer := NewResponsesStreamerWithClient(openaiapi.Client{}, "")

	if got := streamer.Capabilities(); !got.AssistantPrefix || got.ToolResultSendPolicy != threads.ToolResultSendRequiresComplete {
		t.Fatalf("expected assistant-prefix capability, got %#v", got)
	}
}

func valueOrEmpty(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
