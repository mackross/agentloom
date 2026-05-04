package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	anthropicapi "github.com/anthropics/anthropic-sdk-go"
	gschema "github.com/google/jsonschema-go/jsonschema"

	"github.com/mackross/agentloom/threads"
)

const (
	DefaultModel           = anthropicapi.ModelClaudeSonnet4_6
	DefaultMaxTokens int64 = 4096

	LatestFastModel   = anthropicapi.ModelClaudeHaiku4_5
	LatestLargeModel  = anthropicapi.ModelClaudeSonnet4_6
	LatestStrongModel = anthropicapi.ModelClaudeOpus4_6
)

type MessagesStreamer struct {
	client             anthropicapi.Client
	model              anthropicapi.Model
	MaxTokens          int64
	UseAutoCache       bool
	EagerToolStreaming bool
	ServiceTier        anthropicapi.MessageNewParamsServiceTier
	OnOutputTextDelta  func(string)
	normalizers        threads.ToolNormalizers
}

type toolCallMeta struct {
	callID string
	name   string
}

type messageRole string

const (
	messageRoleUser      messageRole = "user"
	messageRoleAssistant messageRole = "assistant"
)

func NewMessagesStreamer(model string) *MessagesStreamer {
	return NewMessagesStreamerWithClient(anthropicapi.NewClient(), model)
}

func NewMessagesStreamerWithClient(client anthropicapi.Client, model string) *MessagesStreamer {
	model = strings.TrimSpace(model)
	if model == "" {
		model = string(DefaultModel)
	}
	return &MessagesStreamer{
		client:             client,
		model:              anthropicapi.Model(model),
		MaxTokens:          DefaultMaxTokens,
		UseAutoCache:       true,
		EagerToolStreaming: true,
		ServiceTier:        anthropicapi.MessageNewParamsServiceTierAuto,
	}
}

func (s *MessagesStreamer) Capabilities() threads.StreamerCapabilities {
	return threads.StreamerCapabilities{AssistantPrefix: supportsAssistantPrefix(string(s.model)), ToolResultSendPolicy: threads.ToolResultSendRequiresComplete}
}

func (s *MessagesStreamer) RegisterToolNormalizer(name string, normalizer threads.ToolNormalizer) {
	s.normalizers.RegisterToolNormalizer(name, normalizer)
}

func (s *MessagesStreamer) UnregisterToolNormalizer(name string) {
	s.normalizers.UnregisterToolNormalizer(name)
}

func supportsAssistantPrefix(model string) bool {
	if strings.HasPrefix(model, "claude-sonnet-4-6") || strings.HasPrefix(model, "claude-opus-4-6") {
		return false
	}
	familyPrefix := "claude-"
	if !strings.HasPrefix(model, familyPrefix) {
		return true
	}
	parts := strings.Split(strings.TrimPrefix(model, familyPrefix), "-")
	if len(parts) < 3 || parts[1] != "4" {
		return true
	}
	minor, err := strconv.Atoi(parts[2])
	return err != nil || minor < 7
}

func (s *MessagesStreamer) StreamReq(req threads.Req, emit func(threads.Item) error) error {
	return s.StreamReqContext(context.Background(), req, emit)
}

func (s *MessagesStreamer) StreamReqContext(ctx context.Context, req threads.Req, emit func(threads.Item) error) error {
	req, err := s.normalizers.NormalizeReq(req)
	if err != nil {
		return err
	}

	messages, err := requestMessages(req)
	if err != nil {
		return err
	}

	params := anthropicapi.MessageNewParams{
		Model:       s.model,
		MaxTokens:   s.MaxTokens,
		Messages:    messages,
		ServiceTier: s.ServiceTier,
	}
	if req.Instruction != "" {
		params.System = []anthropicapi.TextBlockParam{{Text: req.Instruction}}
	}
	if s.UseAutoCache {
		params.CacheControl = anthropicapi.NewCacheControlEphemeralParam()
	}

	tools, err := requestTools(req.Tools, s.EagerToolStreaming)
	if err != nil {
		return err
	}
	if len(tools) > 0 {
		params.Tools = tools
	}

	choice := requestToolChoice(req.Tools, len(tools) > 0)
	if choice != nil {
		params.ToolChoice = *choice
	}

	stream := s.client.Messages.NewStreaming(ctx, params)
	defer stream.Close()

	acc := anthropicapi.Message{}
	toolCalls := map[int64]toolCallMeta{}

	for stream.Next() {
		event := stream.Current()
		if err := acc.Accumulate(event); err != nil {
			return err
		}

		switch v := event.AsAny().(type) {
		case anthropicapi.ContentBlockStartEvent:
			if block, ok := v.ContentBlock.AsAny().(anthropicapi.ToolUseBlock); ok {
				toolCalls[v.Index] = toolCallMeta{callID: block.ID, name: block.Name}
			}
		case anthropicapi.ContentBlockDeltaEvent:
			switch delta := v.Delta.AsAny().(type) {
			case anthropicapi.TextDelta:
				if delta.Text == "" {
					continue
				}
				if s.OnOutputTextDelta != nil {
					s.OnOutputTextDelta(delta.Text)
				}
				if err := emit(threads.AssistantText(delta.Text)); err != nil {
					return err
				}
			case anthropicapi.InputJSONDelta:
				if delta.PartialJSON == "" {
					continue
				}
				meta := toolCalls[v.Index]
				if err := emit(threads.ToolCallChunk{
					CallID:       meta.callID,
					Name:         meta.name,
					PayloadDelta: delta.PartialJSON,
				}); err != nil {
					return err
				}
			}
		case anthropicapi.ContentBlockStopEvent:
			meta, ok := toolCalls[v.Index]
			if !ok {
				continue
			}
			if v.Index < 0 || int(v.Index) >= len(acc.Content) {
				return fmt.Errorf("anthropic content block index out of range: %d", v.Index)
			}
			block, ok := acc.Content[v.Index].AsAny().(anthropicapi.ToolUseBlock)
			if !ok {
				return fmt.Errorf("anthropic content block %d is not a tool_use block", v.Index)
			}
			callID := block.ID
			if meta.callID != "" {
				callID = meta.callID
			}
			name := block.Name
			if name == "" {
				name = meta.name
			}
			call := threads.ToolCall{
				CallID:  callID,
				Name:    name,
				Payload: string(block.Input),
			}
			call, err := s.normalizers.NormalizeResponseToolCall(call)
			if err != nil {
				return err
			}
			if err := emit(call); err != nil {
				return err
			}
			delete(toolCalls, v.Index)
		}
	}

	if err := stream.Err(); err != nil {
		return err
	}
	return nil
}

