package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	gschema "github.com/google/jsonschema-go/jsonschema"
	openaiapi "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"

	"github.com/mackross/agentloom/threads"
)

const DefaultModel = "gpt-4.1-mini"

const (
	DefaultWebSocketMaxIdle = 2 * time.Minute
	DefaultWebSocketMaxAge  = 55 * time.Minute
)

type ResponsesTransport string

const (
	ResponsesTransportWebSocket ResponsesTransport = "websocket"
	ResponsesTransportSSE       ResponsesTransport = "sse"
)

type ResponsesStreamer struct {
	client            openaiapi.Client
	model             string
	Reasoning         shared.ReasoningParam
	ServiceTier       responses.ResponseNewParamsServiceTier
	Transport         ResponsesTransport
	OnOutputTextDelta func(string)

	// WebSocketMaxIdle controls how long an idle cached WebSocket connection is reused.
	// Values less than or equal to zero use DefaultWebSocketMaxIdle.
	WebSocketMaxIdle time.Duration
	// WebSocketMaxAge controls the maximum lifetime of a cached WebSocket connection.
	// Values less than or equal to zero use DefaultWebSocketMaxAge.
	WebSocketMaxAge time.Duration

	// UsePreviousResponseID enables WebSocket continuation requests. When enabled,
	// the streamer uses a prefix hash to detect append-only follow-up requests and
	// sends only the new input items with previous_response_id. The zero value means enabled.
	UsePreviousResponseID bool
	// DisablePreviousResponseID disables WebSocket continuation requests.
	DisablePreviousResponseID bool

	normalizers threads.ToolNormalizers

	mu                         sync.Mutex
	wsConn                     *responses.WebSocketConn
	wsCreatedAt                time.Time
	wsLastUsed                 time.Time
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
	conn             *responses.WebSocketConn
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
	return NewResponsesStreamerWithClient(openaiapi.NewClient(), model)
}

func NewResponsesStreamerWithClient(client openaiapi.Client, model string) *ResponsesStreamer {
	model = strings.TrimSpace(model)
	if model == "" {
		model = DefaultModel
	}
	return &ResponsesStreamer{
		client:           client,
		model:            model,
		Transport:        ResponsesTransportWebSocket,
		WebSocketMaxIdle: DefaultWebSocketMaxIdle,
		WebSocketMaxAge:  DefaultWebSocketMaxAge,
	}
}

func (*ResponsesStreamer) Capabilities() threads.StreamerCapabilities {
	return threads.StreamerCapabilities{AssistantPrefix: true, ToolResultSendPolicy: threads.ToolResultSendRequiresComplete}
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
	s.mu.Lock()
	conn := s.wsConn
	s.clearWebSocketConnLocked()
	s.mu.Unlock()
	if conn == nil {
		return nil
	}
	return conn.Close()
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
		if streamReq.conn != nil {
			s.dropWebSocketConn(streamReq.conn)
		}
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
		if streamReq.conn != nil {
			s.dropWebSocketConn(streamReq.conn)
		}
		return err
	}
	if streamReq.conn != nil {
		s.rememberContinuation(responseID, streamReq.inputItems, outputItems, streamReq.paramsHash)
	}
	return nil
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
		inputItems:       fullInputItems,
		paramsHash:       paramsHash,
		usedContinuation: usedContinuation,
	}

	switch s.Transport {
	case "", ResponsesTransportWebSocket:
		stream, conn, err := s.newWebSocketResponseStream(ctx, sendParams)
		if err != nil {
			return responseStreamRequest{}, err
		}
		sr.stream = stream
		sr.conn = conn
		return sr, nil
	case ResponsesTransportSSE:
		sr.stream = s.client.Responses.NewStreaming(ctx, sendParams)
		return sr, nil
	default:
		return responseStreamRequest{}, fmt.Errorf("openai responses transport %q not supported", s.Transport)
	}
}

func (s *ResponsesStreamer) newWebSocketResponseStream(ctx context.Context, params responses.ResponseNewParams) (*responses.WebSocketStream, *responses.WebSocketConn, error) {
	conn, err := s.webSocketConn(ctx)
	if err != nil {
		return nil, nil, err
	}

	stream, err := conn.New(ctx, params)
	if err == nil {
		return stream, conn, nil
	}
	if !shouldRetryWebSocketNewError(ctx, err) {
		return nil, nil, err
	}

	s.dropWebSocketConn(conn)
	conn, connErr := s.webSocketConn(ctx)
	if connErr != nil {
		return nil, nil, errors.Join(err, connErr)
	}
	stream, streamErr := conn.New(ctx, params)
	if streamErr != nil {
		s.dropWebSocketConn(conn)
		return nil, nil, errors.Join(err, streamErr)
	}
	return stream, conn, nil
}

