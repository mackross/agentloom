package streamerlivetest

import (
	"fmt"
	"reflect"
	"slices"
	"testing"

	gschema "github.com/google/jsonschema-go/jsonschema"

	"github.com/mackross/agentloom/threads"
)

// SupportsToolCallChunking opts a live harness into tool-call chunking tests.
type SupportsToolCallChunking struct{}

func (SupportsToolCallChunking) _SupportsToolCallChunking() {}

// SupportsParallelToolCalls opts a live harness into parallel tool-call tests.
type SupportsParallelToolCalls struct{}

func (SupportsParallelToolCalls) _SupportsParallelToolCalls() {}

// SupportsAllowedTools opts a live harness into allowed-tools tests.
type SupportsAllowedTools struct{}

func (SupportsAllowedTools) _SupportsAllowedTools() {}

type supportsToolCallChunking interface{ _SupportsToolCallChunking() }
type supportsParallelToolCalls interface{ _SupportsParallelToolCalls() }
type supportsAllowedTools interface{ _SupportsAllowedTools() }

func tokenTool(name, token string) threads.ToolSpec {
	return threads.ToolSpec{
		Name:        name,
		Description: "Records the " + token + " token.",
		Payload: threads.ToolPayloadJSONSchema(gschema.Schema{
			Type: "object",
			Properties: map[string]*gschema.Schema{
				"token": {Type: "string", Pattern: "^" + token + "$"},
			},
			Required: []string{"token"},
		}),
	}
}

func parallelTokenTools() []threads.ToolSpec {
	return []threads.ToolSpec{
		tokenTool("alpha_once", "alpha"),
		tokenTool("beta_once", "beta"),
	}
}

func runToolCallChunking(t *testing.T, h LiveHarness) {
	t.Helper()
	t.Run("tool_call_chunks_for_long_payload", func(t *testing.T) {
		minLens := []int{1200, 2400, 4000}
		for i, minLen := range minLens {
			req := threads.Req{
				Instruction: "Call the tool exactly once. Do not output any text.",
				Items: []threads.Item{threads.UserText(fmt.Sprintf(
					"Call tool long_once with a lowercase payload string of at least %d characters.",
					minLen,
				))},
				Tools: threads.ToolOfferSnapshot{Offered: []threads.ToolSpec{{
					Name:        "long_once",
					Description: "Tool for long payload args",
					Payload: threads.ToolPayloadJSONSchema(gschema.Schema{Type: "object", Properties: map[string]*gschema.Schema{
						"payload": {Type: "string", Pattern: "^[a-z]+$", MinLength: &minLen},
					}, Required: []string{"payload"}}),
				}}},
			}

			items := collectItems(t, h, req)
			counts, finals := toolCallEvents(items)
			if hasFinalWithMinChunkCount(counts, finals, 2) {
				return
			}
			if i == len(minLens)-1 {
				t.Skipf("no multi-chunk tool call observed up to minLength=%d (counts=%v finals=%s)", minLen, counts, summarizeToolCalls(finals))
			}
		}
	})
}

func runParallelToolCalls(t *testing.T, h LiveHarness) {
	t.Helper()
	t.Run("parallel_tool_calls", func(t *testing.T) {
		parallel := true
		req := threads.Req{
			Instruction: "Call both tools exactly once in the same response. Do not output any text. Do not wait for tool results.",
			Items:       []threads.Item{threads.UserText("Call tool alpha_once with token alpha and tool beta_once with token beta.")},
			Tools: threads.ToolOfferSnapshot{
				Offered:  parallelTokenTools(),
				Parallel: &parallel,
			},
		}

		for attempt := 1; attempt <= 3; attempt++ {
			items := collectItems(t, h, req)
			finals := finalToolCalls(items)
			if hasToolCalls(finals, "alpha_once", "beta_once") {
				return
			}
			if attempt == 3 {
				t.Skipf("did not observe both tool calls after %d attempts (finals=%s items=%s)", attempt, summarizeToolCalls(finals), summarizeItems(items))
			}
		}
	})
}

func runAllowedTools(t *testing.T, h LiveHarness) {
	t.Helper()
	t.Run("allowed_tools", func(t *testing.T) {
		req := threads.Req{
			Instruction: "Call tools alpha_once and beta_once exactly once each in the same response. Do not call any other tools. Do not output any text.",
			Items:       []threads.Item{threads.UserText("Call tool alpha_once with token alpha and tool beta_once with token beta. Do not call gamma_once.")},
			Tools: threads.ToolOfferSnapshot{
				Offered: append(parallelTokenTools(), threads.ToolSpec{
					Name:        "gamma_once",
					Description: "Records the gamma token. Do not call this tool.",
					Payload: threads.ToolPayloadJSONSchema(gschema.Schema{Type: "object", Properties: map[string]*gschema.Schema{
						"token": {Type: "string", Pattern: "^gamma$"},
					}, Required: []string{"token"}}),
				}),
				Allowed: []string{"alpha_once", "beta_once"},
			},
		}

		for attempt := 1; attempt <= 3; attempt++ {
			items := collectItems(t, h, req)
			finals := finalToolCalls(items)
			if hasToolCalls(finals, "alpha_once", "beta_once") && !hasToolCalls(finals, "gamma_once") {
				return
			}
			if attempt == 3 {
				t.Skipf("did not observe allowed tools only after %d attempts (finals=%s items=%s)", attempt, summarizeToolCalls(finals), summarizeItems(items))
			}
		}
	})
}

