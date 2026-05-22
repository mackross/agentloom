package multitool

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"strings"

	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/tool"
)

type Mode string

const (
	ModeJSON Mode = "json"
	// ModeLark uses a grammar/custom-tool payload. OpenAI GPT-4-family models
	// commonly reject Responses custom tools; use a model/API path with custom
	// tool support (or ModeJSON) for those providers.
	ModeLark Mode = "lark"
)

const (
	DefaultName               = "tool"
	DefaultDescription        = "Call an available command with optional input."
	DefaultLarkDescription    = "Call an available command with optional input. Use a shell-like command line as the first line; the first word selects the command and remaining words are arguments. To provide input, put it after a blank line."
	DefaultCommandDescription = "Shell-like command line. The first word selects the command; remaining words are arguments. Quoting is supported, but shell expansion is not."
	DefaultInputDescription   = "Optional freeform input for the command. For process-like commands this is treated like stdin."
	LarkGrammar               = "start: command_line? input?\ncommand_line: /[^\\n]+/\ninput: /\\n\\n[\\s\\S]*/"
)

// Setup is the model-visible multitool surface. Changing it can change provider
// request bytes and invalidate KV-cache reuse.
type Setup struct {
	Name               string
	Description        string
	Mode               Mode
	CommandDescription string
	InputDescription   string
	Required           bool
}

// Config is hidden multitool setup. It can be replaced as a unit without
// changing the model-visible tool surface.
type Config struct {
	Subtools []Subtool
	Fallback FallbackHandler
}

func (c Config) WithSubtools(subtools ...Subtool) Config {
	c.Subtools = append(append([]Subtool(nil), c.Subtools...), subtools...)
	return c
}

func (c Config) WithFallback(h FallbackHandler) Config {
	c.Fallback = h
	return c
}

type Call struct {
	Command *string
	Args    []string
	Input   *string
}

type ToolCall struct {
	CallID  string
	Name    string
	Command *string
	Args    []string
	Input   *string
}

func (c ToolCall) toThreadToolCall(name, payload string) threads.ToolCall {
	return threads.ToolCall{
		CallID:  c.CallID,
		Name:    name,
		Payload: payload,
	}
}

func (c ToolCall) Call() Call {
	return Call{
		Command: c.Command,
		Args:    append([]string(nil), c.Args...),
		Input:   c.Input,
	}
}

type SubtoolSpec struct {
	Command     string
	Description string
	Usage       string
}

type Subtool interface {
	SubtoolSpec() SubtoolSpec
	HandleMultitoolCall(context.Context, *threads.Thread, ToolCall, tool.ReturnItem) (tool.Handling, error)
}

type Fallback struct {
	ToolCall ToolCall
	Subtools []SubtoolSpec
}

