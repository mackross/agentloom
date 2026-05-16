package fireworks

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	gschema "github.com/google/jsonschema-go/jsonschema"
	openaiapi "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/shared"

	cachefireworks "github.com/mackross/agentloom/llms/cache/fireworks"
	"github.com/mackross/agentloom/llms/internal/streamerutil"
	"github.com/mackross/agentloom/threads"
)

const (
	BaseURL                              = "https://api.fireworks.ai/inference/v1"
	Kimi25Model                          = "accounts/fireworks/models/kimi-k2p5"
	DefaultModel                         = Kimi25Model
	DefaultContextLengthExceededBehavior = "error"
)

type ChatCompletionsStreamer struct {
	client                        openaiapi.Client
	model                         string
	ContextLengthExceededBehavior string
	OnOutputTextDelta             func(string)
	normalizers                   threads.ToolNormalizers
}

type toolKey struct {
	choiceIndex int64
	toolIndex   int
}

type toolState struct {
	callID string
	name   string
	args   strings.Builder
}

func NewChatCompletionsStreamer(model string) *ChatCompletionsStreamer {
	return NewChatCompletionsStreamerWithClient(newClientFromEnv(), model)
}

func NewChatCompletionsStreamerWithClient(client openaiapi.Client, model string) *ChatCompletionsStreamer {
	model = strings.TrimSpace(model)
	if model == "" {
		model = DefaultModel
	}
	return &ChatCompletionsStreamer{
		client:                        client,
		model:                         model,
		ContextLengthExceededBehavior: DefaultContextLengthExceededBehavior,
	}
}

func (*ChatCompletionsStreamer) Capabilities() threads.StreamerCapabilities {
	return threads.StreamerCapabilities{AssistantPrefix: true}
}

func (s *ChatCompletionsStreamer) RegisterToolNormalizer(name string, normalizer threads.ToolNormalizer) {
	s.normalizers.RegisterToolNormalizer(name, normalizer)
}

func (s *ChatCompletionsStreamer) UnregisterToolNormalizer(name string) {
	s.normalizers.UnregisterToolNormalizer(name)
}

func newClientFromEnv() openaiapi.Client {
	opts := []option.RequestOption{option.WithBaseURL(BaseURL)}
	if apiKey := strings.TrimSpace(fireworksAPIKey()); apiKey != "" {
		opts = append(opts, option.WithAPIKey(apiKey))
	}
	return openaiapi.NewClient(opts...)
}

func fireworksAPIKey() string {
	if key := strings.TrimSpace(os.Getenv("FIREWORKS_API_KEY")); key != "" {
		return key
	}
	return strings.TrimSpace(os.Getenv("FIREWORKS_AI_API_KEY"))
}

func (s *ChatCompletionsStreamer) StreamReq(req threads.Req, emit func(threads.Item) error) error {
	return s.StreamReqContext(context.Background(), req, emit)
}

func (s *ChatCompletionsStreamer) StreamReqContext(ctx context.Context, req threads.Req, emit func(threads.Item) error) error {
	req, err := s.normalizers.NormalizeReq(req)
	if err != nil {
		return err
	}

	messages, err := requestMessages(req)
	if err != nil {
		return err
	}

	params := openaiapi.ChatCompletionNewParams{
		Model:    s.model,
		Messages: messages,
	}

	tools, err := requestTools(req.Tools)
	if err != nil {
		return err
	}
	if len(tools) > 0 {
		params.Tools = tools
	}

	toolChoice, err := requestToolChoice(req.Tools)
	if err != nil {
		return err
	}
	if toolChoice != nil {
		params.ToolChoice = *toolChoice
	}
	if req.Tools.Parallel != nil {
		params.ParallelToolCalls = openaiapi.Bool(*req.Tools.Parallel)
	}

	opts := []option.RequestOption{}
	if behavior := strings.TrimSpace(s.ContextLengthExceededBehavior); behavior != "" {
		opts = append(opts, option.WithJSONSet("context_length_exceeded_behavior", behavior))
	}
	if key, ok := streamerutil.LastStringMetadata(req, cachefireworks.SessionAffinityKey); ok {
		opts = append(opts, option.WithHeader("x-session-affinity", key))
	}
	if key, ok := streamerutil.LastStringMetadata(req, cachefireworks.PromptCacheIsolationKeyKey); ok {
		opts = append(opts, option.WithJSONSet("prompt_cache_isolation_key", key))
	}

	stream := s.client.Chat.Completions.NewStreaming(ctx, params, opts...)
	defer stream.Close()

	toolsInFlight := map[toolKey]*toolState{}
	for stream.Next() {
		chunk := stream.Current()
		for _, choice := range chunk.Choices {
			for _, deltaTool := range choice.Delta.ToolCalls {
				key := toolKey{choiceIndex: choice.Index, toolIndex: clampToolIndex(deltaTool.Index)}
				state := ensureToolState(toolsInFlight, key)
				if deltaTool.ID != "" {
					state.callID = deltaTool.ID
				}
				if deltaTool.Function.Name != "" {
					state.name += deltaTool.Function.Name
				}
				argsDelta := deltaTool.Function.Arguments
				hadArgs := state.args.Len() > 0
				if argsDelta != "" {
					state.args.WriteString(argsDelta)
					if choice.FinishReason != "tool_calls" || hadArgs {
						if err := emit(threads.ToolCallChunk{
							CallID:       state.callID,
							Name:         state.name,
							PayloadDelta: argsDelta,
						}); err != nil {
							return err
						}
					}
				}
			}

			if choice.Delta.Content != "" {
				if s.OnOutputTextDelta != nil {
					s.OnOutputTextDelta(choice.Delta.Content)
				}
				if err := emit(threads.AssistantText(choice.Delta.Content)); err != nil {
					return err
				}
			}

			if choice.FinishReason == "tool_calls" {
				if err := emitFinalToolCalls(toolsInFlight, choice.Index, s.normalizeToolCallEmit(emit)); err != nil {
					return err
				}
			}
		}
	}

	if err := stream.Err(); err != nil {
		return err
	}
	return emitRemainingToolCalls(toolsInFlight, s.normalizeToolCallEmit(emit))
}

