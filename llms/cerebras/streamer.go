package cerebras

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	gschema "github.com/google/jsonschema-go/jsonschema"
	openaiapi "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/shared"
	"github.com/vmihailenco/msgpack/v5"

	cachecerebras "github.com/mackross/agentloom/llms/cache/cerebras"
	"github.com/mackross/agentloom/llms/internal/streamerutil"
	"github.com/mackross/agentloom/threads"
)

const (
	BaseURL = "https://api.cerebras.ai/v1"

	Gemma4_31BModel   = "gemma-4-31b"
	GPTOSS120BModel   = "gpt-oss-120b"
	Llama31_8BModel   = "llama3.1-8b"
	Qwen3235BModel    = "qwen-3-235b-a22b-instruct-2507"
	reasoningProvider = "cerebras.chat"

	DefaultModel = GPTOSS120BModel
)

const defaultGzipCompressionLevel = 5

type PredictionFunc func(req threads.Req) (content string, ok bool, err error)

type ChatCompletionsStreamer struct {
	client openaiapi.Client
	model  string

	// ReasoningFormat is empty by default. Cerebras streamed delta.reasoning fields
	// are ignored rather than emitted as thread-visible assistant text.
	ReasoningEffort string
	ReasoningFormat string
	Temperature     *float64

	// ServiceTier accepts Cerebras values: priority, default, auto, and flex.
	ServiceTier    string
	QueueThreshold string

	// Prediction must not be used with tools; Cerebras predicted outputs are a text
	// regeneration optimization for models that support predicted outputs.
	Prediction PredictionFunc

	GzipCompressionLevel int
	OnOutputTextDelta    func(string)
	normalizers          threads.ToolNormalizers
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
	return &ChatCompletionsStreamer{client: client, model: model}
}

func (*ChatCompletionsStreamer) Capabilities() threads.StreamerCapabilities {
	return threads.StreamerCapabilities{AssistantPrefix: true, Reasoning: threads.ReasoningForCurrentTurn(reasoningProvider)}
}

func (*ChatCompletionsStreamer) SyntheticToolCallID() string {
	return fmt.Sprintf("call_%x", time.Now().UnixNano())
}

func (s *ChatCompletionsStreamer) RegisterToolNormalizer(name string, normalizer threads.ToolNormalizer) {
	s.normalizers.RegisterToolNormalizer(name, normalizer)
}

func (s *ChatCompletionsStreamer) UnregisterToolNormalizer(name string) {
	s.normalizers.UnregisterToolNormalizer(name)
}

func newClientFromEnv() openaiapi.Client {
	opts := []option.RequestOption{option.WithBaseURL(BaseURL)}
	if apiKey := strings.TrimSpace(cerebrasAPIKey()); apiKey != "" {
		opts = append(opts, option.WithAPIKey(apiKey))
	}
	return openaiapi.NewClient(opts...)
}

func cerebrasAPIKey() string {
	return strings.TrimSpace(os.Getenv("CEREBRAS_API_KEY"))
}

func (s *ChatCompletionsStreamer) StreamReq(req threads.Req, emit func(threads.Item) error) error {
	return s.StreamReqContext(context.Background(), req, emit)
}