type observedMessage struct {
	role   messageRole
	blocks []anthropicapi.ContentBlockParamUnion
}

func requestMessages(req threads.Req) ([]anthropicapi.MessageParam, error) {
	var grouped []observedMessage
	appendBlock := func(role messageRole, block anthropicapi.ContentBlockParamUnion) {
		if len(grouped) > 0 && grouped[len(grouped)-1].role == role {
			grouped[len(grouped)-1].blocks = append(grouped[len(grouped)-1].blocks, block)
			return
		}
		grouped = append(grouped, observedMessage{role: role, blocks: []anthropicapi.ContentBlockParamUnion{block}})
	}
	outstanding := []string{}
	results := map[string]anthropicapi.ContentBlockParamUnion{}
	deferredUser := []anthropicapi.ContentBlockParamUnion{}
	flushUser := func(force bool) {
		if len(outstanding) == 0 {
			for _, b := range deferredUser {
				appendBlock(messageRoleUser, b)
			}
			deferredUser = nil
			return
		}
		blocks := make([]anthropicapi.ContentBlockParamUnion, 0, len(outstanding)+len(deferredUser))
		for _, id := range outstanding {
			b, ok := results[id]
			if !ok && !force {
				return
			}
			if ok {
				blocks = append(blocks, b)
			}
		}
		blocks = append(blocks, deferredUser...)
		for _, b := range blocks {
			appendBlock(messageRoleUser, b)
		}
		outstanding, deferredUser = nil, nil
		results = map[string]anthropicapi.ContentBlockParamUnion{}
	}

	for _, item := range req.Items {
		switch v := item.(type) {
		case threads.UserText:
			b := anthropicapi.NewTextBlock(string(v))
			if len(outstanding) > 0 {
				deferredUser = append(deferredUser, b)
				continue
			}
			appendBlock(messageRoleUser, b)
		case threads.AssistantText:
			appendBlock(messageRoleAssistant, anthropicapi.NewTextBlock(string(v)))
		case threads.ToolCall:
			input, err := decodeToolInput(v.Payload)
			if err != nil {
				return nil, fmt.Errorf("anthropic tool call %q payload: %w", v.Name, err)
			}
			appendBlock(messageRoleAssistant, anthropicapi.NewToolUseBlock(v.CallID, input, v.Name))
			outstanding = append(outstanding, v.CallID)
		case threads.ToolCallResultable:
			b := anthropicapi.NewToolResultBlock(v.ToolCallID(), v.ToolOutput(), toolDataIsError(v.ToolData()))
			if len(outstanding) > 0 {
				results[v.ToolCallID()] = b
				flushUser(false)
				continue
			}
			appendBlock(messageRoleUser, b)
		default:
			return nil, fmt.Errorf("anthropic request item not supported: %T", item)
		}
	}
	flushUser(true)

	out := make([]anthropicapi.MessageParam, 0, len(grouped))
	for _, msg := range grouped {
		switch msg.role {
		case messageRoleUser:
			out = append(out, anthropicapi.NewUserMessage(msg.blocks...))
		case messageRoleAssistant:
			out = append(out, anthropicapi.NewAssistantMessage(msg.blocks...))
		default:
			return nil, fmt.Errorf("anthropic message role not supported: %q", msg.role)
		}
	}
	return out, nil
}

