package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	gschema "github.com/google/jsonschema-go/jsonschema"

	"github.com/mackross/agentloom/threads"
)

const (
	BaseURL      = "http://localhost:11434"
	DefaultModel = "qwen3"

	reasoningProvider = "ollama.chat"
	maxErrorBody      = 1 << 20
)

// ChatStreamer streams Ollama's native /api/chat endpoint.
//
// Options are passed through as Ollama model options. Think accepts nil, a
// bool, or one of "low", "medium", "high", and "max". KeepAlive accepts an
// Ollama duration string or a numeric duration. Format accepts "json" or a
// JSON schema.
//
// Ollama does not expose native request controls equivalent to required tool
// choice or disabling parallel tool calls. By default, requests which require
// either guarantee are rejected. AllowBestEffortToolControls permits such
// requests while still filtering the offered tools, but cannot make the model
// honor the unsupported control.
type ChatStreamer struct {
	client       *http.Client
	baseURL      *url.URL
	baseURLError error
	model        string

	Options   map[string]any
	Think     any
	KeepAlive any
	Format    json.RawMessage
	Truncate  *bool
	Shift     *bool
	Headers   http.Header

	AllowBestEffortToolControls bool
	OnOutputTextDelta           func(string)

	normalizers threads.ToolNormalizers
}

type chatRequest struct {
	Model     string          `json:"model"`
	Messages  []message       `json:"messages"`
	Stream    bool            `json:"stream"`
	Tools     []tool          `json:"tools,omitempty"`
	Options   map[string]any  `json:"options,omitempty"`
	Think     any             `json:"think,omitempty"`
	KeepAlive any             `json:"keep_alive,omitempty"`
	Format    json.RawMessage `json:"format,omitempty"`
	Truncate  *bool           `json:"truncate,omitempty"`
	Shift     *bool           `json:"shift,omitempty"`
}

type message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	Thinking   string     `json:"thinking,omitempty"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolName   string     `json:"tool_name,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type toolCall struct {
	ID       string       `json:"id,omitempty"`
	Function toolFunction `json:"function"`
}