func (s *ChatCompletionsStreamer) StreamReqContext(ctx context.Context, req threads.Req, emit func(threads.Item) error) error {
	req, err := s.normalizers.NormalizeReq(req)
	if err != nil {
		return err
	}
	var predictionContent string
	var predictionOK bool
	if s.Prediction != nil {
		var err error
		predictionContent, predictionOK, err = s.Prediction(req)
		if err != nil {
			return err
		}
		if predictionOK && hasOfferedTools(req.Tools) {
			return fmt.Errorf("cerebras prediction is not supported with tools")
		}
	}

	messages, err := conversationMessages(req)
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

	toolChoice, err := cerebrasToolSelection(req.Tools)
	if err != nil {
		return err
	}
	if toolChoice != nil {
		params.ToolChoice = *toolChoice
	}
	if req.Tools.Parallel != nil {
		params.ParallelToolCalls = openaiapi.Bool(*req.Tools.Parallel)
	}

	opts, err := s.cerebrasRequestOptions()
	if err != nil {
		return err
	}
	if effort := strings.TrimSpace(s.ReasoningEffort); effort != "" {
		params.ReasoningEffort = shared.ReasoningEffort(effort)
	}
	if s.Temperature != nil {
		params.Temperature = openaiapi.Float(*s.Temperature)
	}
	if key, ok := streamerutil.LastStringMetadata(req, cachecerebras.PromptCacheKeyKey); ok {
		params.PromptCacheKey = openaiapi.String(key)
	}
	if predictionOK {
		params.Prediction = openaiapi.ChatCompletionPredictionContentParam{
			Content: openaiapi.ChatCompletionPredictionContentContentUnionParam{OfString: openaiapi.String(predictionContent)},
		}
	}

	stream := s.client.Chat.Completions.NewStreaming(ctx, params, opts...)
	defer stream.Close()

	toolsInFlight := map[toolKey]*toolState{}
	reasoning := map[int64]string{}
	flushReasoning := func(choice int64) error {
		text := reasoning[choice]
		delete(reasoning, choice)
		if text == "" {
			return nil
		}
		return emit(threads.ReasoningItem{Provider: reasoningProvider, Visibility: threads.ReasoningVisibilityText, Text: text})
	}
	for stream.Next() {
		chunk := stream.Current()
		for _, choice := range chunk.Choices {
			if field := choice.Delta.JSON.ExtraFields["reasoning"]; field.Raw() != "" {
				var delta string
				if err := json.Unmarshal([]byte(field.Raw()), &delta); err != nil {
					return fmt.Errorf("cerebras reasoning delta: %w", err)
				}
				reasoning[choice.Index] += delta
			}
			for _, deltaTool := range choice.Delta.ToolCalls {
				key := toolKey{choiceIndex: choice.Index, toolIndex: nonNegativeToolIndex(deltaTool.Index)}
				state := ensureToolCallState(toolsInFlight, key)
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
				if err := flushReasoning(choice.Index); err != nil {
					return err
				}
				if s.OnOutputTextDelta != nil {
					s.OnOutputTextDelta(choice.Delta.Content)
				}
				if err := emit(threads.AssistantText(choice.Delta.Content)); err != nil {
					return err
				}
			}

			if choice.FinishReason == "tool_calls" {
				if err := flushReasoning(choice.Index); err != nil {
					return err
				}
				if err := finishChoiceToolCalls(toolsInFlight, choice.Index, s.normalizeEmittedToolCalls(emit)); err != nil {
					return err
				}
			}
		}
	}

	if err := stream.Err(); err != nil {
		return err
	}
	for choice := range reasoning {
		if err := flushReasoning(choice); err != nil {
			return err
		}
	}
	return finishRemainingToolCalls(toolsInFlight, s.normalizeEmittedToolCalls(emit))
}

func (s *ChatCompletionsStreamer) cerebrasRequestOptions() ([]option.RequestOption, error) {
	opts := []option.RequestOption{option.WithMiddleware(cerebrasCompressionMiddleware(s.compressionLevel()))}
	if format := strings.TrimSpace(s.ReasoningFormat); format != "" {
		opts = append(opts, option.WithJSONSet("reasoning_format", format))
	}
	if tier := strings.TrimSpace(s.ServiceTier); tier != "" {
		if !validServiceTier(tier) {
			return nil, fmt.Errorf("cerebras service tier %q not supported; use priority, default, auto, or flex", tier)
		}
		opts = append(opts, option.WithJSONSet("service_tier", tier))
	}
	if threshold := strings.TrimSpace(s.QueueThreshold); threshold != "" {
		opts = append(opts, option.WithHeader("queue_threshold", threshold))
	}
	return opts, nil
}

func (s *ChatCompletionsStreamer) compressionLevel() int {
	if s.GzipCompressionLevel == 0 {
		return defaultGzipCompressionLevel
	}
	return s.GzipCompressionLevel
}

func validServiceTier(tier string) bool {
	switch tier {
	case "priority", "default", "auto", "flex":
		return true
	default:
		return false
	}
}

func hasOfferedTools(snap threads.ToolOfferSnapshot) bool {
	return len(snap.Offered) > 0
}

func cerebrasCompressionMiddleware(level int) option.Middleware {
	return func(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
		if !shouldCompressCerebrasRequest(req) {
			return next(req)
		}
		if err := encodeCerebrasMsgpackGzip(req, level); err != nil {
			return nil, err
		}
		return next(req)
	}
}

func shouldCompressCerebrasRequest(req *http.Request) bool {
	if req == nil || req.Method != http.MethodPost || req.URL == nil {
		return false
	}
	return strings.HasSuffix(strings.TrimRight(req.URL.Path, "/"), "/chat/completions") && strings.EqualFold(req.URL.Hostname(), "api.cerebras.ai")
}