func decodeToolInput(payload string) (any, error) {
	payload = strings.TrimSpace(payload)
	if payload == "" {
		return map[string]any{}, nil
	}
	var input any
	if err := json.Unmarshal([]byte(payload), &input); err != nil {
		return nil, err
	}
	return input, nil
}

func toolDataIsError(data map[string]any) bool {
	if len(data) == 0 {
		return false
	}
	_, ok := data["error"]
	return ok
}

func requestTools(snap threads.ToolOfferSnapshot, eagerInputStreaming bool) ([]anthropicapi.ToolUnionParam, error) {
	specs, err := filteredTools(snap)
	if err != nil {
		return nil, err
	}
	if len(specs) == 0 {
		return nil, nil
	}

	out := make([]anthropicapi.ToolUnionParam, 0, len(specs))
	for _, spec := range specs {
		if spec.Payload == nil {
			return nil, fmt.Errorf("anthropic tool %q payload not supported: %T", spec.Name, spec.Payload)
		}
		switch p := spec.Payload.(type) {
		case threads.ToolPayloadJSONSchema:
			schema, err := requestToolInputSchema(spec.Name, p)
			if err != nil {
				return nil, err
			}
			tool := &anthropicapi.ToolParam{
				Name:                spec.Name,
				InputSchema:         schema,
				Strict:              anthropicapi.Bool(true),
				EagerInputStreaming: anthropicapi.Bool(eagerInputStreaming),
				Type:                anthropicapi.ToolTypeCustom,
			}
			if spec.Description != "" {
				tool.Description = anthropicapi.String(spec.Description)
			}
			out = append(out, anthropicapi.ToolUnionParam{OfTool: tool})
		default:
			return nil, fmt.Errorf("anthropic tool %q payload not supported: %T", spec.Name, spec.Payload)
		}
	}
	return out, nil
}

func filteredTools(snap threads.ToolOfferSnapshot) ([]threads.ToolSpec, error) {
	if snap.Allowed == nil {
		return append([]threads.ToolSpec(nil), snap.Offered...), nil
	}
	if len(snap.Allowed) == 0 {
		return append([]threads.ToolSpec(nil), snap.Offered...), nil
	}

	byName := make(map[string]threads.ToolSpec, len(snap.Offered))
	for _, spec := range snap.Offered {
		byName[spec.Name] = spec
	}

	out := make([]threads.ToolSpec, 0, len(snap.Allowed))
	for _, name := range snap.Allowed {
		spec, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("anthropic tool %q not offered", name)
		}
		out = append(out, spec)
	}
	return out, nil
}

func requestToolInputSchema(name string, schema threads.ToolPayloadJSONSchema) (anthropicapi.ToolInputSchemaParam, error) {
	params := map[string]any{}
	buf, err := json.Marshal(gschema.Schema(schema))
	if err != nil {
		return anthropicapi.ToolInputSchemaParam{}, fmt.Errorf("anthropic tool %q schema: %w", name, err)
	}
	if err := json.Unmarshal(buf, &params); err != nil {
		return anthropicapi.ToolInputSchemaParam{}, fmt.Errorf("anthropic tool %q schema: %w", name, err)
	}
	closeObjectSchemas(params)
	if kind := strings.TrimSpace(stringValue(params["type"])); kind != "" && kind != "object" {
		return anthropicapi.ToolInputSchemaParam{}, fmt.Errorf("anthropic tool %q schema type must be object, got %q", name, kind)
	}

	input := anthropicapi.ToolInputSchemaParam{Type: "object"}
	if properties, ok := params["properties"]; ok {
		input.Properties = properties
	}
	if required := stringSlice(params["required"]); required != nil {
		input.Required = required
	}
	delete(params, "type")
	delete(params, "properties")
	delete(params, "required")
	if len(params) > 0 {
		input.ExtraFields = params
	}
	return input, nil
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

func requestToolChoice(snap threads.ToolOfferSnapshot, hasTools bool) *anthropicapi.ToolChoiceUnionParam {
	if !hasTools {
		return nil
	}
	if snap.Allowed != nil && len(snap.Allowed) == 0 {
		none := anthropicapi.NewToolChoiceNoneParam()
		return &anthropicapi.ToolChoiceUnionParam{OfNone: &none}
	}
	if snap.Allowed == nil && snap.Parallel == nil {
		return nil
	}

	auto := anthropicapi.ToolChoiceAutoParam{Type: "auto"}
	if snap.Parallel != nil {
		auto.DisableParallelToolUse = anthropicapi.Bool(!*snap.Parallel)
	}
	return &anthropicapi.ToolChoiceUnionParam{OfAuto: &auto}
}

func stringValue(v any) string {
	s, _ := v.(string)
	return s
}

func stringSlice(v any) []string {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		s, ok := item.(string)
		if !ok {
			return nil
		}
		out = append(out, s)
	}
	return out
}
