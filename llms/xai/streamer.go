// Package xai adapts xAI Grok models to threads.Streamer via the Responses API.
//
// This package owns a forked copy of the Responses streamer (originally based on
// llms/openai) so xAI-specific request shape and behavior can diverge freely.
package xai

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	gschema "github.com/google/jsonschema-go/jsonschema"
	openaiapi "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"

	cachexai "github.com/mackross/agentloom/llms/cache/xai"
	"github.com/mackross/agentloom/llms/internal/streamerutil"
	"github.com/mackross/agentloom/threads"
)

const (
	// BaseURL is the default xAI OpenAI-compatible API root.
	BaseURL = "https://api.x.ai/v1"
	// DefaultModel is the default Grok frontier model.
	DefaultModel = "grok-4.5"
)

type ResponsesStreamer struct {
	client            openaiapi.Client
	model             string
	Reasoning         shared.ReasoningParam
	ServiceTier       responses.ResponseNewParamsServiceTier
	OnOutputTextDelta func(string)

	// UsePreviousResponseID enables continuation requests. When enabled,
	// the streamer uses a prefix hash to detect append-only follow-up requests and
	// sends only the new input items with previous_response_id. The zero value means enabled.
	UsePreviousResponseID bool
	// DisablePreviousResponseID disables continuation requests.
	DisablePreviousResponseID bool
	// Store controls the Responses store parameter. nil defaults to false.
	// xAI constructors set true: previous_response_id requires stored responses.
	Store *bool
	// StreamToolCalls controls the xAI stream_tool_calls request field.
	// nil omits the field; constructors set true so tool args stream as deltas
	// when previous_response_id is disabled. When previous_response_id is enabled
	// (the default), stream_tool_calls is omitted: xAI stores corrupted tool
	// arguments for stream_tool_calls responses, and continuations then fail with
	// "Invalid tool arguments... EOF while parsing a string".
	StreamToolCalls *bool

	normalizers threads.ToolNormalizers

	mu                         sync.Mutex
	continuation               responseContinuation
	lastUsedPreviousResponseID bool
}

type responseContinuation struct {
	responseID string
	prefixHash [32]byte
	prefixLen  int
	paramsHash [32]byte
}

type responseStreamRequest struct {
	stream           responseStream
	inputItems       responses.ResponseInputParam
	paramsHash       [32]byte
	usedContinuation bool
}

type responseStream interface {
	Next() bool
	Current() responses.ResponseStreamEventUnion
	Err() error
	Close() error
}

type functionCallMeta struct {
	callID string
	name   string
}

func NewResponsesStreamer(model string) *ResponsesStreamer {
	return NewResponsesStreamerWithClient(newClientFromEnv(), model)
}

func NewResponsesStreamerWithClient(client openaiapi.Client, model string) *ResponsesStreamer {
	model = strings.TrimSpace(model)
	if model == "" {
		model = DefaultModel
	}
	store := true
	streamTools := true
	return &ResponsesStreamer{
		client:          client,
		model:           model,
		Store:           &store,
		StreamToolCalls: &streamTools,
	}
}

func newClientFromEnv() openaiapi.Client {
	opts := []option.RequestOption{option.WithBaseURL(baseURLFromEnv())}
	if apiKey := strings.TrimSpace(xaiAPIKey()); apiKey != "" {
		opts = append(opts, option.WithAPIKey(apiKey))
	}
	return openaiapi.NewClient(opts...)
}

func baseURLFromEnv() string {
	if base := strings.TrimSpace(os.Getenv("XAI_BASE_URL")); base != "" {
		return strings.TrimRight(base, "/")
	}
	return BaseURL
}

func xaiAPIKey() string {
	return strings.TrimSpace(os.Getenv("XAI_API_KEY"))
}

func (*ResponsesStreamer) Capabilities() threads.StreamerCapabilities {
	return threads.StreamerCapabilities{AssistantPrefix: true, ToolResultSendPolicy: threads.ToolResultSendRequiresComplete}
}

func (*ResponsesStreamer) SyntheticToolCallID() string {
	return fmt.Sprintf("call_%x", time.Now().UnixNano())
}

