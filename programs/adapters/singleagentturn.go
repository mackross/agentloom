package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/mackross/agentloom/programs"
	"github.com/mackross/agentloom/threads"
)

// ErrSingleAgentTurnNoOutput is returned when a typed SingleAgentTurn exhausts
// its attempts to obtain a call to the completion tool.
var ErrSingleAgentTurnNoOutput = errors.New("programs/adapters: single agent turn produced no structured output")

// ToolSet is a model-facing set of tools and their implementations.
//
// A threads/tool.Catalog implements ToolSet.
type ToolSet interface {
	threads.ToolProvider
	threads.ToolResolver
}

// SingleAgentTurn executes a Signature as one agent turn.
//
// The agent may make any number of automatically continuing tool calls. Run
// completes when the thread becomes idle.
//
// When O is an interface type such as any, Run does not require structured
// output and returns O's zero value when the agent finishes. For concrete O,
// Run adds a completion tool to Tools and returns the tool's decoded arguments.
// If the agent reaches idle without calling the completion tool, Run steers it
// to call that tool and sends another request.
//
// Tools is the exclusive tool surface for the duration of Run. A nil Tools
// value disables ordinary agent tools. The thread's previous tool provider and
// resolver are restored before Run returns.
type SingleAgentTurn[I, O any] struct {
	Signature programs.Signature[I, O]
	Tools     ToolSet

	// CompletionName is the model-facing completion tool name. If empty,
	// "submit_output" is used. It must not conflict with a tool in Tools.
	CompletionName string
	// CompletionDescription is the completion tool description. If empty, a
	// default final-output description is used.
	CompletionDescription string
	// MaxRetries is the number of times the model may be steered back to the
	// completion tool after ending without calling it. The zero value uses the
	// default of 10. Negative values disable retries.
	MaxRetries int
}

// Run executes the agent turn on t.
func (a SingleAgentTurn[I, O]) Run(ctx context.Context, t threads.Thread, input I) (O, error) {
	var zero O

	hasOutput := singleAgentTurnHasOutput[O]()
	completionName := a.completionName()
	prompt, err := a.prompt(input, hasOutput, completionName)
	if err != nil {
		return zero, err
	}

	var installed ToolSet = a.Tools
	var completion *toolJSONOutputTool[O]
	var agentTools *singleAgentToolSet[O]
	if hasOutput {
		if a.completionNameConflicts(t, completionName) {
			return zero, fmt.Errorf("programs/adapters: completion tool %q conflicts with agent tools", completionName)
		}
		completion = newToolJSONOutputTool[O](completionName, a.completionDescription(), a.maxRetries())
		agentTools = &singleAgentToolSet[O]{base: a.Tools, completion: completion}
		installed = agentTools
	}

	oldProvider := t.ToolProvider()
	oldResolver := t.ToolResolver()
	t.SetToolProvider(installed)
	t.SetToolResolver(installed)
	defer func() {
		t.SetToolResolver(oldResolver)
		t.SetToolProvider(oldProvider)
	}()

	if a.Signature.Instruction != "" {
		t.QueueItem(threads.AssistantInstruction(a.Signature.Instruction))
	}
	t.QueueItem(threads.UserText(prompt))
	t.QueueItem(threads.SendItem{})

	attempts := 0
	for {
		if err := t.WaitUntilIdle(ctx); err != nil {
			return zero, err
		}
		if !hasOutput {
			return zero, nil
		}
		if out, ok := completion.output(); ok {
			return out, nil
		}
		if attempts >= a.maxRetries() {
			return zero, ErrSingleAgentTurnNoOutput
		}
		attempts++

		agentTools.completionOnly = true
		// Queue a new snapshot so the next request offers only the required
		// completion tool, even though the provider object itself is unchanged.
		t.SetToolProvider(agentTools)
		t.QueueItem(threads.UserText(a.completionHint(completionName)))
		t.QueueItem(threads.SendItem{})
	}
}

func singleAgentTurnHasOutput[O any]() bool {
	t := reflect.TypeFor[O]()
	return t != nil && t.Kind() != reflect.Interface
}

func (a SingleAgentTurn[I, O]) prompt(input I, hasOutput bool, completionName string) (string, error) {
	inputJSON, err := a.Signature.InputJSON(input)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	if a.Signature.Name != "" {
		fmt.Fprintf(&b, "Signature: %s\n\n", a.Signature.Name)
	}
	b.WriteString("Input JSON:\n")
	b.Write(inputJSON)
	b.WriteString("\n\nUse the available tools as needed to complete the task.")
	if hasOutput {
		fmt.Fprintf(&b, " When the task is complete, call %q with the final structured output.", completionName)
	}
	return b.String(), nil
}

func (a SingleAgentTurn[I, O]) completionName() string {
	if a.CompletionName != "" {
		return a.CompletionName
	}
	return defaultToolJSONName
}

func (a SingleAgentTurn[I, O]) completionDescription() string {
	if a.CompletionDescription != "" {
		return a.CompletionDescription
	}
	return defaultToolJSONDescription
}

func (a SingleAgentTurn[I, O]) maxRetries() int {
	if a.MaxRetries == 0 {
		return defaultToolJSONMaxRetries
	}
	return a.MaxRetries
}

func (a SingleAgentTurn[I, O]) completionHint(name string) string {
	return fmt.Sprintf("The agent turn finished without calling %q. Call %q now with the final structured output. Do not return a text response.", name, name)
}

func (a SingleAgentTurn[I, O]) completionNameConflicts(t threads.Thread, name string) bool {
	if a.Tools == nil {
		return false
	}
	snapshot := a.Tools.ToolsSnapshot(t)
	for _, spec := range snapshot.Snapshot.Offered {
		if spec.Name == name {
			return true
		}
	}
	for _, binding := range snapshot.Handlers {
		if binding.Name == name {
			return true
		}
	}
	return false
}

type singleAgentToolSet[O any] struct {
	base           ToolSet
	completion     *toolJSONOutputTool[O]
	completionOnly bool
}

func (s *singleAgentToolSet[O]) ToolsSnapshot(t threads.Thread) threads.ToolsSnapshot {
	if s.completionOnly {
		return s.completion.ToolsSnapshot(t)
	}

	var snapshot threads.ToolsSnapshot
	if s.base != nil {
		snapshot = s.base.ToolsSnapshot(t)
	}
	completion := s.completion.ToolsSnapshot(t)

	snapshot.Snapshot.Offered = append(
		append([]threads.ToolSpec(nil), snapshot.Snapshot.Offered...),
		completion.Snapshot.Offered...,
	)
	if snapshot.Snapshot.Allowed != nil {
		snapshot.Snapshot.Allowed = append(
			append([]string(nil), snapshot.Snapshot.Allowed...),
			s.completion.spec.Name,
		)
	}
	parallel := false
	snapshot.Snapshot.Parallel = &parallel
	snapshot.Handlers = append(
		append([]threads.ToolHandlerBinding(nil), snapshot.Handlers...),
		completion.Handlers...,
	)
	return snapshot
}

func (s *singleAgentToolSet[O]) ResolveTool(ctx context.Context, t threads.Thread, call threads.ToolCall, load json.RawMessage) (threads.ToolDispatch, error) {
	if call.Name == s.completion.spec.Name {
		return s.completion.ResolveTool(ctx, t, call, load)
	}

	if s.completionOnly || s.base == nil {
		return threads.ToolDispatch{}, fmt.Errorf("tool %q not found", call.Name)
	}
	return s.base.ResolveTool(ctx, t, call, load)
}
