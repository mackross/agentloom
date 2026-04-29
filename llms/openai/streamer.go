package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	gschema "github.com/google/jsonschema-go/jsonschema"
	openaiapi "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"

	"github.com/mackross/agentloom/threads"
)

const DefaultModel = "gpt-4.1-mini"

type ResponsesStreamer struct {
	client            openaiapi.Client
	model             string
	Reasoning         shared.ReasoningParam
	ServiceTier       responses.ResponseNewParamsServiceTier
	OnOutputTextDelta func(string)
}

type functionCallMeta struct {
	callID string
	name   string
}

func NewResponsesStreamer(model string) *ResponsesStreamer {
	return NewResponsesStreamerWithClient(openaiapi.NewClient(), model)
}

func NewResponsesStreamerWithClient(client openaiapi.Client, model string) *ResponsesStreamer {
	model = strings.TrimSpace(model)
	if model == "" {
		model = DefaultModel
	}
	return &ResponsesStreamer{client: client, model: model}
}

func (*ResponsesStreamer) Capabilities() threads.StreamerCapabilities {
	return threads.StreamerCapabilities{AssistantPrefix: true, ToolResultSendPolicy: threads.ToolResultSendRequiresComplete}
}

func (s *ResponsesStreamer) StreamReq(req threads.Req, emit func(threads.Item) error) error {
	return s.StreamReqContext(context.Background(), req, emit)
}

func (s *ResponsesStreamer) StreamReqContext(ctx context.Context, req threads.Req, emit func(threads.Item) error) error {
	inputItems, err := requestInputItems(req)
	if err != nil {
		return err
	}

	params := responses.ResponseNewParams{
		Model:     s.model,
		Input:     responses.ResponseNewParamsInputUnion{OfInputItemList: inputItems},
		Reasoning: s.Reasoning,
		Store:     openaiapi.Bool(false),
	}
	if s.ServiceTier != "" {
		params.ServiceTier = s.ServiceTier
	}
	if req.Instruction != "" {
		params.Instructions = openaiapi.String(req.Instruction)
	}
	tools, err := requestTools(req.Tools)
	if err != nil {
		return err
	}
	if len(tools) > 0 {
		params.Tools = tools
	}
	choice, err := requestToolChoice(req.Tools)
	if err != nil {
		return err
	}
	if choice != nil {
		params.ToolChoice = *choice
	}
	if req.Tools.Parallel != nil {
		params.ParallelToolCalls = openaiapi.Bool(*req.Tools.Parallel)
	}

	stream := s.client.Responses.NewStreaming(ctx, params)
	defer stream.Close()
	functionCalls := map[string]functionCallMeta{}

	for stream.Next() {
		event := stream.Current()
		switch event.Type {
		case "response.output_text.delta":
			if event.Delta == "" {
				continue
			}
			if s.OnOutputTextDelta != nil {
				s.OnOutputTextDelta(event.Delta)
			}
			if err := emit(threads.AssistantText(event.Delta)); err != nil {
				return err
			}
		case "response.function_call_arguments.delta":
			if event.Delta == "" {
				continue
			}
			callID, name := resolveFunctionCall(functionCalls, event.ItemID, "")
			if err := emit(threads.ToolCallChunk{CallID: callID, Name: name, PayloadDelta: event.Delta}); err != nil {
				return err
			}
		case "response.function_call_arguments.done":
			callID, name := resolveFunctionCall(functionCalls, event.ItemID, event.Name)
			if err := emit(threads.ToolCall{CallID: callID, Name: name, Payload: event.Arguments}); err != nil {
				return err
			}
		case "response.output_item.added", "response.output_item.done":
			rememberFunctionCall(functionCalls, event.Item)
		case "error":
			if event.Message != "" {
				return fmt.Errorf("openai responses stream error: %s", event.Message)
			}
			return fmt.Errorf("openai responses stream error")
		}
	}

	if err := stream.Err(); err != nil {
		return err
	}
	return nil
}

func rememberFunctionCall(functionCalls map[string]functionCallMeta, item responses.ResponseOutputItemUnion) {
	if item.Type != "function_call" || item.ID == "" {
		return
	}
	meta := functionCalls[item.ID]
	if item.CallID != "" {
		meta.callID = item.CallID
	}
	if item.Name != "" {
		meta.name = item.Name
	}
	functionCalls[item.ID] = meta
}

func resolveFunctionCall(functionCalls map[string]functionCallMeta, itemID, fallbackName string) (callID, name string) {
	callID, name = itemID, fallbackName
	if meta, ok := functionCalls[itemID]; ok {
		if meta.callID != "" {
			callID = meta.callID
		}
		if name == "" && meta.name != "" {
			name = meta.name
		}
	}
	return callID, name
}