func (f Fallback) CommandList() string {
	if len(f.Subtools) == 0 {
		return "Available commands:\n  (none)"
	}
	lines := []string{"Available commands:"}
	for _, spec := range f.Subtools {
		line := "  " + spec.Command
		if spec.Usage != "" {
			line += " " + spec.Usage
		}
		if spec.Description != "" {
			line += " - " + spec.Description
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

type FallbackHandler interface {
	HandleMultitoolFallback(context.Context, *threads.Thread, ToolCall, Fallback, tool.ReturnItem) (tool.Handling, error)
}

type FallbackFunc func(context.Context, *threads.Thread, ToolCall, Fallback, tool.ReturnItem) (tool.Handling, error)

func (f FallbackFunc) HandleMultitoolFallback(ctx context.Context, thread *threads.Thread, raw ToolCall, fallback Fallback, ret tool.ReturnItem) (tool.Handling, error) {
	if f == nil {
		panic("multitool: nil fallback func")
	}
	return f(ctx, thread, raw, fallback, ret)
}

func StaticFallback(text string) FallbackHandler {
	return FallbackFunc(func(_ context.Context, _ *threads.Thread, call ToolCall, _ Fallback, ret tool.ReturnItem) (tool.Handling, error) {
		return tool.Handling{}, ret(result(call, text))
	})
}

type SubtoolFunc func(context.Context, *threads.Thread, ToolCall, tool.ReturnItem) (tool.Handling, error)

type funcSubtool struct {
	spec SubtoolSpec
	fn   SubtoolFunc
}

func Func(spec SubtoolSpec, fn SubtoolFunc) Subtool {
	if fn == nil {
		panic("multitool: nil subtool func")
	}
	return funcSubtool{spec: spec, fn: fn}
}

func (s funcSubtool) SubtoolSpec() SubtoolSpec { return s.spec }
func (s funcSubtool) HandleMultitoolCall(ctx context.Context, thread *threads.Thread, call ToolCall, ret tool.ReturnItem) (tool.Handling, error) {
	return s.fn(ctx, thread, call, ret)
}

type Tool struct {
	setup    Setup
	config   Config
	subtools map[string]Subtool
	order    []string
}

func New(setup Setup, config Config) *Tool {
	setup = normalizeSetup(setup)
	t := &Tool{setup: setup, config: cloneConfig(config), subtools: map[string]Subtool{}}
	for _, st := range t.config.Subtools {
		t.add(st)
	}
	return t
}

func (t *Tool) Setup() Setup   { return t.setup }
func (t *Tool) Config() Config { return cloneConfig(t.config) }

func (t *Tool) WithConfig(config Config) *Tool { return New(t.setup, config) }

func (t *Tool) add(st Subtool) {
	if st == nil {
		panic("multitool: nil subtool")
	}
	spec := st.SubtoolSpec()
	cmd := strings.TrimSpace(spec.Command)
	if cmd == "" {
		panic("multitool: subtool command is empty")
	}
	t.addName(cmd, st, true)
}

func (t *Tool) addName(name string, st Subtool, primary bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	if _, exists := t.subtools[name]; !exists && primary {
		t.order = append(t.order, name)
	}
	t.subtools[name] = st
}

func (t *Tool) ToolsSnapshot(*threads.Thread) threads.ToolsSnapshot {
	return threads.ToolsSnapshot{
		Snapshot: t.Snapshot(),
		Handlers: []threads.ToolHandlerBinding{{Name: t.setup.Name}},
	}
}

func (t *Tool) Snapshot() threads.ToolOfferSnapshot {
	return threads.ToolOfferSnapshot{
		Offered:  []threads.ToolSpec{t.Spec()},
		Required: t.setup.Required,
	}
}

func (t *Tool) Spec() threads.ToolSpec { return t.spec() }

func (t *Tool) ResolveTool(ctx context.Context, thread *threads.Thread, raw threads.ToolCall, _ json.RawMessage) (threads.ToolDispatch, error) {
	var mu sync.Mutex
	inHandler := true
	var items []threads.Item
	ret := func(item tool.Item) error {
		if item == nil {
			return fmt.Errorf("multitool: nil result item")
		}
		mu.Lock()
		if inHandler {
			items = append(items, item)
			mu.Unlock()
			return nil
		}
		mu.Unlock()
		if thread == nil {
			return fmt.Errorf("multitool: async result requires event loop")
		}
		return thread.ReturnAsyncToolItem(context.Background(), raw.CallID, item)
	}
	handling, err := t.dispatch(ctx, thread, raw, ret)
	mu.Lock()
	inHandler = false
	items = append([]threads.Item(nil), items...)
	mu.Unlock()
	return threads.ToolDispatch{Started: true, Continue: handling.Continue, Recovery: handling.Recovery, Items: items}, err
}

func (t *Tool) HandleToolCall(ctx context.Context, thread *threads.Thread, call tool.Call, ret tool.ReturnItem) (tool.Handling, error) {
	if ret == nil {
		return tool.Handling{}, fmt.Errorf("multitool: nil return item handler")
	}
	return t.dispatch(ctx, thread, threads.ToolCall(call), ret)
}

func (t *Tool) dispatch(ctx context.Context, thread *threads.Thread, raw threads.ToolCall, ret tool.ReturnItem) (tool.Handling, error) {
	if raw.Name != t.setup.Name {
		return tool.Handling{}, fmt.Errorf("multitool: tool %q not found", raw.Name)
	}
	call, err := Parse(t.setup.Mode, raw.Payload)
	if err != nil {
		return tool.Handling{}, ret(result(newToolCall(raw, Call{}), fmt.Sprintf("invalid multitool call: %v", err)))
	}
	if call.Command == nil || strings.TrimSpace(*call.Command) == "" {
		return t.fallback(ctx, thread, raw, call, ret)
	}
	st, ok := t.subtools[*call.Command]
	if !ok {
		return t.fallback(ctx, thread, raw, call, ret)
	}
	return st.HandleMultitoolCall(ctx, thread, newToolCall(raw, call), ret)
}

func (t *Tool) Normalizer() threads.ToolNormalizer {
	return threads.ToolNormalizer{
		NormalizeSpec: func(spec threads.ToolSpec) (threads.ToolSpec, error) {
			if spec.Name != t.setup.Name {
				return spec, nil
			}
			return t.spec(), nil
		},
		NormalizeRequestToolCall:  t.normalizeToolCall,
		NormalizeResponseToolCall: t.normalizeToolCall,
	}
}

func (t *Tool) normalizeToolCall(call threads.ToolCall) (threads.ToolCall, error) {
	if call.Name != t.setup.Name {
		return call, nil
	}
	parsed, err := Parse(t.setup.Mode, call.Payload)
	if err != nil {
		return call, err
	}
	payload, err := Format(t.setup.Mode, parsed)
	if err != nil {
		return call, err
	}
	call.Payload = payload
	return call, nil
}

func (t *Tool) spec() threads.ToolSpec {
	payload := threads.ToolPayload(threads.ToolPayloadLark(LarkGrammar))
	if t.setup.Mode == ModeJSON {
		payload = threads.ToolPayloadFor[jsonPayload]()
	}
	return threads.ToolSpec{Name: t.setup.Name, Description: t.setup.Description, Payload: payload}
}

func (t *Tool) fallback(ctx context.Context, thread *threads.Thread, raw threads.ToolCall, call Call, ret tool.ReturnItem) (tool.Handling, error) {
	h := t.config.Fallback
	if h == nil {
		h = FallbackFunc(DefaultFallback)
	}
	toolCall := newToolCall(raw, call)
	return h.HandleMultitoolFallback(ctx, thread, toolCall, Fallback{ToolCall: toolCall, Subtools: t.subtoolSpecs()}, ret)
}

func (t *Tool) subtoolSpecs() []SubtoolSpec {
	specs := make([]SubtoolSpec, 0, len(t.order))
	for _, name := range t.order {
		specs = append(specs, t.subtools[name].SubtoolSpec())
	}
	return specs
}

func newToolCall(raw threads.ToolCall, call Call) ToolCall {
	return ToolCall{
		CallID:  raw.CallID,
		Name:    raw.Name,
		Command: call.Command,
		Args:    append([]string(nil), call.Args...),
		Input:   call.Input,
	}
}

func DefaultFallback(_ context.Context, _ *threads.Thread, call ToolCall, fallback Fallback, ret tool.ReturnItem) (tool.Handling, error) {
	if fallback.ToolCall.Command == nil || strings.TrimSpace(*fallback.ToolCall.Command) == "" {
		return tool.Handling{}, ret(result(call, fallback.CommandList()))
	}
	return tool.Handling{}, ret(result(call, fmt.Sprintf("unknown command: %s\n\n%s", *fallback.ToolCall.Command, fallback.CommandList())))
}

func result(call ToolCall, text string) threads.Item {
	return threads.ToolCallResult{CallID: call.CallID, Output: text}
}

func normalizeSetup(s Setup) Setup {
	if s.Name == "" {
		s.Name = DefaultName
	}
	if s.Mode == "" {
		s.Mode = ModeLark
	}
	if s.Description == "" {
		s.Description = DefaultDescription
		if s.Mode == ModeLark {
			s.Description = DefaultLarkDescription
		}
	}
	if s.CommandDescription == "" {
		s.CommandDescription = DefaultCommandDescription
	}
	if s.InputDescription == "" {
		s.InputDescription = DefaultInputDescription
	}
	return s
}

func cloneConfig(c Config) Config {
	return Config{Subtools: append([]Subtool(nil), c.Subtools...), Fallback: c.Fallback}
}