func shouldRetryWebSocketNewError(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	return !strings.Contains(err.Error(), "another response stream is already active")
}

func shouldRetryResponseStreamError(ctx context.Context, err error, streamReq responseStreamRequest, emitted bool) bool {
	if err == nil || emitted || ctx.Err() != nil {
		return false
	}
	if streamReq.usedContinuation {
		return true
	}
	if streamReq.conn == nil {
		return false
	}
	return errors.Is(err, io.EOF) ||
		strings.Contains(err.Error(), "connection is closed")
}

func (s *ResponsesStreamer) webSocketConn(ctx context.Context) (*responses.WebSocketConn, error) {
	now := time.Now()

	s.mu.Lock()
	conn := s.wsConn
	if conn != nil && !s.webSocketConnExpiredLocked(now) {
		s.wsLastUsed = now
		s.mu.Unlock()
		return conn, nil
	}
	if conn != nil {
		s.clearWebSocketConnLocked()
	}
	s.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now = time.Now()
	if s.wsConn != nil {
		if s.webSocketConnExpiredLocked(now) {
			conn = s.wsConn
			s.clearWebSocketConnLocked()
			s.mu.Unlock()
			_ = conn.Close()
			s.mu.Lock()
		} else {
			s.wsLastUsed = now
			return s.wsConn, nil
		}
	}
	conn, err := s.client.Responses.ConnectWebSocket(ctx)
	if err != nil {
		return nil, err
	}
	s.wsConn = conn
	s.wsCreatedAt = now
	s.wsLastUsed = now
	return conn, nil
}

func (s *ResponsesStreamer) webSocketConnExpiredLocked(now time.Time) bool {
	if s.wsConn == nil {
		return false
	}
	maxIdle := s.WebSocketMaxIdle
	if maxIdle <= 0 {
		maxIdle = DefaultWebSocketMaxIdle
	}
	maxAge := s.WebSocketMaxAge
	if maxAge <= 0 {
		maxAge = DefaultWebSocketMaxAge
	}
	return maxIdle > 0 && now.Sub(s.wsLastUsed) > maxIdle || maxAge > 0 && now.Sub(s.wsCreatedAt) > maxAge
}

func (s *ResponsesStreamer) clearWebSocketConnLocked() {
	s.wsConn = nil
	s.wsCreatedAt = time.Time{}
	s.wsLastUsed = time.Time{}
}

func (s *ResponsesStreamer) dropWebSocketConn(conn *responses.WebSocketConn) {
	s.mu.Lock()
	if s.wsConn == conn {
		s.clearWebSocketConnLocked()
	}
	s.mu.Unlock()
	_ = conn.Close()
}

func (s *ResponsesStreamer) consumeResponseStream(stream responseStream, emit func(threads.Item) error) (string, responses.ResponseInputParam, error) {
	functionCalls := map[string]functionCallMeta{}
	customCalls := map[string]functionCallMeta{}
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
		case "response.custom_tool_call_input.delta":
			if event.Delta == "" {
				continue
			}
			callID, name := resolveFunctionCall(customCalls, event.ItemID, "")
			if err := emit(threads.ToolCallChunk{CallID: callID, Name: name, PayloadDelta: event.Delta}); err != nil {
				return "", nil, err
			}
		case "response.custom_tool_call_input.done":
			callID, name := resolveFunctionCall(customCalls, event.ItemID, "")
			call := threads.ToolCall{CallID: callID, Name: name, Payload: event.Input}
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
			rememberCustomCall(customCalls, event.Item)
		case "error":
			if event.Message != "" {
				return "", nil, fmt.Errorf("openai responses stream error: %s", event.Message)
			}
			return "", nil, fmt.Errorf("openai responses stream error")
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

func rememberCustomCall(customCalls map[string]functionCallMeta, item responses.ResponseOutputItemUnion) {
	if item.Type != "custom_tool_call" || item.ID == "" {
		return
	}
	meta := customCalls[item.ID]
	if item.CallID != "" {
		meta.callID = item.CallID
	}
	if item.Name != "" {
		meta.name = item.Name
	}
	customCalls[item.ID] = meta
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
