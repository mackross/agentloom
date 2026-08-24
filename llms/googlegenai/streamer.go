package googlegenai

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	cachegemini "github.com/mackross/agentloom/llms/cache/gemini"
	"github.com/mackross/agentloom/llms/internal/streamerutil"
	"github.com/mackross/agentloom/threads"
	"google.golang.org/genai"
)

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
	return &GenerateContentStreamer{client: client, model: model}
}

func (*GenerateContentStreamer) Capabilities() threads.StreamerCapabilities {
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
		for _, cand := range resp.Candidates {
			if cand == nil || cand.Content == nil {
				continue
			}
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
	}
	return nil
}

func (s *GenerateContentStreamer) geminiRequestConfig(req threads.Req) (genai.GenerateContentConfig, error) {
	config := s.Config
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
			decls = append(decls, &genai.FunctionDeclaration{
				Name:                 spec.Name,
				Description:          spec.Description,
				ParametersJsonSchema: p,
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
