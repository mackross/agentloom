package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mackross/agentloom/programs"
	"github.com/mackross/agentloom/programs/programtest"
	"github.com/mackross/agentloom/threads"
	threadtool "github.com/mackross/agentloom/threads/tool"
)

type lookupArgs struct {
	Query string `json:"query"`
}

type DeliveryQuestion struct {
	OrderID         string `json:"order_id" jsonschema:"customer order identifier"`
	CustomerMessage string `json:"customer_message" jsonschema:"customer's delivery question"`
}

type SupportResolution struct {
	OrderStatus   string `json:"order_status" jsonschema:"current order status"`
	CustomerReply string `json:"customer_reply" jsonschema:"reply to send to the customer"`
	Escalate      bool   `json:"escalate" jsonschema:"whether a human support agent should take over"`
}

type OrderLookupArgs struct {
	OrderID string `json:"order_id" jsonschema:"order identifier to look up"`
}

type OrderLookupResult struct {
	Status            string `json:"status"`
	LastScan          string `json:"last_scan"`
	EstimatedDelivery string `json:"estimated_delivery"`
}

func TestSingleAgentTurnRunsToolsAndRequiresStructuredCompletion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	branch := newSignatureTestBranch(t, ctx)
	oldTools := threadtool.NewCatalog()
	branch.SetToolProvider(oldTools)
	branch.SetToolResolver(oldTools)

	spec, handler := threadtool.JSON[lookupArgs](
		"lookup",
		"Look up relevant information.",
		func(_ context.Context, _ threads.Thread, call threadtool.Call, args lookupArgs) threadtool.Item {
			if args.Query != "change" {
				t.Errorf("lookup query = %q, want change", args.Query)
			}
			return threadtool.ResultText(call, "A programs package was added.")
		},
	)
	tools := threadtool.NewCatalog().Add(spec, handler)

	streamer := &singleAgentTurnStreamer{t: t}
	branch.SetExecutor(threads.NewThreadExecutor(streamer))

	agent := SingleAgentTurn[signatureInput, signatureOutput]{
		Signature: programs.Signature[signatureInput, signatureOutput]{
			Name:        "research_answer",
			Instruction: "Research before answering.",
		},
		Tools: tools,
	}
	out, err := agent.Run(ctx, branch, signatureInput{
		Question: "What changed?",
		Context:  "Use the lookup tool.",
	})
	if err != nil {
		t.Fatalf("SingleAgentTurn.Run: %v", err)
	}
	if out.Answer != "A programs package was added." {
		t.Fatalf("Answer = %q", out.Answer)
	}
	if streamer.calls != 3 {
		t.Fatalf("stream calls = %d, want 3", streamer.calls)
	}
	if branch.ToolProvider() != oldTools {
		t.Fatal("ToolProvider was not restored")
	}
	if branch.ToolResolver() != oldTools {
		t.Fatal("ToolResolver was not restored")
	}
}

type singleAgentTurnStreamer struct {
	t        *testing.T
	calls    int
	requests []threads.Req
	emitted  []threads.Item
}

func (*singleAgentTurnStreamer) Capabilities() threads.StreamerCapabilities {
	return threads.StreamerCapabilities{}
}

func (*singleAgentTurnStreamer) RegisterToolNormalizer(string, threads.ToolNormalizer) {}

func (*singleAgentTurnStreamer) UnregisterToolNormalizer(string) {}

func (*singleAgentTurnStreamer) SyntheticToolCallID() string { return "" }