func (s *ResponsesStreamer) RegisterToolNormalizer(name string, normalizer threads.ToolNormalizer) {
	s.normalizers.RegisterToolNormalizer(name, normalizer)
}

func (s *ResponsesStreamer) UnregisterToolNormalizer(name string) {
	s.normalizers.UnregisterToolNormalizer(name)
}

func (s *ResponsesStreamer) StreamReq(req threads.Req, emit func(threads.Item) error) error {
	return s.StreamReqContext(context.Background(), req, emit)
}

func (s *ResponsesStreamer) Close() error {
	return nil
}

func (s *ResponsesStreamer) StreamReqContext(ctx context.Context, req threads.Req, emit func(threads.Item) error) error {
	req, err := s.normalizers.NormalizeReq(req)
	if err != nil {
		return err
	}

	params, err := s.responseParams(req)
	if err != nil {
		return err
	}

	streamReq, err := s.newResponseStream(ctx, params, true)
	if err != nil {
		return err
	}
	defer streamReq.stream.Close()

	emitted := false
	emitTracked := func(item threads.Item) error {
		emitted = true
		return emit(item)
	}
	responseID, outputItems, err := s.consumeResponseStream(streamReq.stream, emitTracked)
	if err != nil && shouldRetryResponseStreamError(ctx, err, streamReq, emitted) {
		_ = streamReq.stream.Close()
		if streamReq.usedContinuation {
			s.clearContinuation()
		}
		streamReq, err = s.newResponseStream(ctx, params, false)
		if err != nil {
			return err
		}
		defer streamReq.stream.Close()
		responseID, outputItems, err = s.consumeResponseStream(streamReq.stream, emitTracked)
	}
	if err != nil {
		return err
	}
	if s.usePreviousResponseID() {
		s.rememberContinuation(responseID, streamReq.inputItems, outputItems, streamReq.paramsHash)
	}
	return nil
}

func (s *ResponsesStreamer) storeResponses() bool {
	if s.Store == nil {
		return false
	}
	return *s.Store
}

func (s *ResponsesStreamer) responseParams(req threads.Req) (responses.ResponseNewParams, error) {
	inputItems, err := requestInputItems(req)
	if err != nil {
		return responses.ResponseNewParams{}, err
	}

	params := responses.ResponseNewParams{
		Model:     s.model,
		Input:     responses.ResponseNewParamsInputUnion{OfInputItemList: inputItems},
		Reasoning: s.Reasoning,
		Store:     openaiapi.Bool(s.storeResponses()),
	}
	if s.ServiceTier != "" {
		params.ServiceTier = s.ServiceTier
	}
	if req.Instruction != "" {
		params.Instructions = openaiapi.String(req.Instruction)
	}
	tools, err := requestTools(req.Tools)
	if err != nil {
		return responses.ResponseNewParams{}, err
	}
	if len(tools) > 0 {
		params.Tools = tools
	}
	choice, err := requestToolChoice(req.Tools)
	if err != nil {
		return responses.ResponseNewParams{}, err
	}
	if choice != nil {
		params.ToolChoice = *choice
	}
	if req.Tools.Parallel != nil {
		params.ParallelToolCalls = openaiapi.Bool(*req.Tools.Parallel)
	}
	if s, ok := streamerutil.LastStringMetadata(req, cachexai.PromptCacheKeyKey); ok {
		params.PromptCacheKey = openaiapi.String(s)
	}
	if s, ok := streamerutil.LastStringMetadata(req, cachexai.PromptCacheRetentionKey); ok {
		params.PromptCacheRetention = responses.ResponseNewParamsPromptCacheRetention(s)
	}
	// Prefer previous_response_id over stream_tool_calls: enabling both breaks
	// multi-turn tool loops on xAI (stored function-call arguments become unusable).
	if s.StreamToolCalls != nil && !s.usePreviousResponseID() {
		// xAI/Grok: not on ResponseNewParams; inject via extra fields.
		params.SetExtraFields(map[string]any{"stream_tool_calls": *s.StreamToolCalls})
	}
	return params, nil
}