func toolCallEvents(items []threads.Item) (map[string]int, []threads.ToolCall) {
	chunkCounts := map[string]int{}
	finals := []threads.ToolCall{}
	for _, item := range items {
		switch v := item.(type) {
		case threads.ToolCallChunk:
			chunkCounts[v.CallID]++
		case threads.ToolCall:
			finals = append(finals, v)
		}
	}
	return chunkCounts, finals
}

func finalToolCalls(items []threads.Item) []threads.ToolCall {
	_, finals := toolCallEvents(items)
	return finals
}

func hasFinalWithMinChunkCount(chunkCounts map[string]int, finals []threads.ToolCall, min int) bool {
	for _, call := range finals {
		if chunkCounts[call.CallID] >= min {
			return true
		}
	}
	return false
}

func hasToolCalls(finals []threads.ToolCall, wantNames ...string) bool {
	names := make([]string, 0, len(finals))
	for _, call := range finals {
		names = append(names, call.Name)
	}
	for _, want := range wantNames {
		if !slices.Contains(names, want) {
			return false
		}
	}
	return true
}

// SupportsReasoningToolLoop may be embedded in a live harness to opt it into
// the reasoning tool-loop suite.
type SupportsReasoningToolLoop struct {
	Streamer threads.LLMStreamer
}

func (s SupportsReasoningToolLoop) ReasoningToolLoopStreamer() threads.LLMStreamer {
	return s.Streamer
}

// ReasoningToolLoopHarness opts a live harness into reasoning continuation
// tests. Run discovers this interface automatically.
type ReasoningToolLoopHarness interface {
	ReasoningToolLoopStreamer() threads.LLMStreamer
}

func runReasoningToolLoopSuite(t *testing.T, h ReasoningToolLoopHarness) {
	t.Helper()
	streamer := h.ReasoningToolLoopStreamer()
	if streamer == nil {
		t.Fatal("reasoning tool-loop streamer is nil")
	}
	t.Run("reasoning_tool_loop", func(t *testing.T) {
		runReasoningToolLoop(t, streamer)
	})
}

func runReasoningToolLoop(t *testing.T, streamer threads.LLMStreamer) {
	t.Helper()
	tools := threads.ToolOfferSnapshot{Offered: []threads.ToolSpec{{
		Name: "lookup_token", Description: "Returns the stable token for a key.",
		Payload: threads.ToolPayloadFor[struct {
			Key string `json:"key" jsonschema:"key to look up; use alpha"`
		}](),
	}}}
	build := func(items []threads.Item) threads.Req {
		req := threads.DefaultRequestBuilder.Build(items, streamer.Capabilities())
		req.Tools = tools
		return req
	}

	history := []threads.Item{threads.UserText("Use lookup_token with key alpha, then answer only with the returned token.")}
	var first []threads.Item
	var reasoning threads.ReasoningItem
	var call threads.ToolCall
	for attempt := 1; attempt <= 3; attempt++ {
		reasoning, call = threads.ReasoningItem{}, threads.ToolCall{}
		first = streamItems(t, streamer, build(history))
		for _, item := range first {
			if candidate, ok := item.(threads.ReasoningItem); ok {
				reasoning = candidate
			}
			if candidate, ok := item.(threads.ToolCall); ok {
				call = candidate
				break
			}
		}
		if reasoning.Provider != "" && call.CallID != "" {
			break
		}
		if attempt == 3 {
			t.Fatalf("automatic tool call with provider reasoning not observed after %d attempts", attempt)
		}
	}
	if reasoning.Provider != streamer.Capabilities().Reasoning[0] {
		t.Fatalf("reasoning provider = %q, want configured provider", reasoning.Provider)
	}

	history = append(history, first...)
	history = append(history, threads.ToolCallResult{CallID: call.CallID, Output: "token-alpha"})
	secondReq := build(history)
	reasoningAt, callAt := -1, -1
	for i, item := range secondReq.Items {
		if reflect.DeepEqual(item, reasoning) {
			reasoningAt = i
		}
		if candidate, ok := item.(threads.ToolCall); ok && candidate.CallID == call.CallID {
			callAt = i
		}
	}
	if reasoningAt < 0 || callAt < 0 || reasoningAt >= callAt {
		t.Fatal("tool-result request did not retain the exact reasoning item before its tool call")
	}
	second := streamItems(t, streamer, secondReq)
	hasText := false
	for _, item := range second {
		if text, ok := item.(threads.AssistantText); ok && text != "" {
			hasText = true
		}
	}
	if !hasText {
		t.Fatal("tool-result response contained no final assistant text")
	}

	history = append(history, second...)
	history = append(history, threads.UserText("Reply only with acknowledged."))
	thirdReq := build(history)
	thirdReq.Tools = threads.ToolOfferSnapshot{}
	for _, item := range thirdReq.Items {
		if _, ok := item.(threads.ReasoningItem); ok {
			t.Fatal("new user request retained reasoning from an earlier turn")
		}
	}
	_ = streamItems(t, streamer, thirdReq)
}

func streamItems(t testing.TB, streamer threads.LLMStreamer, req threads.Req) []threads.Item {
	t.Helper()
	var items []threads.Item
	if err := streamer.StreamReq(req, func(item threads.Item) error { items = append(items, item); return nil }); err != nil {
		t.Fatalf("live stream request: %v", err)
	}
	return items
}