func (s *singleAgentTurnStreamer) StreamReq(req threads.Req, emit func(threads.Item) error) error {
	s.requests = append(s.requests, req)
	s.calls++
	switch s.calls {
	case 1:
		if req.Instruction != "Research before answering." {
			s.t.Fatalf("Instruction = %q", req.Instruction)
		}
		if req.Tools.Required {
			s.t.Fatal("initial tools unexpectedly required")
		}
		if got := offeredToolNames(req); strings.Join(got, ",") != "lookup,submit_output" {
			s.t.Fatalf("Offered = %v, want lookup and submit_output", got)
		}
		if req.Tools.Parallel == nil || *req.Tools.Parallel {
			s.t.Fatalf("Parallel = %#v, want false", req.Tools.Parallel)
		}
		if len(req.Items) != 1 {
			s.t.Fatalf("first request items = %d, want 1", len(req.Items))
		}
		prompt, ok := req.Items[0].(threads.UserText)
		if !ok {
			s.t.Fatalf("first request item = %T, want UserText", req.Items[0])
		}
		for _, want := range []string{
			`"question": "What changed?"`,
			`call "submit_output"`,
		} {
			if !strings.Contains(string(prompt), want) {
				s.t.Errorf("prompt missing %q:\n%s", want, prompt)
			}
		}
		return s.emit(emit, threads.ToolCall{
			CallID:  "lookup-1",
			Name:    "lookup",
			Payload: `{"query":"change"}`,
		})
	case 2:
		var sawCall, sawResult bool
		for _, item := range req.Items {
			switch item.(type) {
			case threads.ToolCall:
				sawCall = true
			case threads.ToolCallResult:
				sawResult = true
			}
		}
		if !sawCall || !sawResult {
			s.t.Fatalf("continuation missing tool exchange: %#v", req.Items)
		}
		// Finishing with text instead of the completion tool makes the adapter
		// steer the model into a required completion-only request.
		return s.emit(emit, threads.AssistantText("The answer is ready."))
	case 3:
		if !req.Tools.Required {
			s.t.Fatal("completion tool is not required after missing completion")
		}
		if got := offeredToolNames(req); len(got) != 1 || got[0] != "submit_output" {
			s.t.Fatalf("Offered = %v, want only submit_output", got)
		}
		var sawHint bool
		for _, item := range req.Items {
			if text, ok := item.(threads.UserText); ok && strings.Contains(string(text), "finished without calling") {
				sawHint = true
			}
		}
		if !sawHint {
			s.t.Fatalf("completion request has no steering hint: %#v", req.Items)
		}
		return s.emit(emit, threads.ToolCall{
			CallID:  "complete-1",
			Name:    "submit_output",
			Payload: `{"answer":"A programs package was added."}`,
		})
	default:
		s.t.Fatalf("unexpected stream call %d", s.calls)
		return nil
	}
}

func (s *singleAgentTurnStreamer) emit(emit func(threads.Item) error, item threads.Item) error {
	s.emitted = append(s.emitted, item)
	return emit(item)
}

func offeredToolNames(req threads.Req) []string {
	names := make([]string, 0, len(req.Tools.Offered))
	for _, spec := range req.Tools.Offered {
		names = append(names, spec.Name)
	}
	return names
}

func TestSingleAgentTurnConversationGolden(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	branch := newSignatureTestBranch(t, ctx)
	streamer := &goldenConversationStreamer{
		items: []threads.Item{
			threads.ToolCall{
				CallID:  "lookup-order-1",
				Name:    "lookup_order",
				Payload: `{"order_id":"ORD-2048"}`,
			},
			threads.AssistantText("I found the shipment and prepared a customer response."),
			threads.ToolCall{
				CallID: "complete-order-1",
				Name:   "submit_output",
				Payload: `{
					"order_status": "delayed",
					"customer_reply": "Order ORD-2048 is delayed at the Sydney distribution centre and is now expected by 3 April 2026.",
					"escalate": false
				}`,
			},
		},
	}
	branch.SetExecutor(threads.NewThreadExecutor(streamer))

	programtest.Golden(t, filepath.Join("testdata", "single_agent_turn_conversation.golden"), func() (SupportResolution, error) {
		lookupSpec, lookupHandler := threadtool.JSON[OrderLookupArgs](
			"lookup_order",
			"Look up the current fulfillment and delivery state for an order.",
			func(_ context.Context, _ threads.Thread, call threadtool.Call, args OrderLookupArgs) threadtool.Item {
				if args.OrderID != "ORD-2048" {
					return threadtool.ResultError(call, fmt.Errorf("order %q not found", args.OrderID))
				}
				return threadtool.ResultJSON(call, OrderLookupResult{
					Status:            "delayed",
					LastScan:          "Sydney distribution centre",
					EstimatedDelivery: "2026-04-03",
				})
			},
		)
		tools := threadtool.NewCatalog().Add(lookupSpec, lookupHandler)

		agent := SingleAgentTurn[DeliveryQuestion, SupportResolution]{
			Signature: programs.Signature[DeliveryQuestion, SupportResolution]{
				Name:        "resolve_delivery_question",
				Instruction: "Resolve the customer's delivery question using order data. Do not invent shipment details.",
			},
			Tools: tools,
		}

		out, err := agent.Run(ctx, branch, DeliveryQuestion{
			OrderID:         "ORD-2048",
			CustomerMessage: "My order was due yesterday. Where is it?",
		})
		return out, err
	}, func(out SupportResolution) string {
		return renderSingleAgentTurnConversation(t, streamer.requests, streamer.emitted, out)
	})
}