func (s *ResponsesStreamer) newResponseStream(ctx context.Context, params responses.ResponseNewParams, allowContinuation bool) (responseStreamRequest, error) {
	fullInputItems := params.Input.OfInputItemList
	paramsHash, err := hashResponseParams(params)
	if err != nil {
		return responseStreamRequest{}, err
	}

	sendParams := params
	usedContinuation := false
	if allowContinuation && s.usePreviousResponseID() {
		usedContinuation = s.applyPreviousResponseID(&sendParams, fullInputItems, paramsHash)
	}
	s.mu.Lock()
	s.lastUsedPreviousResponseID = usedContinuation
	s.mu.Unlock()

	sr := responseStreamRequest{
		stream:           s.client.Responses.NewStreaming(ctx, sendParams),
		inputItems:       fullInputItems,
		paramsHash:       paramsHash,
		usedContinuation: usedContinuation,
	}
	return sr, nil
}

func shouldRetryResponseStreamError(ctx context.Context, err error, streamReq responseStreamRequest, emitted bool) bool {
	if err == nil || emitted || ctx.Err() != nil {
		return false
	}
	// Retry once without previous_response_id when a continuation request fails
	// before any items were emitted (e.g. stale/invalid response id).
	return streamReq.usedContinuation
}

func (s *ResponsesStreamer) consumeResponseStream(stream responseStream, emit func(threads.Item) error) (string, responses.ResponseInputParam, error) {
	functionCalls := map[string]functionCallMeta{}
	var responseID string
	var outputItems responses.ResponseInputParam

	for stream.Next() {
		event := stream.Current()
		switch event.Type {
		case "response.created", "response.completed":
			if event.Response.ID != "" {
				responseID = event.Response.ID
			}
		case "response.output_text.delta":
			if event.Delta == "" {
				continue
			}
			if s.OnOutputTextDelta != nil {
				s.OnOutputTextDelta(event.Delta)
			}
			if err := emit(threads.AssistantText(event.Delta)); err != nil {
				return "", nil, err
			}
		case "response.function_call_arguments.delta":
			if event.Delta == "" {
				continue
			}
			callID, name := resolveFunctionCall(functionCalls, event.ItemID, "")
			if err := emit(threads.ToolCallChunk{CallID: callID, Name: name, PayloadDelta: event.Delta}); err != nil {
				return "", nil, err
			}
		case "response.function_call_arguments.done":
			callID, name := resolveFunctionCall(functionCalls, event.ItemID, event.Name)
			call := threads.ToolCall{CallID: callID, Name: name, Payload: event.Arguments}
			call, err := s.normalizers.NormalizeResponseToolCall(call)
			if err != nil {
				return "", nil, err
			}
			if err := emit(call); err != nil {
				return "", nil, err
			}
		case "response.output_item.added", "response.output_item.done":
			if event.Type == "response.output_item.done" {
				if item, ok := outputItemInputParam(event.Item); ok {
					outputItems = append(outputItems, item)
				}
			}
			rememberFunctionCall(functionCalls, event.Item)
		case "error":
			if event.Message != "" {
				return "", nil, fmt.Errorf("xai responses stream error: %s", event.Message)
			}
			return "", nil, fmt.Errorf("xai responses stream error")
		}
	}

	if err := stream.Err(); err != nil {
		return "", nil, err
	}
	return responseID, outputItems, nil
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
		case threads.ToolCallChunk:
			// ToolCallChunk is a streaming artifact for partial parsing. The
			// Responses API request history should contain the final ToolCall, not
			// each chunk that preceded it.
			continue
		case threads.ToolCall:
			inputItems = append(inputItems, responses.ResponseInputItemParamOfFunctionCall(v.Payload, v.CallID, v.Name))
		case threads.ToolCallResult:
			inputItems = append(inputItems, responses.ResponseInputItemParamOfFunctionCallOutput(v.CallID, v.Output))
		default:
			return nil, fmt.Errorf("xai request item not supported: %T", it)
		}
	}
	return inputItems, nil
}

