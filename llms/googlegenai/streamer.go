package googlegenai

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	gschema "github.com/google/jsonschema-go/jsonschema"
	cachegemini "github.com/mackross/agentloom/llms/cache/gemini"
	"github.com/mackross/agentloom/llms/internal/streamerutil"
	"github.com/mackross/agentloom/threads"
	"google.golang.org/genai"
)

// DefaultModel is the stable Gemini 3.8 Flash model.
const DefaultModel = "gemini-3.8-flash"

// GenerateContentStreamer adapts google.golang.org/genai streaming generate-content
// calls to threads.Streamer.
type GenerateContentStreamer struct {
	client *genai.Client
	model  string

	Config genai.GenerateContentConfig

	OnOutputTextDelta func(string)

	normalizers threads.ToolNormalizers
}

func NewGenerateContentStreamer(model string) *GenerateContentStreamer {
	ctx := context.Background()
	client, err := genai.NewClient(ctx, &genai.ClientConfig{Backend: genai.BackendGeminiAPI})
	if err != nil {
		panic(err)
	}
	return NewGenerateContentStreamerWithClient(client, model)
}

func NewGenerateContentStreamerWithClient(client *genai.Client, model string) *GenerateContentStreamer {
	model = strings.TrimSpace(model)
	if model == "" {
		model = DefaultModel
	}
	return &GenerateContentStreamer{client: client, model: model}
}

func (s *GenerateContentStreamer) Capabilities() threads.StreamerCapabilities {
	if isGemini38Flash(s.model) {
		return threads.StreamerCapabilities{Reasoning: threads.ReasoningForProvider("google.gemini")}
	}
	return threads.StreamerCapabilities{Reasoning: threads.ReasoningForCurrentTurn("google.gemini")}
}

func (*GenerateContentStreamer) SyntheticToolCallID() string {
	return fmt.Sprintf("call_%x", time.Now().UnixNano())
}

func (s *GenerateContentStreamer) RegisterToolNormalizer(name string, normalizer threads.ToolNormalizer) {
	s.normalizers.RegisterToolNormalizer(name, normalizer)
}

func (s *GenerateContentStreamer) UnregisterToolNormalizer(name string) {
	s.normalizers.UnregisterToolNormalizer(name)
}

func (s *GenerateContentStreamer) StreamReq(req threads.Req, emit func(threads.Item) error) error {
	return s.StreamReqContext(context.Background(), req, emit)
}

