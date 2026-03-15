package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

type Handler interface {
	HandleToolCall(context.Context, Call) Item
}

type HandlerFunc func(context.Context, Call) Item

func (f HandlerFunc) HandleToolCall(ctx context.Context, call Call) Item {
	if f == nil {
		panic("tool.HandlerFunc is nil")
	}
	item := f(ctx, call)
	if item == nil {
		panic(fmt.Sprintf("tool %q returned nil item", call.Name))
	}
	return item
}

type Catalog struct {
	entries    map[string]catalogEntry
	order      []string
	allowed    []string
	disallowed map[string]struct{}
	parallel   *bool
}

type catalogEntry struct {
	spec    Spec
	handler Handler
}

func NewCatalog() *Catalog { return &Catalog{} }

func (c *Catalog) Add(spec Spec, h Handler) *Catalog {
	c.init()
	name := strings.TrimSpace(spec.Name)
	if name == "" {
		panic("tool.Catalog.Add requires non-empty tool name")
	}
	if h == nil {
		panic("tool.Catalog.Add requires non-nil handler")
	}
	spec.Name = name
	if _, exists := c.entries[name]; !exists {
		c.order = append(c.order, name)
	}
	c.entries[name] = catalogEntry{
		spec:    cloneSpec(spec),
		handler: h,
	}
	return c
}

func (c *Catalog) AddFunc(spec Spec, fn HandlerFunc) *Catalog {
	if fn == nil {
		panic("tool.Catalog.AddFunc requires non-nil handler")
	}
	return c.Add(spec, fn)
}

func (c *Catalog) AllowAll() *Catalog {
	c.init()
	c.allowed = nil
	c.disallowed = nil
	return c
}

func (c *Catalog) AllowOnly(names ...string) *Catalog {
	c.init()
	c.allowed = normalizeNames(names...)
	c.disallowed = nil
	return c
}

func (c *Catalog) Disallow(names ...string) *Catalog {
	c.init()
	names = normalizeNames(names...)
	if c.allowed == nil {
		if c.disallowed == nil {
			c.disallowed = map[string]struct{}{}
		}
		for _, name := range names {
			c.disallowed[name] = struct{}{}
		}
		return c
	}
	if len(c.allowed) == 0 {
		return c
	}
	blocked := makeNameSet(names...)
	out := c.allowed[:0]
	for _, name := range c.allowed {
		if _, ok := blocked[name]; ok {
			continue
		}
		out = append(out, name)
	}
	c.allowed = out
	return c
}

func (c *Catalog) DisableAll() *Catalog {
	c.init()
	c.allowed = []string{}
	c.disallowed = nil
	return c
}

func (c *Catalog) SetParallel(v bool) *Catalog {
	c.init()
	c.parallel = &v
	return c
}

func (c *Catalog) Snapshot() Snapshot {
	c.init()
	snap := Snapshot{
		Offered: make([]Spec, 0, len(c.order)),
	}
	for _, name := range c.order {
		snap.Offered = append(snap.Offered, cloneSpec(c.entries[name].spec))
	}
	if c.allowed != nil {
		snap.Allowed = append([]string(nil), c.allowed...)
	} else if len(c.disallowed) > 0 {
		snap.Allowed = c.snapshotAllowed()
	}
	if c.parallel != nil {
		v := *c.parallel
		snap.Parallel = &v
	}
	return snap
}

func (c *Catalog) LoadTool(name string) (Handler, error) {
	c.init()
	entry, ok := c.entries[strings.TrimSpace(name)]
	if !ok {
		return nil, fmt.Errorf("tool %q not found", name)
	}
	return entry.handler, nil
}

func (c *Catalog) init() {
	if c.entries == nil {
		c.entries = map[string]catalogEntry{}
	}
}

func (c *Catalog) snapshotAllowed() []string {
	out := make([]string, 0, len(c.order))
	for _, name := range c.order {
		if _, blocked := c.disallowed[name]; blocked {
			continue
		}
		out = append(out, name)
	}
	return out
}

func normalizeNames(names ...string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

func makeNameSet(names ...string) map[string]struct{} {
	if len(names) == 0 {
		return nil
	}
	out := map[string]struct{}{}
	for _, name := range normalizeNames(names...) {
		out[name] = struct{}{}
	}
	return out
}

func cloneSpec(spec Spec) Spec {
	buf, err := json.Marshal(spec)
	if err != nil {
		panic(fmt.Sprintf("tool clone spec: %v", err))
	}
	var out Spec
	if err := json.Unmarshal(buf, &out); err != nil {
		panic(fmt.Sprintf("tool clone spec: %v", err))
	}
	return out
}