func TestSingleAgentTurnNilOutputConversationGolden(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	branch := newSignatureTestBranch(t, ctx)
	streamer := &goldenConversationStreamer{
		items: []threads.Item{
			threads.ToolCall{
				CallID:  "lookup-order-1",
				Name:    "lookup_order",
				Payload: `{"order_id":"ORD-2048"}`,
			},
			threads.AssistantText("Order ORD-2048 is delayed and is now expected by 3 April 2026."),
		},
	}
	branch.SetExecutor(threads.NewThreadExecutor(streamer))

	programtest.Golden(t, filepath.Join("testdata", "single_agent_turn_nil_output_conversation.golden"), func() (any, error) {
		lookupSpec, lookupHandler := threadtool.JSON[OrderLookupArgs](
			"lookup_order",
			"Look up the current fulfillment and delivery state for an order.",
			func(_ context.Context, _ threads.Thread, call threadtool.Call, args OrderLookupArgs) threadtool.Item {
				if args.OrderID != "ORD-2048" {
					return threadtool.ResultError(call, fmt.Errorf("order %q not found", args.OrderID))
				}
				return threadtool.ResultJSON(call, OrderLookupResult{
					Status:            "delayed",
					LastScan:          "Sydney distribution centre",
					EstimatedDelivery: "2026-04-03",
				})
			},
		)
		tools := threadtool.NewCatalog().Add(lookupSpec, lookupHandler)

		agent := SingleAgentTurn[DeliveryQuestion, any]{
			Signature: programs.Signature[DeliveryQuestion, any]{
				Name:        "answer_delivery_question",
				Instruction: "Answer the customer's delivery question using order data. Do not invent shipment details.",
			},
			Tools: tools,
		}

		out, err := agent.Run(ctx, branch, DeliveryQuestion{
			OrderID:         "ORD-2048",
			CustomerMessage: "My order was due yesterday. Where is it?",
		})
		return out, err
	}, func(out any) string {
		return renderSingleAgentTurnNilOutputConversation(t, streamer.requests, streamer.emitted, out)
	})
}

type goldenConversationStreamer struct {
	requests []threads.Req
	emitted  []threads.Item
	items    []threads.Item
}

func (*goldenConversationStreamer) Capabilities() threads.StreamerCapabilities {
	return threads.StreamerCapabilities{}
}

func (*goldenConversationStreamer) RegisterToolNormalizer(string, threads.ToolNormalizer) {}

func (*goldenConversationStreamer) UnregisterToolNormalizer(string) {}

func (*goldenConversationStreamer) SyntheticToolCallID() string { return "" }

func (s *goldenConversationStreamer) StreamReq(req threads.Req, emit func(threads.Item) error) error {
	s.requests = append(s.requests, req)
	if len(s.items) == 0 {
		return fmt.Errorf("golden conversation has no scripted item for request %d", len(s.requests))
	}
	item := s.items[0]
	s.items = s.items[1:]
	s.emitted = append(s.emitted, item)
	return emit(item)
}