func (s *ChatCompletionsStreamer) normalizeToolCallEmit(emit func(threads.Item) error) func(threads.Item) error {
	return func(item threads.Item) error {
		call, ok := item.(threads.ToolCall)
		if ok {
			var err error
			call, err = s.normalizers.NormalizeResponseToolCall(call)
			if err != nil {
				return err
			}
			item = call
		}
		return emit(item)
	}
}

func requestMessages(req threads.Req) ([]openaiapi.ChatCompletionMessageParamUnion, error) {
	out := make([]openaiapi.ChatCompletionMessageParamUnion, 0, len(req.Items)+1)
	if req.Instruction != "" {
		out = append(out, openaiapi.SystemMessage(req.Instruction))
	}
	for _, item := range req.Items {
		switch v := item.(type) {
		case threads.UserText:
			out = append(out, openaiapi.UserMessage(string(v)))
		case threads.AssistantText:
			out = append(out, openaiapi.AssistantMessage(string(v)))
		case threads.ToolCall:
			out = append(out, assistantToolCallMessage(v))
		case threads.ToolCallResultable:
			out = append(out, openaiapi.ToolMessage(v.ToolOutput(), v.ToolCallID()))
		default:
			return nil, fmt.Errorf("fireworks request item not supported: %T", item)
		}
	}
	return out, nil
}

func assistantToolCallMessage(call threads.ToolCall) openaiapi.ChatCompletionMessageParamUnion {
	return openaiapi.ChatCompletionMessageParamUnion{
		OfAssistant: &openaiapi.ChatCompletionAssistantMessageParam{
			ToolCalls: []openaiapi.ChatCompletionMessageToolCallUnionParam{{
				OfFunction: &openaiapi.ChatCompletionMessageFunctionToolCallParam{
					ID: call.CallID,
					Function: openaiapi.ChatCompletionMessageFunctionToolCallFunctionParam{
						Name:      call.Name,
						Arguments: call.Payload,
					},
				},
			}},
		},
	}
}

func requestTools(snap threads.ToolOfferSnapshot) ([]openaiapi.ChatCompletionToolUnionParam, error) {
	specs, err := filteredTools(snap)
	if err != nil {
		return nil, err
	}
	if len(specs) == 0 {
		return nil, nil
	}

	out := make([]openaiapi.ChatCompletionToolUnionParam, 0, len(specs))
	for _, spec := range specs {
		if spec.Payload == nil {
			return nil, fmt.Errorf("fireworks tool %q payload not supported: %T", spec.Name, spec.Payload)
		}
		switch p := spec.Payload.(type) {
		case threads.ToolPayloadJSONSchema:
			parameters, err := requestFunctionParameters(spec.Name, p)
			if err != nil {
				return nil, err
			}
			def := shared.FunctionDefinitionParam{
				Name:       spec.Name,
				Parameters: parameters,
				Strict:     openaiapi.Bool(true),
			}
			if spec.Description != "" {
				def.Description = openaiapi.String(spec.Description)
			}
			out = append(out, openaiapi.ChatCompletionFunctionTool(def))
		default:
			return nil, fmt.Errorf("fireworks tool %q payload not supported: %T", spec.Name, spec.Payload)
		}
	}
	return out, nil
}

