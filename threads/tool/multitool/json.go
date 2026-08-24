package multitool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/tool"
)

func jsonSubtool[T any](spec SubtoolSpec, toolName string, fn func(context.Context, threads.Thread, threads.ToolCall, T, tool.ReturnItem) (tool.Handling, error)) Subtool {
	if fn == nil {
		panic("multitool: nil JSON subtool handler")
	}
	validation := tool.NewJSONValidation(tool.PayloadFor[T]().(tool.PayloadJSONSchema), tool.DefaultJSONValidationMaxRetries)
	return Func(spec, func(ctx context.Context, thread threads.Thread, call ToolCall, ret tool.ReturnItem) (tool.Handling, error) {
		inner, args, handling, ok := parseJSONInput[T](thread, &validation, call, spec, toolName, ret)
		if !ok {
			return handling, nil
		}
		return fn(ctx, thread, inner, args, ret)
	})
}

// JSONHandler adapts an existing tool.Handler into a JSON multitool subtool.
//
// It is meant for hiding an ordinary JSON-shaped tool behind a multitool
// command. For a multitool call like:
//
//	lookup --fast
//
//	{"id":"123"}
//
// JSONHandler decodes the input into T, returns a model-visible error if
// decoding fails, canonicalizes the JSON, then invokes h.HandleToolCall with an
// inner threads.ToolCall. The inner call keeps the original CallID, uses
// toolName as its Name, and uses the canonical JSON as Payload. If toolName is
// empty, the parsed command name is used.
func JSONHandler[T any](spec SubtoolSpec, toolName string, h tool.Handler) Subtool {
	if h == nil {
		panic("multitool.JSONHandler requires non-nil handler")
	}
	return jsonSubtool(spec, toolName, func(ctx context.Context, thread threads.Thread, inner threads.ToolCall, _ T, ret tool.ReturnItem) (tool.Handling, error) {
		return h.HandleToolCall(ctx, thread, tool.Call(inner), ret)
	})
}

func parseJSONInput[T any](thread threads.Thread, validation *tool.JSONValidation, call ToolCall, spec SubtoolSpec, toolName string, ret tool.ReturnItem) (threads.ToolCall, T, tool.Handling, bool) {
	var zero T
	if toolName == "" {
		if call.Command != nil {
			toolName = *call.Command
		}
	}
	raw := ""
	if call.Input != nil {
		raw = *call.Input
	}
	var args T
	if raw == "" {
		raw = "{}"
	}
	validationCall := tool.Call(call.toThreadToolCall(call.Name, raw))
	result, continueMode := validation.ValidateInto(thread, validationCall, &args, func(err error) string {
		detail := strings.TrimPrefix(err.Error(), "invalid JSON arguments: ")
		output := fmt.Sprintf("invalid JSON input for command %q: %s", commandName(call), detail)
		return jsonInputErrorHint[T](call, spec, output)
	})
	if result != nil {
		detail := strings.TrimPrefix(result.Output, "invalid JSON arguments: ")
		result.Output = fmt.Sprintf("invalid JSON input for command %q: %s", commandName(call), detail)
		result.Data = nil
		_ = ret(*result)
		return threads.ToolCall{}, zero, tool.Handling{Continue: continueMode}, false
	}
	canonical, err := json.Marshal(args)
	if err != nil {
		_ = ret(tool.ResultError(validationCall, fmt.Errorf("encode JSON input for command %q: %w", commandName(call), err)))
		return threads.ToolCall{}, zero, tool.Handling{Continue: threads.ToolContinueManual}, false
	}
	return call.toThreadToolCall(toolName, string(canonical)), args, tool.Handling{}, true
}

func jsonInputErrorHint[T any](call ToolCall, spec SubtoolSpec, output string) string {
	schema := jsonInputSchema[T]()
	schemaText := ""
	if !strings.Contains(spec.Description, schema) && !strings.Contains(spec.Usage, schema) {
		schemaText = fmt.Sprintf(" matching this schema:\n\n%s", schema)
	}
	return fmt.Sprintf("\n\n<tool_call_hint tool=\"%s\" command=\"%s\">\n%s%s\n\nCall the tool again with command %q and JSON input%s\n</tool_call_hint>",
		call.Name,
		commandName(call),
		output,
		jsonSubtoolHintText(spec),
		commandName(call),
		schemaText,
	)
}

func jsonSubtoolHintText(spec SubtoolSpec) string {
	out := ""
	if spec.Description != "" {
		out += "\n\n" + spec.Description
	}
	if spec.Usage != "" {
		out += "\nUsage: " + spec.Usage
	}
	return out
}

func jsonInputSchema[T any]() string {
	buf, err := json.MarshalIndent(threads.ToolPayloadFor[T](), "", "  ")
	if err != nil {
		return "{}"
	}
	return string(buf)
}

func commandName(call ToolCall) string {
	if call.Command == nil {
		return ""
	}
	return *call.Command
}
