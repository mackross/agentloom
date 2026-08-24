package streamerlivetest

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mackross/agentloom/threads"
)

type LiveHarness interface {
	Stream(t testing.TB, req threads.Req, emit func(threads.Item) error) error
}

// Run runs every live suite supported by h. Optional suites are selected
// by the additional interfaces implemented by the harness.
func Run(t *testing.T, h LiveHarness) {
	t.Helper()
	if _, ok := h.(supportsToolCallChunking); ok {
		runToolCallChunking(t, h)
	}
	if _, ok := h.(supportsAssistantTextChunking); ok {
		runAssistantTextChunking(t, h)
	}
	if _, ok := h.(supportsParallelToolCalls); ok {
		runParallelToolCalls(t, h)
	}
	if _, ok := h.(supportsAllowedTools); ok {
		runAllowedTools(t, h)
	}
	if reasoning, ok := h.(ReasoningToolLoopHarness); ok {
		runReasoningToolLoopSuite(t, reasoning)
	}
}

func collectItems(t testing.TB, h LiveHarness, req threads.Req) []threads.Item {
	t.Helper()
	items := []threads.Item{}
	err := h.Stream(t, req, func(item threads.Item) error {
		items = append(items, item)
		return nil
	})
	if err != nil {
		t.Fatalf("stream req: %v", err)
	}
	return items
}

func summarizeItems(items []threads.Item) string {
	if len(items) == 0 {
		return "[]"
	}
	parts := make([]string, 0, len(items))
	for _, item := range items {
		switch v := item.(type) {
		case threads.AssistantText:
			parts = append(parts, fmt.Sprintf("assistant_text(%d)", len(v)))
		case threads.ToolCallChunk:
			parts = append(parts, fmt.Sprintf("tool_chunk(%s,%d)", v.Name, len(v.PayloadDelta)))
		case threads.ToolCall:
			parts = append(parts, fmt.Sprintf("tool_call(%s)", v.Name))
		default:
			parts = append(parts, fmt.Sprintf("%T", item))
		}
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func summarizeToolCalls(finals []threads.ToolCall) string {
	if len(finals) == 0 {
		return "[]"
	}
	parts := make([]string, 0, len(finals))
	for _, call := range finals {
		parts = append(parts, fmt.Sprintf("%s:%s", call.CallID, call.Name))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