func encodeCerebrasMsgpackGzip(req *http.Request, level int) error {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return fmt.Errorf("cerebras msgpack gzip read body: %w", err)
	}
	_ = req.Body.Close()

	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return fmt.Errorf("cerebras msgpack gzip decode json: %w", err)
	}
	value = convertJSONNumbersForMsgpack(value)
	msgpackBody, err := msgpack.Marshal(value)
	if err != nil {
		return fmt.Errorf("cerebras msgpack gzip encode msgpack: %w", err)
	}
	var gz bytes.Buffer
	zw, err := gzip.NewWriterLevel(&gz, level)
	if err != nil {
		return fmt.Errorf("cerebras msgpack gzip compression level: %w", err)
	}
	if _, err := zw.Write(msgpackBody); err != nil {
		_ = zw.Close()
		return fmt.Errorf("cerebras msgpack gzip compress: %w", err)
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("cerebras msgpack gzip close: %w", err)
	}
	compressed := gz.Bytes()
	req.Body = io.NopCloser(bytes.NewReader(compressed))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(compressed)), nil }
	req.ContentLength = int64(len(compressed))
	req.Header.Set("Content-Type", "application/vnd.msgpack")
	req.Header.Set("Content-Encoding", "gzip")
	return nil
}

func convertJSONNumbersForMsgpack(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for key, child := range x {
			x[key] = convertJSONNumbersForMsgpack(child)
		}
		return x
	case []any:
		for i, child := range x {
			x[i] = convertJSONNumbersForMsgpack(child)
		}
		return x
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i
		}
		if f, err := x.Float64(); err == nil {
			return f
		}
		return x.String()
	default:
		return v
	}
}

func (s *ChatCompletionsStreamer) normalizeEmittedToolCalls(emit func(threads.Item) error) func(threads.Item) error {
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

func conversationMessages(req threads.Req) ([]openaiapi.ChatCompletionMessageParamUnion, error) {
	out := make([]openaiapi.ChatCompletionMessageParamUnion, 0, len(req.Items)+1)
	if req.Instruction != "" {
		out = append(out, openaiapi.SystemMessage(req.Instruction))
	}
	pendingReasoning := ""
	for i := 0; i < len(req.Items); i++ {
		item := req.Items[i]
		switch v := item.(type) {
		case threads.ReasoningItem:
			pendingReasoning += v.Text
		case threads.UserText:
			if pendingReasoning != "" {
				return nil, fmt.Errorf("cerebras reasoning is not followed by a tool call")
			}
			out = append(out, openaiapi.UserMessage(string(v)))
		case threads.AssistantText:
			pendingReasoning = ""
			out = append(out, openaiapi.AssistantMessage(string(v)))
		case threads.ToolCall:
			calls := []threads.ToolCall{v}
			for i+1 < len(req.Items) {
				next, ok := req.Items[i+1].(threads.ToolCall)
				if !ok {
					break
				}
				i++
				calls = append(calls, next)
			}
			message := toolCallsMessage(calls)
			if pendingReasoning != "" {
				message.OfAssistant.Content = openaiapi.ChatCompletionAssistantMessageParamContentUnion{OfString: openaiapi.String(pendingReasoning)}
				pendingReasoning = ""
			}
			out = append(out, message)
		case threads.ToolCallResult:
			if pendingReasoning != "" {
				return nil, fmt.Errorf("cerebras reasoning is not followed by a tool call")
			}
			out = append(out, openaiapi.ToolMessage(v.Output, v.CallID))
		default:
			return nil, fmt.Errorf("cerebras request item not supported: %T", item)
		}
	}
	if pendingReasoning != "" {
		return nil, fmt.Errorf("cerebras reasoning has no associated tool call")
	}
	return out, nil
}

func toolCallsMessage(calls []threads.ToolCall) openaiapi.ChatCompletionMessageParamUnion {
	toolCalls := make([]openaiapi.ChatCompletionMessageToolCallUnionParam, 0, len(calls))
	for _, call := range calls {
		toolCalls = append(toolCalls, openaiapi.ChatCompletionMessageToolCallUnionParam{
			OfFunction: &openaiapi.ChatCompletionMessageFunctionToolCallParam{
				ID: call.CallID,
				Function: openaiapi.ChatCompletionMessageFunctionToolCallFunctionParam{
					Name: call.Name, Arguments: call.Payload,
				},
			},
		})
	}
	return openaiapi.ChatCompletionMessageParamUnion{
		OfAssistant: &openaiapi.ChatCompletionAssistantMessageParam{
			ToolCalls: toolCalls,
		},
	}
}

func requestTools(snap threads.ToolOfferSnapshot) ([]openaiapi.ChatCompletionToolUnionParam, error) {
	specs, err := requestToolSpecs(snap)
	if err != nil {
		return nil, err
	}
	if len(specs) == 0 {
		return nil, nil
	}

	out := make([]openaiapi.ChatCompletionToolUnionParam, 0, len(specs))
	for _, spec := range specs {
		if spec.Payload == nil {
			return nil, fmt.Errorf("cerebras tool %q payload not supported: %T", spec.Name, spec.Payload)
		}
		switch p := spec.Payload.(type) {
		case threads.ToolPayloadJSONSchema:
			parameters, err := cerebrasToolInputSchema(spec.Name, p)
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
			return nil, fmt.Errorf("cerebras tool %q payload not supported: %T", spec.Name, spec.Payload)
		}
	}
	return out, nil
}

func requestToolSpecs(snap threads.ToolOfferSnapshot) ([]threads.ToolSpec, error) {
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
			return nil, fmt.Errorf("cerebras tool %q not offered", name)
		}
		out = append(out, spec)
	}
	return out, nil
}

