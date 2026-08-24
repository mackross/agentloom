package tool

import (
	"encoding/json"
	"fmt"
	"strings"

	gschema "github.com/google/jsonschema-go/jsonschema"

	"github.com/mackross/agentloom/threads"
)

const DefaultJSONValidationMaxRetries = 2

// JSONValidation validates finalized model-generated tool arguments before a
// handler can perform side effects. It is immutable after construction and safe
// to embed in typed tool handlers shared by multiple threads.
type JSONValidation struct {
	Schema *gschema.Schema
	// MaxRetries is unlimited when negative.
	MaxRetries int

	resolved *gschema.Resolved
}

func NewJSONValidation(schema PayloadJSONSchema, maxRetries int) JSONValidation {
	raw := gschema.Schema(schema)
	cloned := raw.CloneSchemas()
	if cloned == nil {
		cloned = &raw
	}
	resolved, err := cloned.Resolve(nil)
	if err != nil {
		panic(fmt.Sprintf("tool.NewJSONValidation: %v", err))
	}
	return JSONValidation{Schema: cloned, MaxRetries: maxRetries, resolved: resolved}
}

// ValidateInto validates call.Payload and decodes it into dst. A non-nil result
// is model-visible and retryable while the configured retry budget remains.
// buildHint, when non-nil, owns the complete steering hint text.
func (v *JSONValidation) ValidateInto(thread threads.Thread, call Call, dst any, buildHint func(error) string) (*threads.ToolCallResult, threads.ToolContinue) {
	err := v.validateInto(call.Payload, dst)
	if err == nil {
		return nil, threads.ToolContinueAuto
	}

	result := ResultError(call, err).(threads.ToolCallResult)
	attempt := nextJSONValidationRetryAttempt(thread, call)
	if v.MaxRetries >= 0 && attempt > v.MaxRetries {
		return &result, threads.ToolContinueManual
	}

	hint := ""
	if buildHint != nil {
		hint = buildHint(err)
	}
	if strings.TrimSpace(hint) == "" {
		hint = fmt.Sprintf(
			"<tool_call_hint tool=%q>\nThe previous tool call had invalid arguments: %v. Call %q exactly once with arguments matching its schema.\n</tool_call_hint>",
			call.Name,
			err,
			call.Name,
		)
	}
	result.SafeRollback = &threads.ToolCallSafeRollback{
		SteeringHint: hint,
		RetryAttempt: attempt,
		MaxRetries:   v.MaxRetries,
	}
	return &result, threads.ToolContinueAuto
}

func (v *JSONValidation) validateInto(raw string, dst any) error {
	if strings.TrimSpace(raw) == "" {
		raw = "{}"
	}

	var instance any
	if err := json.Unmarshal([]byte(raw), &instance); err != nil {
		return fmt.Errorf("invalid JSON arguments: %w", err)
	}
	if err := v.resolved.Validate(instance); err != nil {
		return fmt.Errorf("arguments do not match the tool schema: %w", err)
	}
	if err := json.Unmarshal([]byte(raw), dst); err != nil {
		return fmt.Errorf("arguments cannot be decoded by the tool: %w", err)
	}
	return nil
}

func nextJSONValidationRetryAttempt(thread threads.Thread, call Call) int {
	if thread == nil {
		return 1
	}
	snapshot, err := thread.Snapshot()
	if err != nil {
		return 1
	}
	segmentStart := 0
	for i, item := range snapshot.Items {
		if item.Type == "user_text" {
			segmentStart = i + 1
		}
	}

	type batch struct {
		callNames map[string]string
		results   map[string]*threads.ToolCallSafeRollback
	}
	var batches []batch
	callBatches := make(map[string]int)
	needBatch := true
	for _, item := range snapshot.Items[segmentStart:] {
		switch item.Type {
		case "tool_call":
			if needBatch {
				batches = append(batches, batch{
					callNames: make(map[string]string),
					results:   make(map[string]*threads.ToolCallSafeRollback),
				})
				needBatch = false
			}
			batchIndex := len(batches) - 1
			batches[batchIndex].callNames[item.ID] = item.Name
			callBatches[item.ID] = batchIndex
		case "tool_result":
			if batchIndex, ok := callBatches[item.ID]; ok {
				batches[batchIndex].results[item.ID] = item.SafeRollback
			}
			needBatch = true
		}
	}

	currentBatch, ok := callBatches[call.CallID]
	if !ok {
		currentBatch = len(batches)
	}
	for batchIndex := currentBatch - 1; batchIndex >= 0; batchIndex-- {
		candidate := batches[batchIndex]
		foundTool := false
		foundRollback := false
		maxAttempt := 0
		for callID, name := range candidate.callNames {
			if name != call.Name {
				continue
			}
			foundTool = true
			rollback, hasResult := candidate.results[callID]
			if !hasResult || rollback == nil {
				continue
			}
			foundRollback = true
			if rollback.RetryAttempt > maxAttempt {
				maxAttempt = rollback.RetryAttempt
			}
		}
		if foundRollback {
			return maxAttempt + 1
		}
		if foundTool {
			return 1
		}
	}
	return 1
}