func renderSingleAgentTurnConversation(t *testing.T, requests []threads.Req, emitted []threads.Item, out any) string {
	t.Helper()
	if len(requests) != 3 || len(emitted) != 3 {
		t.Fatalf("conversation requests/emissions = %d/%d, want 3/3", len(requests), len(emitted))
	}

	initial := requests[0]
	lookupCall, ok := emitted[0].(threads.ToolCall)
	if !ok {
		t.Fatalf("first emission = %T, want ToolCall", emitted[0])
	}
	assistantText, ok := emitted[1].(threads.AssistantText)
	if !ok {
		t.Fatalf("second emission = %T, want AssistantText", emitted[1])
	}
	completionCall, ok := emitted[2].(threads.ToolCall)
	if !ok {
		t.Fatalf("third emission = %T, want ToolCall", emitted[2])
	}

	prompt := firstUserText(t, initial)
	lookupResult := toolResultText(t, requests[1], lookupCall.CallID)
	completionReq := requests[2]
	hint := lastUserText(t, completionReq)
	outputJSON, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatalf("marshal program output: %v", err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "ASSISTANT INSTRUCTION\n%s\n\n", initial.Instruction)
	renderGoldenTools(t, &b, initial.Tools)
	fmt.Fprintf(&b, "\nUSER\n%s\n\n", prompt)
	fmt.Fprintf(&b, "ASSISTANT TOOL CALL: %s\n%s\n\n", lookupCall.Name, prettyJSON(t, lookupCall.Payload))
	fmt.Fprintf(&b, "TOOL RESULT: %s\n%s\n\n", lookupCall.Name, prettyJSON(t, lookupResult))
	fmt.Fprintf(&b, "ASSISTANT\n%s\n\n", assistantText)
	renderGoldenTools(t, &b, completionReq.Tools)
	fmt.Fprintf(&b, "\nUSER\n%s\n\n", hint)
	fmt.Fprintf(&b, "ASSISTANT TOOL CALL: %s\n%s\n\n", completionCall.Name, prettyJSON(t, completionCall.Payload))
	fmt.Fprintf(&b, "PROGRAM OUTPUT\n%s\n", outputJSON)
	return b.String()
}

func renderSingleAgentTurnNilOutputConversation(t *testing.T, requests []threads.Req, emitted []threads.Item, out any) string {
	t.Helper()
	if len(requests) != 2 || len(emitted) != 2 {
		t.Fatalf("conversation requests/emissions = %d/%d, want 2/2", len(requests), len(emitted))
	}

	initial := requests[0]
	lookupCall, ok := emitted[0].(threads.ToolCall)
	if !ok {
		t.Fatalf("first emission = %T, want ToolCall", emitted[0])
	}
	assistantText, ok := emitted[1].(threads.AssistantText)
	if !ok {
		t.Fatalf("second emission = %T, want AssistantText", emitted[1])
	}

	prompt := firstUserText(t, initial)
	lookupResult := toolResultText(t, requests[1], lookupCall.CallID)
	outputJSON, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatalf("marshal program output: %v", err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "ASSISTANT INSTRUCTION\n%s\n\n", initial.Instruction)
	renderGoldenTools(t, &b, initial.Tools)
	fmt.Fprintf(&b, "\nUSER\n%s\n\n", prompt)
	fmt.Fprintf(&b, "ASSISTANT TOOL CALL: %s\n%s\n\n", lookupCall.Name, prettyJSON(t, lookupCall.Payload))
	fmt.Fprintf(&b, "TOOL RESULT: %s\n%s\n\n", lookupCall.Name, prettyJSON(t, lookupResult))
	fmt.Fprintf(&b, "ASSISTANT\n%s\n\n", assistantText)
	fmt.Fprintf(&b, "PROGRAM OUTPUT\n%s\n", outputJSON)
	return b.String()
}

