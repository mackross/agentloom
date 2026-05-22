package voicethread

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/mackross/agentloom/threads"
	toolpkg "github.com/mackross/agentloom/threads/tool"
)

// ReturnItem is called by tools when a tool result is ready. Tools may call it
// before Dispatch returns or later from a goroutine.
type ReturnItem func(threads.Item) error

// ToolRuntime adapts Agentloom-shaped tool snapshots and tool calls to the
// threadless Realtime spike.
type ToolRuntime interface {
	Snapshot() threads.ToolOfferSnapshot
	Dispatch(context.Context, threads.ToolCall, ReturnItem) (threads.ToolDispatch, error)
}

// CatalogRuntime adapts threads/tool.Catalog without requiring a threads.Thread.
type CatalogRuntime struct {
	Catalog *toolpkg.Catalog
}

// NewCatalogRuntime adapts catalog to ToolRuntime.
func NewCatalogRuntime(catalog *toolpkg.Catalog) CatalogRuntime {
	return CatalogRuntime{Catalog: catalog}
}

func (r CatalogRuntime) Snapshot() threads.ToolOfferSnapshot {
	if r.Catalog == nil {
		return threads.ToolOfferSnapshot{}
	}
	return threads.ToolOfferSnapshot(r.Catalog.Snapshot())
}

func (r CatalogRuntime) Dispatch(ctx context.Context, call threads.ToolCall, ret ReturnItem) (threads.ToolDispatch, error) {
	if r.Catalog == nil {
		return threads.ToolDispatch{}, fmt.Errorf("voicethread: nil tool catalog")
	}
	if ret == nil {
		return threads.ToolDispatch{}, fmt.Errorf("voicethread: nil tool return callback")
	}
	h, err := r.Catalog.LoadTool(call.Name)
	if err != nil {
		return threads.ToolDispatch{}, err
	}

	var mu sync.Mutex
	inHandler := true
	items := []threads.Item(nil)
	toolCall := toolpkg.Call(call)
	returnItem := func(item toolpkg.Item) error {
		if item == nil {
			return fmt.Errorf("tool %q returned nil item", call.Name)
		}
		mu.Lock()
		if inHandler {
			items = append(items, item)
			mu.Unlock()
			return nil
		}
		mu.Unlock()
		return ret(item)
	}
	handling, err := h.HandleToolCall(ctx, nil, toolCall, returnItem)
	mu.Lock()
	inHandler = false
	items = append([]threads.Item(nil), items...)
	mu.Unlock()
	return threads.ToolDispatch{
		Started:  true,
		Continue: handling.Continue,
		Recovery: handling.Recovery,
		Items:    items,
	}, err
}

// ProviderResolverRuntime adapts a ToolProvider and ToolResolver. It is mostly
// synchronous because Agentloom async tool return paths require a real Thread.
type ProviderResolverRuntime struct {
	Provider threads.ToolProvider
	Resolver threads.ToolResolver
}

// NewProviderResolverRuntime adapts provider and resolver to ToolRuntime.
func NewProviderResolverRuntime(provider threads.ToolProvider, resolver threads.ToolResolver) ProviderResolverRuntime {
	return ProviderResolverRuntime{Provider: provider, Resolver: resolver}
}

func (r ProviderResolverRuntime) Snapshot() threads.ToolOfferSnapshot {
	if r.Provider == nil {
		return threads.ToolOfferSnapshot{}
	}
	return r.Provider.ToolsSnapshot(nil).Snapshot
}

func (r ProviderResolverRuntime) Dispatch(ctx context.Context, call threads.ToolCall, _ ReturnItem) (threads.ToolDispatch, error) {
	if r.Resolver == nil {
		return threads.ToolDispatch{}, fmt.Errorf("voicethread: nil tool resolver")
	}
	var loadData json.RawMessage
	if r.Provider != nil {
		for _, binding := range r.Provider.ToolsSnapshot(nil).Handlers {
			if binding.Name == call.Name {
				loadData = binding.HandlerLoadData
				break
			}
		}
	}
	return r.Resolver.ResolveTool(ctx, nil, call, loadData)
}

func toolsForRealtime(snapshot threads.ToolOfferSnapshot) ([]map[string]any, error) {
	allowed := map[string]struct{}(nil)
	if snapshot.Allowed != nil {
		allowed = map[string]struct{}{}
		for _, name := range snapshot.Allowed {
			allowed[name] = struct{}{}
		}
	}

	tools := make([]map[string]any, 0, len(snapshot.Offered))
	for _, spec := range snapshot.Offered {
		if allowed != nil {
			if _, ok := allowed[spec.Name]; !ok {
				continue
			}
		}
		parameters, err := parametersForPayload(spec.Payload)
		if err != nil {
			return nil, fmt.Errorf("tool %q: %w", spec.Name, err)
		}
		tools = append(tools, map[string]any{
			"type":        "function",
			"name":        spec.Name,
			"description": spec.Description,
			"parameters":  parameters,
		})
	}
	return tools, nil
}

func parametersForPayload(payload threads.ToolPayload) (map[string]any, error) {
	switch p := payload.(type) {
	case nil:
		return emptyObjectSchema(), nil
	case threads.ToolPayloadJSONSchema:
		var out map[string]any
		b, err := json.Marshal(p)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(b, &out); err != nil {
			return nil, err
		}
		return normalizeObjectSchema(out), nil
	default:
		// Text/regexp/Lark payloads are represented as a single string argument for
		// the spike. A future integration can preserve richer parser semantics.
		return map[string]any{
			"type": "object",
			"properties": map[string]any{
				"input": map[string]any{"type": "string"},
			},
			"required":             []string{"input"},
			"additionalProperties": false,
		}, nil
	}
}

func emptyObjectSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"properties":           map[string]any{},
		"additionalProperties": false,
	}
}

const selfInterruptToolName = "self_interrupt"

func selfInterruptToolForRealtime() map[string]any {
	return map[string]any{
		"type":        "function",
		"name":        selfInterruptToolName,
		"description": "Interrupt your own current spoken response and restart from the current conversation context. Use this when you realize mid-response that you should stop speaking or correct direction before continuing.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"reason": map[string]any{
					"type":        "string",
					"description": "Why you are interrupting yourself.",
				},
			},
			"additionalProperties": false,
		},
	}
}

func normalizeObjectSchema(schema map[string]any) map[string]any {
	if len(schema) == 0 {
		return emptyObjectSchema()
	}
	if _, ok := schema["type"]; !ok {
		schema["type"] = "object"
	}
	if _, ok := schema["properties"]; !ok {
		schema["properties"] = map[string]any{}
	}
	if _, ok := schema["additionalProperties"]; !ok {
		schema["additionalProperties"] = false
	}
	return schema
}