type toolFunction struct {
	Index     *int            `json:"index,omitempty"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type tool struct {
	Type     string         `json:"type"`
	Function toolDefinition `json:"function"`
}

type toolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type chatResponse struct {
	Message message `json:"message"`
	Done    bool    `json:"done"`
	Error   string  `json:"error,omitempty"`
}

// APIError is returned for a non-successful Ollama HTTP response.
type APIError struct {
	StatusCode int
	Status     string
	Message    string
}

func (e *APIError) Error() string {
	switch {
	case e.Status != "" && e.Message != "":
		return fmt.Sprintf("ollama: %s: %s", e.Status, e.Message)
	case e.Status != "":
		return "ollama: " + e.Status
	case e.Message != "":
		return "ollama: " + e.Message
	default:
		return fmt.Sprintf("ollama: HTTP status %d", e.StatusCode)
	}
}

// NewChatStreamer constructs a streamer using OLLAMA_HOST, or BaseURL when
// OLLAMA_HOST is unset.
func NewChatStreamer(model string) *ChatStreamer {
	base, err := baseURLFromEnvironment()
	s := NewChatStreamerWithClient(http.DefaultClient, base, model)
	s.baseURLError = err
	return s
}

// NewChatStreamerWithClient constructs a streamer with an explicit HTTP
// client and Ollama server URL. baseURL should identify the server root, such
// as http://localhost:11434; a trailing /api is also accepted.
func NewChatStreamerWithClient(client *http.Client, baseURL *url.URL, model string) *ChatStreamer {
	if client == nil {
		client = http.DefaultClient
	}
	model = strings.TrimSpace(model)
	if model == "" {
		model = DefaultModel
	}
	return &ChatStreamer{
		client:  client,
		baseURL: cloneURL(baseURL),
		model:   model,
	}
}

func (*ChatStreamer) Capabilities() threads.StreamerCapabilities {
	return threads.StreamerCapabilities{
		AssistantPrefix: true,
		Reasoning:       threads.ReasoningForCurrentTurn(reasoningProvider),
	}
}

var syntheticCallSequence atomic.Uint64

func (*ChatStreamer) SyntheticToolCallID() string {
	return fmt.Sprintf("call_%x_%x", time.Now().UnixNano(), syntheticCallSequence.Add(1))
}

func (s *ChatStreamer) RegisterToolNormalizer(name string, normalizer threads.ToolNormalizer) {
	s.normalizers.RegisterToolNormalizer(name, normalizer)
}

func (s *ChatStreamer) UnregisterToolNormalizer(name string) {
	s.normalizers.UnregisterToolNormalizer(name)
}

func (s *ChatStreamer) StreamReq(req threads.Req, emit func(threads.Item) error) error {
	return s.StreamReqContext(context.Background(), req, emit)
}

func (s *ChatStreamer) StreamReqContext(ctx context.Context, req threads.Req, emit func(threads.Item) error) error {
	if s.baseURLError != nil {
		return fmt.Errorf("ollama host: %w", s.baseURLError)
	}
	if s.baseURL == nil {
		return fmt.Errorf("ollama base URL is nil")
	}
	if err := validateThink(s.Think); err != nil {
		return err
	}

	req, err := s.normalizers.NormalizeReq(req)
	if err != nil {
		return err
	}
	messages, err := conversationMessages(req)
	if err != nil {
		return err
	}
	tools, err := requestTools(req.Tools)
	if err != nil {
		return err
	}
	if err := s.validateToolControls(req.Tools, len(tools) > 0); err != nil {
		return err
	}

	body, err := json.Marshal(chatRequest{
		Model:     s.model,
		Messages:  messages,
		Stream:    true,
		Tools:     tools,
		Options:   cloneMap(s.Options),
		Think:     s.Think,
		KeepAlive: s.KeepAlive,
		Format:    append(json.RawMessage(nil), s.Format...),
		Truncate:  s.Truncate,
		Shift:     s.Shift,
	})
	if err != nil {
		return fmt.Errorf("ollama chat request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, s.chatURL().String(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("ollama chat request: %w", err)
	}
	for name, values := range s.Headers {
		for _, value := range values {
			httpReq.Header.Add(name, value)
		}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/x-ndjson")

	resp, err := s.client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return decodeAPIError(resp)
	}
	return s.decodeStream(resp.Body, emit)
}

func (s *ChatStreamer) validateToolControls(snap threads.ToolOfferSnapshot, hasTools bool) error {
	if snap.Required && !hasTools {
		return fmt.Errorf("ollama tool choice cannot require an empty tool set")
	}
	if !hasTools {
		return nil
	}
	if snap.Required && !s.AllowBestEffortToolControls {
		return fmt.Errorf("ollama native chat API does not support required tool choice")
	}
	if snap.Parallel != nil && !*snap.Parallel && !s.AllowBestEffortToolControls {
		return fmt.Errorf("ollama native chat API cannot disable parallel tool calls")
	}
	return nil
}

func (s *ChatStreamer) decodeStream(r io.Reader, emit func(threads.Item) error) error {
	decoder := json.NewDecoder(r)
	var reasoning strings.Builder
	calls := map[string]*responseToolCall{}
	order := 0

	flushReasoning := func() error {
		if reasoning.Len() == 0 {
			return nil
		}
		text := reasoning.String()
		reasoning.Reset()
		return emit(threads.ReasoningItem{
			Provider:   reasoningProvider,
			Visibility: threads.ReasoningVisibilityText,
			Text:       text,
		})
	}
	flushCalls := func() error {
		if err := flushReasoning(); err != nil {
			return err
		}
		ordered := make([]*responseToolCall, 0, len(calls))
		for _, call := range calls {
			ordered = append(ordered, call)
		}
		sort.SliceStable(ordered, func(i, j int) bool {
			if ordered[i].hasIndex != ordered[j].hasIndex {
				return ordered[i].hasIndex
			}
			if ordered[i].hasIndex && ordered[i].index != ordered[j].index {
				return ordered[i].index < ordered[j].index
			}
			return ordered[i].order < ordered[j].order
		})
		for _, state := range ordered {
			if state.name == "" {
				return fmt.Errorf("ollama tool call at stream position %d has no name", state.order)
			}
			callID := state.id
			if callID == "" {
				callID = s.SyntheticToolCallID()
			}
			payload := state.arguments
			if len(payload) == 0 {
				payload = json.RawMessage(`{}`)
			}
			call := threads.ToolCall{CallID: callID, Name: state.name, Payload: string(payload)}
			var err error
			call, err = s.normalizers.NormalizeResponseToolCall(call)
			if err != nil {
				return err
			}
			if err := emit(call); err != nil {
				return err
			}
		}
		return nil
	}

	for {
		var chunk chatResponse
		err := decoder.Decode(&chunk)
		if errors.Is(err, io.EOF) {
			return io.ErrUnexpectedEOF
		}
		if err != nil {
			return fmt.Errorf("ollama chat stream: %w", err)
		}
		if chunk.Error != "" {
			return fmt.Errorf("ollama chat stream: %s", chunk.Error)
		}
		if chunk.Message.Thinking != "" {
			reasoning.WriteString(chunk.Message.Thinking)
		}
		if chunk.Message.Content != "" {
			if err := flushReasoning(); err != nil {
				return err
			}
			if s.OnOutputTextDelta != nil {
				s.OnOutputTextDelta(chunk.Message.Content)
			}
			if err := emit(threads.AssistantText(chunk.Message.Content)); err != nil {
				return err
			}
		}
		for position, call := range chunk.Message.ToolCalls {
			key := responseToolCallKey(call, position)
			state := calls[key]
			if state == nil {
				state = &responseToolCall{order: order}
				order++
				calls[key] = state
			}
			if call.ID != "" {
				state.id = call.ID
			}
			if call.Function.Index != nil {
				state.hasIndex = true
				state.index = *call.Function.Index
			}
			if call.Function.Name != "" {
				state.name = call.Function.Name
			}
			if len(call.Function.Arguments) > 0 && string(call.Function.Arguments) != "null" {
				if err := validateArguments(call.Function.Arguments); err != nil {
					return fmt.Errorf("ollama tool call %q arguments: %w", state.name, err)
				}
				state.arguments = append(state.arguments[:0], call.Function.Arguments...)
			}
		}
		if chunk.Done {
			return flushCalls()
		}
	}
}

type responseToolCall struct {
	id        string
	name      string
	arguments json.RawMessage
	index     int
	hasIndex  bool
	order     int
}

func responseToolCallKey(call toolCall, position int) string {
	if call.Function.Index != nil {
		return fmt.Sprintf("index:%d", *call.Function.Index)
	}
	if call.ID != "" {
		return "id:" + call.ID
	}
	return fmt.Sprintf("position:%d", position)
}

func conversationMessages(req threads.Req) ([]message, error) {
	out := make([]message, 0, len(req.Items)+1)
	if req.Instruction != "" {
		out = append(out, message{Role: "system", Content: req.Instruction})
	}

	callNames := map[string]string{}
	var assistant *message
	flushAssistant := func() {
		if assistant == nil {
			return
		}
		out = append(out, *assistant)
		assistant = nil
	}
	currentAssistant := func() *message {
		if assistant == nil {
			assistant = &message{Role: "assistant"}
		}
		return assistant
	}

	for _, item := range req.Items {
		switch v := item.(type) {
		case threads.ReasoningItem:
			if v.Text == "" {
				return nil, fmt.Errorf("ollama reasoning input requires text")
			}
			currentAssistant().Thinking += v.Text
		case threads.UserText:
			flushAssistant()
			out = append(out, message{Role: "user", Content: string(v)})
		case threads.AssistantText:
			currentAssistant().Content += string(v)
		case threads.ToolCall:
			args, err := requestToolArguments(v.Name, v.Payload)
			if err != nil {
				return nil, err
			}
			callNames[v.CallID] = v.Name
			msg := currentAssistant()
			index := len(msg.ToolCalls)
			msg.ToolCalls = append(msg.ToolCalls, toolCall{
				ID: v.CallID,
				Function: toolFunction{
					Index:     &index,
					Name:      v.Name,
					Arguments: args,
				},
			})
		case threads.ToolCallResult:
			flushAssistant()
			name := callNames[v.CallID]
			if name == "" {
				return nil, fmt.Errorf("ollama tool result %q has no preceding tool call", v.CallID)
			}
			out = append(out, message{
				Role:       "tool",
				Content:    v.Output,
				ToolName:   name,
				ToolCallID: v.CallID,
			})
		default:
			return nil, fmt.Errorf("ollama request item not supported: %T", item)
		}
	}
	flushAssistant()
	return out, nil
}

func requestToolArguments(name, payload string) (json.RawMessage, error) {
	if payload == "" {
		return json.RawMessage(`{}`), nil
	}
	raw := json.RawMessage(payload)
	if err := validateArguments(raw); err != nil {
		return nil, fmt.Errorf("ollama tool call %q arguments: %w", name, err)
	}
	return append(json.RawMessage(nil), raw...), nil
}

func validateArguments(raw json.RawMessage) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return fmt.Errorf("must be a JSON object")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return err
	}
	if object == nil {
		return fmt.Errorf("must be a JSON object")
	}
	return nil
}

func requestTools(snap threads.ToolOfferSnapshot) ([]tool, error) {
	specs, err := requestToolSpecs(snap)
	if err != nil {
		return nil, err
	}
	out := make([]tool, 0, len(specs))
	for _, spec := range specs {
		schema, ok := spec.Payload.(threads.ToolPayloadJSONSchema)
		if !ok {
			return nil, fmt.Errorf("ollama tool %q payload not supported: %T", spec.Name, spec.Payload)
		}
		parameters, err := json.Marshal(gschema.Schema(schema))
		if err != nil {
			return nil, fmt.Errorf("ollama tool %q schema: %w", spec.Name, err)
		}
		out = append(out, tool{
			Type: "function",
			Function: toolDefinition{
				Name:        spec.Name,
				Description: spec.Description,
				Parameters:  parameters,
			},
		})
	}
	return out, nil
}

func requestToolSpecs(snap threads.ToolOfferSnapshot) ([]threads.ToolSpec, error) {
	if snap.Allowed == nil {
		return append([]threads.ToolSpec(nil), snap.Offered...), nil
	}
	if len(snap.Allowed) == 0 {
		return nil, nil
	}
	byName := make(map[string]threads.ToolSpec, len(snap.Offered))
	for _, spec := range snap.Offered {
		byName[spec.Name] = spec
	}
	out := make([]threads.ToolSpec, 0, len(snap.Allowed))
	for _, name := range snap.Allowed {
		spec, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("ollama allowed tool %q not offered", name)
		}
		out = append(out, spec)
	}
	return out, nil
}

func validateThink(value any) error {
	switch v := value.(type) {
	case nil, bool:
		return nil
	case string:
		switch v {
		case "low", "medium", "high", "max":
			return nil
		default:
			return fmt.Errorf("ollama think level %q not supported", v)
		}
	default:
		return fmt.Errorf("ollama think value not supported: %T", value)
	}
}

func decodeAPIError(resp *http.Response) error {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	if err != nil {
		return fmt.Errorf("ollama: %s: read error response: %w", resp.Status, err)
	}
	var payload struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &payload)
	if payload.Error == "" {
		payload.Error = strings.TrimSpace(string(body))
	}
	return &APIError{
		StatusCode: resp.StatusCode,
		Status:     resp.Status,
		Message:    payload.Error,
	}
}

func (s *ChatStreamer) chatURL() *url.URL {
	base := cloneURL(s.baseURL)
	path := strings.TrimRight(base.Path, "/")
	rawPath := strings.TrimRight(base.EscapedPath(), "/")
	suffix := "/api/chat"
	if strings.HasSuffix(path, "/api") {
		suffix = "/chat"
	}
	base.Path = path + suffix
	if base.RawPath != "" {
		base.RawPath = rawPath + suffix
	}
	return base
}

func baseURLFromEnvironment() (*url.URL, error) {
	raw := strings.TrimSpace(os.Getenv("OLLAMA_HOST"))
	if raw == "" {
		raw = BaseURL
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	base, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("invalid URL %q", raw)
	}
	return base, nil
}

func cloneURL(in *url.URL) *url.URL {
	if in == nil {
		return nil
	}
	out := *in
	if in.User != nil {
		user := *in.User
		out.User = &user
	}
	return &out
}

func cloneMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