func requestInputItems(req threads.Req) (responses.ResponseInputParam, error) {
	inputItems := make(responses.ResponseInputParam, 0, len(req.Items))
	for _, it := range req.Items {
		switch v := it.(type) {
		case threads.UserText:
			inputItems = append(inputItems, responses.ResponseInputItemParamOfMessage(string(v), responses.EasyInputMessageRoleUser))
		case threads.AssistantText:
			inputItems = append(inputItems, responses.ResponseInputItemParamOfMessage(string(v), responses.EasyInputMessageRoleAssistant))
		case threads.ToolCall:
			inputItems = append(inputItems, responses.ResponseInputItemParamOfFunctionCall(v.Payload, v.CallID, v.Name))
		case threads.ToolCallResultable:
			inputItems = append(inputItems, responses.ResponseInputItemParamOfFunctionCallOutput(v.ToolCallID(), v.ToolOutput()))
		default:
			return nil, fmt.Errorf("openai request item not supported: %T", it)
		}
	}
	return inputItems, nil
}

func requestTools(snap threads.ToolOfferSnapshot) ([]responses.ToolUnionParam, error) {
	if len(snap.Offered) == 0 {
		return nil, nil
	}
	out := make([]responses.ToolUnionParam, 0, len(snap.Offered))
	for _, spec := range snap.Offered {
		if spec.Payload == nil {
			return nil, fmt.Errorf("openai tool %q payload not supported: %T", spec.Name, spec.Payload)
		}
		switch p := spec.Payload.(type) {
		case threads.ToolPayloadJSONSchema:
			params := map[string]any{}
			b, err := json.Marshal(gschema.Schema(p))
			if err != nil {
				return nil, fmt.Errorf("openai tool %q schema: %w", spec.Name, err)
			}
			if err := json.Unmarshal(b, &params); err != nil {
				return nil, fmt.Errorf("openai tool %q schema: %w", spec.Name, err)
			}
			closeObjectSchemas(params)
			f := &responses.FunctionToolParam{Name: spec.Name, Parameters: params, Strict: openaiapi.Bool(true)}
			if spec.Description != "" {
				f.Description = openaiapi.String(spec.Description)
			}
			out = append(out, responses.ToolUnionParam{OfFunction: f})
		case threads.ToolPayloadRegexp:
			c := &responses.CustomToolParam{Name: spec.Name, Format: shared.CustomToolInputFormatParamOfGrammar(string(p), "regex")}
			if spec.Description != "" {
				c.Description = openaiapi.String(spec.Description)
			}
			out = append(out, responses.ToolUnionParam{OfCustom: c})
		case threads.ToolPayloadLark:
			c := &responses.CustomToolParam{Name: spec.Name, Format: shared.CustomToolInputFormatParamOfGrammar(string(p), "lark")}
			if spec.Description != "" {
				c.Description = openaiapi.String(spec.Description)
			}
			out = append(out, responses.ToolUnionParam{OfCustom: c})
		default:
			c := &responses.CustomToolParam{Name: spec.Name}
			if spec.Description != "" {
				c.Description = openaiapi.String(spec.Description)
			}
			out = append(out, responses.ToolUnionParam{OfCustom: c})
		}
	}
	return out, nil
}

func closeObjectSchemas(v any) {
	switch x := v.(type) {
	case map[string]any:
		if x["type"] == "object" {
			if _, ok := x["additionalProperties"]; !ok {
				x["additionalProperties"] = false
			}
		}
		for _, child := range x {
			closeObjectSchemas(child)
		}
	case []any:
		for _, child := range x {
			closeObjectSchemas(child)
		}
	}
}

func requestToolChoice(snap threads.ToolOfferSnapshot) (*responses.ResponseNewParamsToolChoiceUnion, error) {
	if snap.Allowed == nil {
		return nil, nil
	}
	if len(snap.Allowed) > 0 {
		tools, err := requestAllowedTools(snap)
		if err != nil {
			return nil, err
		}
		return &responses.ResponseNewParamsToolChoiceUnion{OfAllowedTools: &responses.ToolChoiceAllowedParam{Mode: responses.ToolChoiceAllowedModeAuto, Tools: tools}}, nil
	}
	opt := param.NewOpt(responses.ToolChoiceOptionsNone)
	return &responses.ResponseNewParamsToolChoiceUnion{OfToolChoiceMode: opt}, nil
}

func requestAllowedTools(snap threads.ToolOfferSnapshot) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(snap.Allowed))
	for _, name := range snap.Allowed {
		kind, err := requestToolKind(snap, name)
		if err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"type": kind, "name": name})
	}
	return out, nil
}

func requestToolKind(snap threads.ToolOfferSnapshot, name string) (string, error) {
	for _, spec := range snap.Offered {
		if spec.Name != name {
			continue
		}
		if spec.Payload == nil {
			return "", fmt.Errorf("openai tool %q payload not supported: %T", name, spec.Payload)
		}
		switch spec.Payload.(type) {
		case threads.ToolPayloadJSONSchema:
			return "function", nil
		case threads.ToolPayloadRegexp, threads.ToolPayloadLark:
			return "custom", nil
		default:
			return "custom", nil
		}
	}
	return "", fmt.Errorf("openai tool %q not offered", name)
}
