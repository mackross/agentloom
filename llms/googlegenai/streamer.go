package googlegenai

import (
	"context"
	"encoding/json"
	"fmt"

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
	return threads.StreamerCapabilities{}
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

	contents, err := requestContents(req)
	if err != nil {
		return err
	}

	config, err := s.generateContentConfig(req)
	if err != nil {
		return err
	}

	emittedCalls := map[string]bool{}
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
				if part.Text != "" {
					if s.OnOutputTextDelta != nil {
						s.OnOutputTextDelta(part.Text)
					}
					if err := emit(threads.AssistantText(part.Text)); err != nil {
						return err
					}
				}
				if part.FunctionCall != nil {
					call, err := functionCallItem(part.FunctionCall)
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

func (s *GenerateContentStreamer) generateContentConfig(req threads.Req) (genai.GenerateContentConfig, error) {
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
	toolConfig := requestToolConfig(req.Tools, len(tools) > 0)
	if toolConfig != nil {
		config.ToolConfig = toolConfig
	}
	if cached, ok := streamerutil.LastStringMetadata(req, cachegemini.CachedContentKey); ok {
		config.CachedContent = cached
	}
	return config, nil
}

func requestContents(req threads.Req) ([]*genai.Content, error) {
	var out []*genai.Content
	callNames := map[string]string{}
	appendPart := func(role genai.Role, part *genai.Part) {
		if len(out) > 0 && out[len(out)-1].Role == string(role) {
			out[len(out)-1].Parts = append(out[len(out)-1].Parts, part)
			return
		}
		out = append(out, genai.NewContentFromParts([]*genai.Part{part}, role))
	}

	for _, item := range req.Items {
		switch v := item.(type) {
		case threads.UserText:
			appendPart(genai.RoleUser, genai.NewPartFromText(string(v)))
		case threads.AssistantText:
			appendPart(genai.RoleModel, genai.NewPartFromText(string(v)))
		case threads.ToolCall:
			args, err := decodeObject(v.Payload)
			if err != nil {
				return nil, fmt.Errorf("googlegenai tool call %q payload: %w", v.Name, err)
			}
			callNames[v.CallID] = v.Name
			appendPart(genai.RoleModel, &genai.Part{FunctionCall: &genai.FunctionCall{
				ID:   v.CallID,
				Name: v.Name,
				Args: args,
			}})
		case threads.ToolCallResultable:
			name := callNames[v.ToolCallID()]
			if name == "" {
				name = v.ToolCallID()
			}
			response := map[string]any{"output": v.ToolOutput()}
			if data := v.ToolData(); data != nil {
				if isErr, _ := data["error"].(bool); isErr {
					response = map[string]any{"error": v.ToolOutput()}
				}
			}
			appendPart(genai.RoleUser, &genai.Part{FunctionResponse: &genai.FunctionResponse{
				ID:       v.ToolCallID(),
				Name:     name,
				Response: response,
			}})
		default:
			return nil, fmt.Errorf("googlegenai request item not supported: %T", item)
		}
	}
	return out, nil
}

func requestTools(snap threads.ToolOfferSnapshot) ([]*genai.Tool, error) {
	specs, err := filteredTools(snap)
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

func filteredTools(snap threads.ToolOfferSnapshot) ([]threads.ToolSpec, error) {
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

func requestToolConfig(snap threads.ToolOfferSnapshot, hasTools bool) *genai.ToolConfig {
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

func functionCallItem(call *genai.FunctionCall) (threads.ToolCall, error) {
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

func decodeObject(raw string) (map[string]any, error) {
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