func requestTools(snap threads.ToolOfferSnapshot) ([]responses.ToolUnionParam, error) {
	if len(snap.Offered) == 0 {
		return nil, nil
	}
	// xAI has no tool_choice.allowed_tools. When Allowed is set, only include those
	// tools in the request (Weaver uses Allowed for enabled tools).
	allowed := map[string]struct{}{}
	filterAllowed := snap.Allowed != nil
	if filterAllowed {
		for _, name := range snap.Allowed {
			allowed[name] = struct{}{}
		}
		if len(allowed) == 0 {
			return nil, nil
		}
	}
	out := make([]responses.ToolUnionParam, 0, len(snap.Offered))
	for _, spec := range snap.Offered {
		if filterAllowed {
			if _, ok := allowed[spec.Name]; !ok {
				continue
			}
		}
		if spec.Payload == nil {
			return nil, fmt.Errorf("xai tool %q payload not supported: %T", spec.Name, spec.Payload)
		}
		switch p := spec.Payload.(type) {
		case threads.ToolPayloadJSONSchema:
			params := map[string]any{}
			b, err := json.Marshal(gschema.Schema(p))
			if err != nil {
				return nil, fmt.Errorf("xai tool %q schema: %w", spec.Name, err)
			}
			if err := json.Unmarshal(b, &params); err != nil {
				return nil, fmt.Errorf("xai tool %q schema: %w", spec.Name, err)
			}
			normalizeStrictObjectSchemas(params)
			f := &responses.FunctionToolParam{Name: spec.Name, Parameters: params, Strict: openaiapi.Bool(true)}
			if spec.Description != "" {
				f.Description = openaiapi.String(spec.Description)
			}
			out = append(out, responses.ToolUnionParam{OfFunction: f})
		default:
			return nil, fmt.Errorf("xai tool %q payload not supported: %T (functions only)", spec.Name, spec.Payload)
		}
	}
	return out, nil
}

func normalizeStrictObjectSchemas(v any) {
	switch x := v.(type) {
	case map[string]any:
		if schemaHasType(x, "object") {
			if _, ok := x["additionalProperties"]; !ok {
				x["additionalProperties"] = false
			}
			if props, ok := x["properties"].(map[string]any); ok && len(props) > 0 {
				originalRequired := requiredStringSet(x["required"])
				required := make([]string, 0, len(props))
				for name, child := range props {
					required = append(required, name)
					if _, ok := originalRequired[name]; !ok {
						allowSchemaNull(child)
					}
				}
				sort.Strings(required)
				x["required"] = required
			}
		}
		for _, child := range x {
			normalizeStrictObjectSchemas(child)
		}
	case []any:
		for _, child := range x {
			normalizeStrictObjectSchemas(child)
		}
	}
}

func schemaHasType(schema map[string]any, want string) bool {
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

func requiredStringSet(v any) map[string]struct{} {
	out := map[string]struct{}{}
	switch xs := v.(type) {
	case []any:
		for _, v := range xs {
			if s, ok := v.(string); ok {
				out[s] = struct{}{}
			}
		}
	case []string:
		for _, s := range xs {
			out[s] = struct{}{}
		}
	}
	return out
}

func allowSchemaNull(v any) {
	schema, ok := v.(map[string]any)
	if !ok {
		return
	}
	switch typ := schema["type"].(type) {
	case string:
		if typ != "null" {
			schema["type"] = []any{typ, "null"}
		}
	case []any:
		for _, v := range typ {
			if s, ok := v.(string); ok && s == "null" {
				return
			}
		}
		schema["type"] = append(typ, "null")
	}
}

// requestToolChoice uses only xAI-supported modes: auto (omit), required, none.
// Allowed is applied by filtering tools in requestTools, not via allowed_tools.
func requestToolChoice(snap threads.ToolOfferSnapshot) (*responses.ResponseNewParamsToolChoiceUnion, error) {
	if snap.Allowed != nil && len(snap.Allowed) == 0 {
		opt := param.NewOpt(responses.ToolChoiceOptionsNone)
		return &responses.ResponseNewParamsToolChoiceUnion{OfToolChoiceMode: opt}, nil
	}
	if snap.Required {
		opt := param.NewOpt(responses.ToolChoiceOptionsRequired)
		return &responses.ResponseNewParamsToolChoiceUnion{OfToolChoiceMode: opt}, nil
	}
	return nil, nil
}
