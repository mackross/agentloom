package tool

import (
	"context"
	"fmt"
	"strings"
	"testing"

	gschema "github.com/google/jsonschema-go/jsonschema"

	"github.com/mackross/agentloom/threads"
)

func TestJSONValidationRetriesMalformedAndSchemaInvalidArguments(t *testing.T) {
	schema := PayloadJSONSchema(gschema.Schema{
		Type: "object",
		Properties: map[string]*gschema.Schema{
			"mode": {Type: "string", Enum: []any{"read", "write"}},
		},
		Required: []string{"mode"},
	})
	validation := NewJSONValidation(schema, 2)
	thread := threads.New()

	type args struct {
		Mode string `json:"mode"`
	}

	firstCall := Call{CallID: "c1", Name: "configure", Payload: `{"mode":`}
	var firstArgs args
	first, firstContinue := validation.ValidateInto(thread, firstCall, &firstArgs, nil)
	if first == nil || first.SafeRollback == nil {
		t.Fatalf("malformed JSON failure = %#v, want rollbackable result", first)
	}
	if firstContinue != threads.ToolContinueAuto {
		t.Fatalf("malformed JSON continue = %q, want auto", firstContinue)
	}
	if first.SafeRollback.RetryAttempt != 1 || first.SafeRollback.MaxRetries != 2 {
		t.Fatalf("malformed JSON rollback metadata = %#v", first.SafeRollback)
	}
	thread.QueueItem(threads.ToolCall(firstCall))
	thread.QueueItem(*first)

	secondCall := Call{CallID: "c2", Name: "configure", Payload: `{"mode":"admin"}`}
	var secondArgs args
	second, secondContinue := validation.ValidateInto(thread, secondCall, &secondArgs, nil)
	if second == nil || second.SafeRollback == nil {
		t.Fatalf("schema failure = %#v, want rollbackable result", second)
	}
	if secondContinue != threads.ToolContinueAuto {
		t.Fatalf("schema failure continue = %q, want auto", secondContinue)
	}
	if second.SafeRollback.RetryAttempt != 2 || second.SafeRollback.MaxRetries != 2 {
		t.Fatalf("schema rollback metadata = %#v", second.SafeRollback)
	}
	thread.QueueItem(threads.ToolCall(secondCall))
	thread.QueueItem(*second)

	thirdCall := Call{CallID: "c3", Name: "configure", Payload: `{}`}
	var thirdArgs args
	third, thirdContinue := validation.ValidateInto(thread, thirdCall, &thirdArgs, nil)
	if third == nil {
		t.Fatal("exhausted failure = nil, want terminal result")
	}
	if third.SafeRollback != nil {
		t.Fatalf("exhausted failure remained rollbackable: %#v", third)
	}
	if thirdContinue != threads.ToolContinueManual {
		t.Fatalf("exhausted failure continue = %q, want manual", thirdContinue)
	}
}

func TestJSONValidationNegativeMaxRetriesIsUnlimited(t *testing.T) {
	schema := PayloadJSONSchema(gschema.Schema{
		Type:     "object",
		Required: []string{"value"},
		Properties: map[string]*gschema.Schema{
			"value": {Type: "string"},
		},
	})
	validation := NewJSONValidation(schema, -1)
	thread := threads.New()

	for attempt := 1; attempt <= 4; attempt++ {
		call := Call{CallID: fmt.Sprintf("c%d", attempt), Name: "echo", Payload: `{}`}
		var args struct {
			Value string `json:"value"`
		}
		result, continueMode := validation.ValidateInto(thread, call, &args, nil)
		if result == nil || result.SafeRollback == nil {
			t.Fatalf("attempt %d result = %#v, want rollbackable failure", attempt, result)
		}
		if continueMode != threads.ToolContinueAuto {
			t.Fatalf("attempt %d continue = %q, want auto", attempt, continueMode)
		}
		if result.SafeRollback.RetryAttempt != attempt || result.SafeRollback.MaxRetries != -1 {
			t.Fatalf("attempt %d rollback metadata = %#v", attempt, result.SafeRollback)
		}
		thread.QueueItem(threads.ToolCall(call))
		thread.QueueItem(*result)
	}
}