func cerebrasToolInputSchema(name string, schema threads.ToolPayloadJSONSchema) (shared.FunctionParameters, error) {
	params := map[string]any{}
	buf, err := json.Marshal(gschema.Schema(schema))
	if err != nil {
		return nil, fmt.Errorf("cerebras tool %q schema: %w", name, err)
	}
	if err := json.Unmarshal(buf, &params); err != nil {
		return nil, fmt.Errorf("cerebras tool %q schema: %w", name, err)
	}
	normalizeCerebrasToolSchema(params)
	return shared.FunctionParameters(params), nil
}

func normalizeCerebrasToolSchema(v any) {
	switch x := v.(type) {
	case map[string]any:
		if schemaAllowsType(x, "object") {
			if props, ok := x["properties"].(map[string]any); (!ok || len(props) == 0) && x["anyOf"] == nil {
				if additional, ok := x["additionalProperties"].(map[string]any); ok {
					x["properties"] = map[string]any{
						"_": additional,
					}
					delete(x, "additionalProperties")
				}
			}
			if _, ok := x["additionalProperties"]; !ok {
				x["additionalProperties"] = false
			}
		}
		for _, child := range x {
			normalizeCerebrasToolSchema(child)
		}
	case []any:
		for _, child := range x {
			normalizeCerebrasToolSchema(child)
		}
	}
}

func schemaAllowsType(schema map[string]any, want string) bool {
	switch typ := schema["type"].(type) {
	case string:
		return typ == want
	case []any:
		for _, v := range typ {
			if s, ok := v.(string); ok && s == want {
				return true
			}
		}
	}
	return false
}

func cerebrasToolSelection(snap threads.ToolOfferSnapshot) (*openaiapi.ChatCompletionToolChoiceOptionUnionParam, error) {
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
			return nil, fmt.Errorf("cerebras tool choice cannot require an empty allowed tool set")
		}
		return &openaiapi.ChatCompletionToolChoiceOptionUnionParam{
			OfAuto: param.NewOpt(string(openaiapi.ChatCompletionToolChoiceOptionAutoNone)),
		}, nil
	}
	if len(snap.Allowed) == 1 {
		name := snap.Allowed[0]
		if _, err := offeredToolKind(snap, name); err != nil {
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

func offeredToolKind(snap threads.ToolOfferSnapshot, name string) (string, error) {
	for _, spec := range snap.Offered {
		if spec.Name != name {
			continue
		}
		switch spec.Payload.(type) {
		case threads.ToolPayloadJSONSchema:
			return "function", nil
		default:
			return "", fmt.Errorf("cerebras tool %q payload not supported: %T", name, spec.Payload)
		}
	}
	return "", fmt.Errorf("cerebras tool %q not offered", name)
}

func nonNegativeToolIndex(index int64) int {
	if index < 0 {
		return 0
	}
	return int(index)
}

func ensureToolCallState(states map[toolKey]*toolState, key toolKey) *toolState {
	state, ok := states[key]
	if ok {
		return state
	}
	state = &toolState{}
	states[key] = state
	return state
}

func finishChoiceToolCalls(states map[toolKey]*toolState, choiceIndex int64, emit func(threads.Item) error) error {
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

func finishRemainingToolCalls(states map[toolKey]*toolState, emit func(threads.Item) error) error {
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
