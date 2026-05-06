package threads

import "sync"

// ToolNormalizer adapts canonical tool specs/calls at streamer boundaries.
// Streamers may use this to present a provider-compatible form of a tool while
// normalizing returned tool calls back to the canonical form before emitting.
// For example, apply_patch may use an OpenAI context-free grammar tool for
// better performance, while JSON-only providers need a JSON-shaped equivalent;
// a normalizer lets the same thread transition between providers without
// changing the canonical tool seen by the application.
type ToolNormalizer struct {
	NormalizeSpec             func(ToolSpec) (ToolSpec, error)
	NormalizeRequestToolCall  func(ToolCall) (ToolCall, error)
	NormalizeResponseToolCall func(ToolCall) (ToolCall, error)
}

// ToolNormalizerRegistry is implemented by streamers that support registering
// per-tool boundary normalizers.
type ToolNormalizerRegistry interface {
	RegisterToolNormalizer(name string, normalizer ToolNormalizer)
	UnregisterToolNormalizer(name string)
}

// ToolNormalizers is a reusable ToolNormalizerRegistry implementation for streamers.
type ToolNormalizers struct {
	mu     sync.RWMutex
	byName map[string]ToolNormalizer
}

func (n *ToolNormalizers) RegisterToolNormalizer(name string, normalizer ToolNormalizer) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.byName == nil {
		n.byName = map[string]ToolNormalizer{}
	}
	n.byName[name] = normalizer
}

func (n *ToolNormalizers) UnregisterToolNormalizer(name string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.byName, name)
}

func (n *ToolNormalizers) NormalizeReq(req Req) (Req, error) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	if len(n.byName) == 0 {
		return req, nil
	}
	out := req
	out.Tools = cloneToolOfferSnapshot(req.Tools)
	for i, spec := range out.Tools.Offered {
		normalizer, ok := n.byName[spec.Name]
		if !ok || normalizer.NormalizeSpec == nil {
			continue
		}
		next, err := normalizer.NormalizeSpec(spec)
		if err != nil {
			return Req{}, err
		}
		out.Tools.Offered[i] = next
	}
	if len(req.Items) > 0 {
		out.Items = append([]Item(nil), req.Items...)
		for i, item := range out.Items {
			call, ok := item.(ToolCall)
			if !ok {
				continue
			}
			normalizer, ok := n.byName[call.Name]
			if !ok || normalizer.NormalizeRequestToolCall == nil {
				continue
			}
			next, err := normalizer.NormalizeRequestToolCall(call)
			if err != nil {
				return Req{}, err
			}
			out.Items[i] = next
		}
	}
	return out, nil
}

func (n *ToolNormalizers) NormalizeResponseToolCall(call ToolCall) (ToolCall, error) {
	n.mu.RLock()
	normalizer, ok := n.byName[call.Name]
	n.mu.RUnlock()
	if !ok || normalizer.NormalizeResponseToolCall == nil {
		return call, nil
	}
	return normalizer.NormalizeResponseToolCall(call)
}