func renderGoldenTools(t *testing.T, b *strings.Builder, tools threads.ToolOfferSnapshot) {
	t.Helper()
	mode := "optional"
	if tools.Required {
		mode = "required"
	}
	parallel := "provider default"
	if tools.Parallel != nil {
		parallel = fmt.Sprintf("%t", *tools.Parallel)
	}
	fmt.Fprintf(b, "AVAILABLE TOOLS (%s, parallel=%s)\n", mode, parallel)
	for _, spec := range tools.Offered {
		fmt.Fprintf(b, "- %s: %s\n", spec.Name, spec.Description)
		schema, err := json.MarshalIndent(spec.Payload, "  ", "  ")
		if err != nil {
			t.Fatalf("marshal payload for %s: %v", spec.Name, err)
		}
		fmt.Fprintf(b, "  Payload:\n  %s\n", schema)
	}
}

func firstUserText(t *testing.T, req threads.Req) string {
	t.Helper()
	for _, item := range req.Items {
		if text, ok := item.(threads.UserText); ok {
			return string(text)
		}
	}
	t.Fatal("request has no user text")
	return ""
}

func lastUserText(t *testing.T, req threads.Req) string {
	t.Helper()
	for i := len(req.Items) - 1; i >= 0; i-- {
		if text, ok := req.Items[i].(threads.UserText); ok {
			return string(text)
		}
	}
	t.Fatal("request has no user text")
	return ""
}

func toolResultText(t *testing.T, req threads.Req, callID string) string {
	t.Helper()
	for _, item := range req.Items {
		if result, ok := item.(threads.ToolCallResult); ok && result.CallID == callID {
			return result.Output
		}
	}
	t.Fatalf("request has no result for tool call %q", callID)
	return ""
}

func prettyJSON(t *testing.T, raw string) string {
	t.Helper()
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		t.Fatalf("parse JSON %q: %v", raw, err)
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("marshal JSON %q: %v", raw, err)
	}
	return string(data)
}

func TestSingleAgentTurnAnyOutputReturnsWhenAgentFinishes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	branch := newSignatureTestBranch(t, ctx)
	streamer := &signatureTestStreamer{
		t:     t,
		reply: "The task is complete.",
		assertRequest: func(t *testing.T, req threads.Req) {
			t.Helper()
			if len(req.Tools.Offered) != 0 {
				t.Fatalf("no-output agent offered tools: %#v", req.Tools.Offered)
			}
			if len(req.Items) != 1 || strings.Contains(string(req.Items[0].(threads.UserText)), "submit_output") {
				t.Fatalf("no-output prompt requests structured completion: %#v", req.Items)
			}
		},
	}
	branch.SetExecutor(threads.NewThreadExecutor(streamer))

	agent := SingleAgentTurn[signatureInput, any]{
		Signature: programs.Signature[signatureInput, any]{Name: "do_task"},
	}
	out, err := agent.Run(ctx, branch, signatureInput{Question: "Do it."})
	if err != nil {
		t.Fatalf("SingleAgentTurn.Run: %v", err)
	}
	if out != nil {
		t.Fatalf("output = %#v, want nil", out)
	}
	if streamer.calls != 1 {
		t.Fatalf("stream calls = %d, want 1", streamer.calls)
	}
}

func TestSingleAgentTurnCanDisableMissingCompletionRetries(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	branch := newSignatureTestBranch(t, ctx)
	streamer := &signatureTestStreamer{t: t, reply: "done without completion"}
	branch.SetExecutor(threads.NewThreadExecutor(streamer))

	agent := SingleAgentTurn[signatureInput, signatureOutput]{
		Signature:  programs.Signature[signatureInput, signatureOutput]{Name: "no_completion"},
		MaxRetries: -1,
	}
	_, err := agent.Run(ctx, branch, signatureInput{})
	if !errors.Is(err, ErrSingleAgentTurnNoOutput) {
		t.Fatalf("error = %v, want %v", err, ErrSingleAgentTurnNoOutput)
	}
	if streamer.calls != 1 {
		t.Fatalf("stream calls = %d, want 1", streamer.calls)
	}
}