func (s *GenerateContentStreamer) StreamReqContext(ctx context.Context, req threads.Req, emit func(threads.Item) error) error {
	req, err := s.normalizers.NormalizeReq(req)
	if err != nil {
		return err
	}

	contents, err := conversationContents(req)
	if err != nil {
		return err
	}
	if isGemini38Flash(s.model) {
		if err := validateGemini38Contents(contents); err != nil {
			return err
		}
	}

	config, err := s.geminiRequestConfig(req)
	if err != nil {
		return err
	}

	emittedCalls := map[string]bool{}
	textTail := map[int32]bool{}
	for resp, err := range s.client.Models.GenerateContentStream(ctx, s.model, contents, &config) {
		if err != nil {
			return err
		}
		if resp.PromptFeedback != nil && resp.PromptFeedback.BlockReason != "" && resp.PromptFeedback.BlockReason != genai.BlockedReasonUnspecified {
			message := strings.TrimSpace(resp.PromptFeedback.BlockReasonMessage)
			if message == "" {
				return fmt.Errorf("googlegenai prompt blocked: %s", resp.PromptFeedback.BlockReason)
			}
			return fmt.Errorf("googlegenai prompt blocked: %s: %s", resp.PromptFeedback.BlockReason, message)
		}
		for _, cand := range resp.Candidates {
			if cand == nil {
				continue
			}
			if cand.Content != nil {
				for _, part := range cand.Content.Parts {
					if part == nil {
						continue
					}
					if part.Thought {
						textTail[cand.Index] = false
						if part.FunctionCall != nil {
							return fmt.Errorf("googlegenai thought signature is attached to a function call")
						}
						if err := emit(threads.ReasoningItem{Provider: "google.gemini", Visibility: threads.ReasoningVisibilitySummary, Summary: part.Text, Opaque: append([]byte(nil), part.ThoughtSignature...)}); err != nil {
							return err
						}
						continue
					}
					hasText, hasCall := part.Text != "", part.FunctionCall != nil
					if len(part.ThoughtSignature) > 0 {
						signatureOnly := reflect.DeepEqual(part, &genai.Part{ThoughtSignature: part.ThoughtSignature})
						if signatureOnly && textTail[cand.Index] {
							if err := emit(threads.ReasoningItem{Provider: "google.gemini", Visibility: threads.ReasoningVisibilityHidden, Opaque: append([]byte(nil), part.ThoughtSignature...)}); err != nil {
								return err
							}
							textTail[cand.Index] = false
							continue
						}
						if hasText == hasCall {
							return fmt.Errorf("googlegenai thought signature has no single associated part")
						}
						if err := emit(threads.ReasoningItem{Provider: "google.gemini", Visibility: threads.ReasoningVisibilityHidden, Opaque: append([]byte(nil), part.ThoughtSignature...)}); err != nil {
							return err
						}
					}
					if hasText {
						if s.OnOutputTextDelta != nil {
							s.OnOutputTextDelta(part.Text)
						}
						if err := emit(threads.AssistantText(part.Text)); err != nil {
							return err
						}
						textTail[cand.Index] = true
					}
					if hasCall {
						textTail[cand.Index] = false
						call, err := geminiToolCall(part.FunctionCall)
						if err != nil {
							return err
						}
						key := call.CallID
						if key == "" {
							key = call.Name + "\x00" + call.Payload
						}
						if emittedCalls[key] {
							continue
						}
						emittedCalls[key] = true
						call, err = s.normalizers.NormalizeResponseToolCall(call)
						if err != nil {
							return err
						}
						if err := emit(call); err != nil {
							return err
						}
					}
				}
			}
			if err := geminiFinishError(cand); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *GenerateContentStreamer) geminiRequestConfig(req threads.Req) (genai.GenerateContentConfig, error) {
	config := s.Config
	if isGemini38Flash(s.model) {
		if err := validateGemini38Config(config); err != nil {
			return genai.GenerateContentConfig{}, err
		}
	}
	if req.Instruction != "" {
		config.SystemInstruction = genai.NewContentFromText(req.Instruction, genai.RoleUser)
	}
	tools, err := requestTools(req.Tools)
	if err != nil {
		return genai.GenerateContentConfig{}, err
	}
	if len(tools) > 0 {
		config.Tools = append(append([]*genai.Tool(nil), config.Tools...), tools...)
	}
	toolConfig := geminiToolSelection(req.Tools, len(tools) > 0)
	if toolConfig != nil {
		config.ToolConfig = toolConfig
	}
	if cached, ok := streamerutil.LastStringMetadata(req, cachegemini.CachedContentKey); ok {
		config.CachedContent = cached
	}
	return config, nil
}

func isGemini38Flash(model string) bool {
	model = strings.TrimPrefix(strings.TrimSpace(model), "models/")
	return model == DefaultModel
}

func validateGemini38Config(config genai.GenerateContentConfig) error {
	switch {
	case config.CandidateCount < 0:
		return fmt.Errorf("googlegenai %s candidate_count must be non-negative", DefaultModel)
	case config.CandidateCount > 1:
		return fmt.Errorf("googlegenai %s does not support multiple candidates; use candidate_count 0 or 1", DefaultModel)
	}
	if config.ThinkingConfig == nil {
		return nil
	}
	switch config.ThinkingConfig.ThinkingLevel {
	case "", genai.ThinkingLevelUnspecified, genai.ThinkingLevelLow, genai.ThinkingLevelMedium, genai.ThinkingLevelHigh:
		return nil
	case genai.ThinkingLevelMinimal:
		return fmt.Errorf("googlegenai %s does not support minimal thinking; use low, medium, or high", DefaultModel)
	default:
		return fmt.Errorf("googlegenai %s has invalid thinking level %q", DefaultModel, config.ThinkingConfig.ThinkingLevel)
	}
}

func validateGemini38Contents(contents []*genai.Content) error {
	if len(contents) == 0 {
		return fmt.Errorf("googlegenai %s requires request content", DefaultModel)
	}

	var pendingCalls map[string]string
	for _, content := range contents {
		if content == nil {
			continue
		}
		calls := map[string]string{}
		responses := map[string]string{}
		for _, part := range content.Parts {
			if part == nil {
				continue
			}
			if call := part.FunctionCall; call != nil {
				if call.ID == "" || call.Name == "" {
					return fmt.Errorf("googlegenai %s function calls require id and name", DefaultModel)
				}
				if _, exists := calls[call.ID]; exists {
					return fmt.Errorf("googlegenai %s function call id %q is duplicated", DefaultModel, call.ID)
				}
				calls[call.ID] = call.Name
			}
			if response := part.FunctionResponse; response != nil {
				if response.ID == "" || response.Name == "" {
					return fmt.Errorf("googlegenai %s function responses require id and name", DefaultModel)
				}
				if _, exists := responses[response.ID]; exists {
					return fmt.Errorf("googlegenai %s function response id %q is duplicated", DefaultModel, response.ID)
				}
				responses[response.ID] = response.Name
			}
		}

		if len(responses) > 0 {
			if len(pendingCalls) != len(responses) {
				return fmt.Errorf("googlegenai %s function response count does not match preceding calls", DefaultModel)
			}
			for id, name := range responses {
				if pendingCalls[id] != name {
					return fmt.Errorf("googlegenai %s function response %q does not match preceding call name", DefaultModel, id)
				}
			}
			pendingCalls = nil
		}
		if len(calls) > 0 {
			if pendingCalls != nil {
				return fmt.Errorf("googlegenai %s function calls are missing responses", DefaultModel)
			}
			pendingCalls = calls
		}
	}

	last := contents[len(contents)-1]
	if last == nil || last.Role != string(genai.RoleUser) {
		return fmt.Errorf("googlegenai %s does not support a prefilled model turn", DefaultModel)
	}
	hasText, hasResponse := false, false
	for _, part := range last.Parts {
		if part == nil {
			continue
		}
		hasText = hasText || strings.TrimSpace(part.Text) != ""
		hasResponse = hasResponse || part.FunctionResponse != nil
	}
	if !hasText && !hasResponse {
		return fmt.Errorf("googlegenai %s requires non-empty text in the final user turn", DefaultModel)
	}
	if pendingCalls != nil && !hasResponse {
		return fmt.Errorf("googlegenai %s function calls are missing responses", DefaultModel)
	}
	return nil
}

func geminiFinishError(cand *genai.Candidate) error {
	if cand == nil {
		return nil
	}
	switch cand.FinishReason {
	case "", genai.FinishReasonUnspecified, genai.FinishReasonStop:
		return nil
	}
	message := strings.TrimSpace(cand.FinishMessage)
	if message == "" {
		return fmt.Errorf("googlegenai generation stopped: %s", cand.FinishReason)
	}
	return fmt.Errorf("googlegenai generation stopped: %s: %s", cand.FinishReason, message)
}

func conversationContents(req threads.Req) ([]*genai.Content, error) {
	var out []*genai.Content
	callNames := map[string]string{}
	appendPart := func(role genai.Role, part *genai.Part) {
		if len(out) > 0 && out[len(out)-1].Role == string(role) {
			out[len(out)-1].Parts = append(out[len(out)-1].Parts, part)
			return
		}
		out = append(out, genai.NewContentFromParts([]*genai.Part{part}, role))
	}
	var pendingSignature []byte
	appendAssistantPart := func(part *genai.Part) {
		part.ThoughtSignature = pendingSignature
		pendingSignature = nil
		appendPart(genai.RoleModel, part)
	}

	for i, item := range req.Items {
		if len(pendingSignature) > 0 {
			if _, text := item.(threads.AssistantText); !text {
				if _, call := item.(threads.ToolCall); !call {
					return nil, fmt.Errorf("googlegenai thought signature is not followed by assistant text or tool call")
				}
			}
		}
		switch v := item.(type) {
		case threads.UserText:
			appendPart(genai.RoleUser, genai.NewPartFromText(string(v)))
		case threads.AssistantText:
			appendAssistantPart(genai.NewPartFromText(string(v)))
		case threads.ReasoningItem:
			if v.Visibility == threads.ReasoningVisibilitySummary {
				appendPart(genai.RoleModel, &genai.Part{Text: v.Summary, Thought: true, ThoughtSignature: append([]byte(nil), v.Opaque...)})
				continue
			}
			if v.Visibility != threads.ReasoningVisibilityHidden || len(v.Opaque) == 0 || len(pendingSignature) > 0 {
				return nil, fmt.Errorf("googlegenai reasoning item cannot be associated")
			}
			if i+1 < len(req.Items) {
				switch req.Items[i+1].(type) {
				case threads.AssistantText, threads.ToolCall:
					pendingSignature = append([]byte(nil), v.Opaque...)
					continue
				}
			}
			if i > 0 {
				if _, ok := req.Items[i-1].(threads.AssistantText); ok && len(out) > 0 {
					parts := out[len(out)-1].Parts
					if part := parts[len(parts)-1]; part.Text != "" && len(part.ThoughtSignature) == 0 {
						part.ThoughtSignature = append([]byte(nil), v.Opaque...)
						continue
					}
				}
			}
			return nil, fmt.Errorf("googlegenai reasoning item cannot be associated")
		case threads.ToolCall:
			args, err := geminiToolArguments(v.Payload)
			if err != nil {
				return nil, fmt.Errorf("googlegenai tool call %q payload: %w", v.Name, err)
			}
			callNames[v.CallID] = v.Name
			appendAssistantPart(&genai.Part{FunctionCall: &genai.FunctionCall{
				ID:   v.CallID,
				Name: v.Name,
				Args: args,
			}})
		case threads.ToolCallResult:
			name := callNames[v.CallID]
			if name == "" {
				name = v.CallID
			}
			response := map[string]any{"output": v.Output}
			appendPart(genai.RoleUser, &genai.Part{FunctionResponse: &genai.FunctionResponse{
				ID:       v.CallID,
				Name:     name,
				Response: response,
			}})
		default:
			return nil, fmt.Errorf("googlegenai request item not supported: %T", item)
		}
	}
	if len(pendingSignature) > 0 {
		return nil, fmt.Errorf("googlegenai thought signature has no associated part")
	}
	return out, nil
}

func requestTools(snap threads.ToolOfferSnapshot) ([]*genai.Tool, error) {
	specs, err := requestToolSpecs(snap)
	if err != nil {
		return nil, err
	}
	if len(specs) == 0 {
		return nil, nil
	}

	decls := make([]*genai.FunctionDeclaration, 0, len(specs))
	for _, spec := range specs {
		if spec.Payload == nil {
			return nil, fmt.Errorf("googlegenai tool %q payload not supported: %T", spec.Name, spec.Payload)
		}
		switch p := spec.Payload.(type) {
		case threads.ToolPayloadJSONSchema:
			schema := gschema.Schema(p)
			decls = append(decls, &genai.FunctionDeclaration{
				Name:                 spec.Name,
				Description:          spec.Description,
				ParametersJsonSchema: schema,
			})
		default:
			return nil, fmt.Errorf("googlegenai tool %q payload not supported: %T", spec.Name, spec.Payload)
		}
	}
	return []*genai.Tool{{FunctionDeclarations: decls}}, nil
}

func requestToolSpecs(snap threads.ToolOfferSnapshot) ([]threads.ToolSpec, error) {
	if len(snap.Allowed) == 0 {
		return snap.Offered, nil
	}
	byName := map[string]threads.ToolSpec{}
	for _, spec := range snap.Offered {
		byName[spec.Name] = spec
	}
	out := make([]threads.ToolSpec, 0, len(snap.Allowed))
	for _, name := range snap.Allowed {
		spec, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("googlegenai allowed tool %q not offered", name)
		}
		out = append(out, spec)
	}
	return out, nil
}

func geminiToolSelection(snap threads.ToolOfferSnapshot, hasTools bool) *genai.ToolConfig {
	if !hasTools {
		return nil
	}
	if !snap.Required && snap.Allowed == nil {
		return nil
	}
	mode := genai.FunctionCallingConfigModeAuto
	if snap.Required {
		mode = genai.FunctionCallingConfigModeAny
	} else if snap.Allowed != nil && len(snap.Allowed) == 0 {
		mode = genai.FunctionCallingConfigModeNone
	} else if len(snap.Allowed) > 0 {
		mode = genai.FunctionCallingConfigModeValidated
	}
	cfg := &genai.FunctionCallingConfig{Mode: mode}
	if len(snap.Allowed) > 0 {
		cfg.AllowedFunctionNames = append([]string(nil), snap.Allowed...)
	}
	return &genai.ToolConfig{FunctionCallingConfig: cfg}
}

func geminiToolCall(call *genai.FunctionCall) (threads.ToolCall, error) {
	payload, err := json.Marshal(call.Args)
	if err != nil {
		return threads.ToolCall{}, fmt.Errorf("googlegenai tool call %q args: %w", call.Name, err)
	}
	callID := call.ID
	if callID == "" {
		callID = call.Name
	}
	return threads.ToolCall{CallID: callID, Name: call.Name, Payload: string(payload)}, nil
}

func geminiToolArguments(raw string) (map[string]any, error) {
	if raw == "" {
		return map[string]any{}, nil
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}