func filteredTools(snap threads.ToolOfferSnapshot) ([]threads.ToolSpec, error) {
	if snap.Allowed == nil || len(snap.Allowed) == 0 {
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
			return nil, fmt.Errorf("fireworks tool %q not offered", name)
		}
		out = append(out, spec)
	}
	return out, nil
}

func requestFunctionParameters(name string, schema threads.ToolPayloadJSONSchema) (shared.FunctionParameters, error) {
	params := map[string]any{}
	buf, err := json.Marshal(gschema.Schema(schema))
	if err != nil {
		return nil, fmt.Errorf("fireworks tool %q schema: %w", name, err)
	}
	if err := json.Unmarshal(buf, &params); err != nil {
		return nil, fmt.Errorf("fireworks tool %q schema: %w", name, err)
	}
	closeObjectSchemas(params)
	return shared.FunctionParameters(params), nil
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

func requestToolChoice(snap threads.ToolOfferSnapshot) (*openaiapi.ChatCompletionToolChoiceOptionUnionParam, error) {
	if snap.Allowed == nil {
		if snap.Required {
			return &openaiapi.ChatCompletionToolChoiceOptionUnionParam{
				OfAuto: param.NewOpt(string(openaiapi.ChatCompletionToolChoiceOptionAutoRequired)),
			}, nil
		}
		return nil, nil
	}
	if len(snap.Allowed) == 0 {
		if snap.Required {
			return nil, fmt.Errorf("fireworks tool choice cannot require an empty allowed tool set")
		}
		return &openaiapi.ChatCompletionToolChoiceOptionUnionParam{
			OfAuto: param.NewOpt(string(openaiapi.ChatCompletionToolChoiceOptionAutoNone)),
		}, nil
	}
	if len(snap.Allowed) == 1 {
		name := snap.Allowed[0]
		if _, err := toolKind(snap, name); err != nil {
			return nil, err
		}
		return &openaiapi.ChatCompletionToolChoiceOptionUnionParam{
			OfFunctionToolChoice: &openaiapi.ChatCompletionNamedToolChoiceParam{
				Function: openaiapi.ChatCompletionNamedToolChoiceFunctionParam{Name: name},
			},
		}, nil
	}
	if snap.Required {
		return &openaiapi.ChatCompletionToolChoiceOptionUnionParam{
			OfAuto: param.NewOpt(string(openaiapi.ChatCompletionToolChoiceOptionAutoRequired)),
		}, nil
	}
	return &openaiapi.ChatCompletionToolChoiceOptionUnionParam{
		OfAuto: param.NewOpt(string(openaiapi.ChatCompletionToolChoiceOptionAutoAuto)),
	}, nil
}

func toolKind(snap threads.ToolOfferSnapshot, name string) (string, error) {
	for _, spec := range snap.Offered {
		if spec.Name != name {
			continue
		}
		switch spec.Payload.(type) {
		case threads.ToolPayloadJSONSchema:
			return "function", nil
		default:
			return "", fmt.Errorf("fireworks tool %q payload not supported: %T", name, spec.Payload)
		}
	}
	return "", fmt.Errorf("fireworks tool %q not offered", name)
}

func clampToolIndex(index int64) int {
	if index < 0 {
		return 0
	}
	return int(index)
}

func ensureToolState(states map[toolKey]*toolState, key toolKey) *toolState {
	state, ok := states[key]
	if ok {
		return state
	}
	state = &toolState{}
	states[key] = state
	return state
}

func emitFinalToolCalls(states map[toolKey]*toolState, choiceIndex int64, emit func(threads.Item) error) error {
	keys := make([]toolKey, 0, len(states))
	for key := range states {
		if key.choiceIndex == choiceIndex {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		return keys[i].toolIndex < keys[j].toolIndex
	})
	for _, key := range keys {
		state := states[key]
		if err := emit(threads.ToolCall{
			CallID:  state.callID,
			Name:    state.name,
			Payload: state.args.String(),
		}); err != nil {
			return err
		}
		delete(states, key)
	}
	return nil
}

func emitRemainingToolCalls(states map[toolKey]*toolState, emit func(threads.Item) error) error {
	keys := make([]toolKey, 0, len(states))
	for key := range states {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].choiceIndex != keys[j].choiceIndex {
			return keys[i].choiceIndex < keys[j].choiceIndex
		}
		return keys[i].toolIndex < keys[j].toolIndex
	})
	for _, key := range keys {
		state := states[key]
		if err := emit(threads.ToolCall{
			CallID:  state.callID,
			Name:    state.name,
			Payload: state.args.String(),
		}); err != nil {
			return err
		}
	}
	return nil
}

func stringValue(v any) string {
	s, _ := v.(string)
	return s
}