func TestJSONValidationRetryAttemptIgnoresParallelSiblingResult(t *testing.T) {
	schema := PayloadJSONSchema(gschema.Schema{
		Type:     "object",
		Required: []string{"value"},
		Properties: map[string]*gschema.Schema{
			"value": {Type: "string"},
		},
	})
	validation := NewJSONValidation(schema, 2)
	thread := threads.New()

	firstCall := Call{CallID: "c1", Name: "calc", Payload: `{}`}
	var args struct {
		Value string `json:"value"`
	}
	first, _ := validation.ValidateInto(thread, firstCall, &args, nil)
	thread.QueueItem(threads.ToolCall(firstCall))
	thread.QueueItem(threads.ToolCall{CallID: "c2", Name: "background", Payload: `{}`})
	thread.QueueItem(*first)
	thread.QueueItem(threads.ToolCallResult{CallID: "c2", Output: "background success"})

	secondCall := Call{CallID: "c3", Name: "calc", Payload: `{}`}
	second, _ := validation.ValidateInto(thread, secondCall, &args, nil)
	if second == nil || second.SafeRollback == nil {
		t.Fatalf("second failure = %#v, want rollbackable result", second)
	}
	if second.SafeRollback.RetryAttempt != 2 {
		t.Fatalf("retry attempt after sibling result = %d, want 2", second.SafeRollback.RetryAttempt)
	}
}

func TestJSONValidationSameNameParallelCallsShareRetryRound(t *testing.T) {
	schema := PayloadJSONSchema(gschema.Schema{
		Type:     "object",
		Required: []string{"value"},
		Properties: map[string]*gschema.Schema{
			"value": {Type: "string"},
		},
	})
	validation := NewJSONValidation(schema, 2)
	thread := threads.New()
	type args struct {
		Value string `json:"value"`
	}

	firstRound := []Call{
		{CallID: "c1", Name: "calc", Payload: `{}`},
		{CallID: "c2", Name: "calc", Payload: `{}`},
	}
	for _, call := range firstRound {
		thread.QueueItem(threads.ToolCall(call))
	}
	for _, call := range firstRound {
		var dst args
		result, _ := validation.ValidateInto(thread, call, &dst, nil)
		if result == nil || result.SafeRollback == nil || result.SafeRollback.RetryAttempt != 1 {
			t.Fatalf("first-round call %s result = %#v, want retry attempt 1", call.CallID, result)
		}
		thread.QueueItem(*result)
	}

	secondRound := []Call{
		{CallID: "c3", Name: "calc", Payload: `{}`},
		{CallID: "c4", Name: "calc", Payload: `{}`},
	}
	for _, call := range secondRound {
		thread.QueueItem(threads.ToolCall(call))
	}
	for _, call := range secondRound {
		var dst args
		result, _ := validation.ValidateInto(thread, call, &dst, nil)
		if result == nil || result.SafeRollback == nil || result.SafeRollback.RetryAttempt != 2 {
			t.Fatalf("second-round call %s result = %#v, want retry attempt 2", call.CallID, result)
		}
		thread.QueueItem(*result)
	}
}

func TestJSONValidationDecodesValidArguments(t *testing.T) {
	schema := PayloadJSONSchema(gschema.Schema{
		Type: "object",
		Properties: map[string]*gschema.Schema{
			"count": {Type: "integer"},
		},
		Required: []string{"count"},
	})
	validation := NewJSONValidation(schema, 2)
	var got struct {
		Count int `json:"count"`
	}

	failure, continueMode := validation.ValidateInto(
		threads.New(),
		Call{CallID: "c1", Name: "count", Payload: `{"count":7}`},
		&got,
		nil,
	)
	if failure != nil || continueMode != threads.ToolContinueAuto {
		t.Fatalf("valid failure = %#v, continue = %q", failure, continueMode)
	}
	if got.Count != 7 {
		t.Fatalf("decoded count = %d, want 7", got.Count)
	}
}

func TestJSONHandlerValidatesBeforeCallingHandler(t *testing.T) {
	called := false
	handler := JSONHandler(func(_ context.Context, _ threads.Thread, call Call, args struct {
		Count int `json:"count"`
	}) Item {
		called = true
		return ResultText(call, "ok")
	})

	var got Item
	handling, err := handler.HandleToolCall(
		context.Background(),
		threads.New(),
		Call{CallID: "c1", Name: "count", Payload: `{"count":"seven"}`},
		func(item Item) error { got = item; return nil },
	)
	if err != nil {
		t.Fatalf("HandleToolCall: %v", err)
	}
	if called {
		t.Fatal("handler ran for schema-invalid arguments")
	}
	if handling.Continue != threads.ToolContinueAuto {
		t.Fatalf("continue = %q, want auto", handling.Continue)
	}
	result := got.(threads.ToolCallResult)
	if result.SafeRollback == nil || !strings.Contains(result.Output, "tool schema") {
		t.Fatalf("result = %#v, want rollbackable schema error", result)
	}
}
