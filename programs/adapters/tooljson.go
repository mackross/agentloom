package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/mackross/agentloom/programs"
	"github.com/mackross/agentloom/threads"
	threadtool "github.com/mackross/agentloom/threads/tool"
)

const (
	defaultToolJSONName        = "submit_output"
	defaultToolJSONDescription = "Submit the final structured output."
	defaultToolJSONMaxRetries  = 10
)

// ErrToolJSONNoOutput is returned when ToolJSON completes without its output
// tool being called successfully.
var ErrToolJSONNoOutput = errors.New("programs/adapters: tool JSON produced no output")

// ToolJSON executes a Signature by temporarily installing one required JSON
// tool and returning the arguments from the tool call.
//
// ToolJSON is intended for one-shot structured calls where the adapter owns the
// tool surface during Run. It restores the thread's previous tool provider and
// resolver before returning.
type ToolJSON[I, O any] struct {
	Signature programs.Signature[I, O]

	// Name is the model-facing output tool name. If empty, "submit_output" is used.
	Name string
	// Description is the model-facing output tool description. If empty, a default
	// final-output description is used.
	Description string
	// MaxRetries is the number of invalid output tool calls the model may correct.
	// The zero value uses the default of 10. Negative values disable retries.
	MaxRetries int
}

// Run executes c on t.
func (c ToolJSON[I, O]) Run(ctx context.Context, t threads.Thread, input I) (O, error) {
	var zero O

	prompt, err := c.prompt(input)
	if err != nil {
		return zero, err
	}

	tool := newToolJSONOutputTool[O](c.toolName(), c.toolDescription(), c.maxRetries())
	oldProvider := t.ToolProvider()
	oldResolver := t.ToolResolver()
	t.SetToolProvider(tool)
	t.SetToolResolver(tool)
	defer func() {
		t.SetToolResolver(oldResolver)
		t.SetToolProvider(oldProvider)
	}()

	if c.Signature.Instruction != "" {
		t.QueueItem(threads.AssistantInstruction(c.Signature.Instruction))
	}
	t.QueueItem(threads.UserText(prompt))
	t.QueueItem(threads.SendItem{})

	if err := t.WaitUntilIdle(ctx); err != nil {
		return zero, err
	}
	out, ok := tool.output()
	if !ok {
		return zero, ErrToolJSONNoOutput
	}
	return out, nil
}

func (c ToolJSON[I, O]) prompt(input I) (string, error) {
	inputJSON, err := c.Signature.InputJSON(input)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if c.Signature.Name != "" {
		b.WriteString("Signature: ")
		b.WriteString(c.Signature.Name)
		b.WriteString("\n\n")
	}
	b.WriteString("Input JSON:\n")
	b.Write(inputJSON)
	b.WriteString("\n\nCall the required output tool with the final structured output.")
	return b.String(), nil
}

func (c ToolJSON[I, O]) toolName() string {
	if c.Name != "" {
		return c.Name
	}
	return defaultToolJSONName
}

func (c ToolJSON[I, O]) toolDescription() string {
	if c.Description != "" {
		return c.Description
	}
	return defaultToolJSONDescription
}

func (c ToolJSON[I, O]) maxRetries() int {
	if c.MaxRetries == 0 {
		return defaultToolJSONMaxRetries
	}
	return c.MaxRetries
}

type toolJSONOutputTool[O any] struct {
	spec       threads.ToolSpec
	maxRetries int

	mu       sync.Mutex
	out      O
	ok       bool
	failures int
}

func newToolJSONOutputTool[O any](name, description string, maxRetries int) *toolJSONOutputTool[O] {
	return &toolJSONOutputTool[O]{
		spec: threads.ToolSpec{
			Name:        name,
			Description: description,
			Payload:     threads.ToolPayloadFor[O](),
		},
		maxRetries: maxRetries,
	}
}

func (t *toolJSONOutputTool[O]) ToolsSnapshot(_ threads.Thread) threads.ToolsSnapshot {
	parallel := false
	return threads.ToolsSnapshot{
		Snapshot: threads.ToolOfferSnapshot{
			Offered:  []threads.ToolSpec{t.spec},
			Allowed:  []string{t.spec.Name},
			Parallel: &parallel,
			Required: true,
		},
		Handlers: []threads.ToolHandlerBinding{{Name: t.spec.Name}},
	}
}

func (t *toolJSONOutputTool[O]) ResolveTool(_ context.Context, _ threads.Thread, call threads.ToolCall, _ json.RawMessage) (threads.ToolDispatch, error) {
	if call.Name != t.spec.Name {
		return threads.ToolDispatch{}, fmt.Errorf("tool %q not found", call.Name)
	}
	var out O
	if err := call.UnmarshalJSON(&out); err != nil {
		return t.invalidDispatch(call, err), nil
	}
	t.mu.Lock()
	t.out = out
	t.ok = true
	t.mu.Unlock()
	return threads.ToolDispatch{
		Started:  true,
		Continue: threads.ToolContinueManual,
		Items:    []threads.Item{threadtool.ResultText(threadtool.Call(call), "ok")},
	}, nil
}

func (t *toolJSONOutputTool[O]) invalidDispatch(call threads.ToolCall, err error) threads.ToolDispatch {
	t.mu.Lock()
	t.failures++
	failure := t.failures
	t.mu.Unlock()

	wrapped := fmt.Errorf("tool %q payload: %w", call.Name, err)
	result := threadtool.ResultError(threadtool.Call(call), wrapped).(threads.ToolCallResult)
	if t.maxRetries >= 0 && failure <= t.maxRetries {
		result.SafeRollback = &threads.ToolCallSafeRollback{SteeringHint: fmt.Sprintf(
			"The previous %q tool call had invalid JSON arguments: %v. Call %q again with arguments matching the tool schema.",
			call.Name, wrapped, call.Name,
		)}
		return threads.ToolDispatch{
			Started: true,
			Items:   []threads.Item{result},
		}
	}
	return threads.ToolDispatch{
		Started:  true,
		Continue: threads.ToolContinueManual,
		Items:    []threads.Item{result},
	}
}

func (t *toolJSONOutputTool[O]) output() (O, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.out, t.ok
}
