package streamertest

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	gschema "github.com/google/jsonschema-go/jsonschema"

	"github.com/mackross/agentloom/threads"
)

type LiveHarness interface {
	Capabilities() Capabilities
	Stream(t testing.TB, req threads.Req, emit func(threads.Item) error) error
}

func RunLiveCapabilityTests(t *testing.T, h LiveHarness) {
	t.Helper()

	if h.Capabilities().ToolCallChunks {
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

				items := collectLiveItems(t, h, req)
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

	if h.Capabilities().AssistantTextChunks {
		t.Run("assistant_text_chunks_for_long_output", func(t *testing.T) {
			minLens := []int{1200, 2400, 4000}
			for i, minLen := range minLens {
				req := threads.Req{
					Instruction: "Reply with only plain text. Do not call tools. Do not use markdown.",
					Items: []threads.Item{threads.UserText(fmt.Sprintf(
						"Write a single lowercase paragraph of at least %d characters about coastlines and weather. Return only the paragraph.",
						minLen,
					))},
				}

				items := collectLiveItems(t, h, req)
				chunks, text := assistantTextStats(items)
				if chunks >= 2 {
					return
				}
				if i == len(minLens)-1 {
					t.Skipf("no multi-chunk assistant text observed up to minLength=%d (chunks=%d text_len=%d items=%s)", minLen, chunks, len(text), summarizeItems(items))
				}
			}
		})
	}

	if h.Capabilities().ParallelToolCalls {
		t.Run("parallel_tool_calls", func(t *testing.T) {
			parallel := true
			req := threads.Req{
				Instruction: "Call both tools exactly once in the same response. Do not output any text. Do not wait for tool results.",
				Items: []threads.Item{threads.UserText(
					"Call tool alpha_once with token alpha and tool beta_once with token beta.",
				)},
				Tools: threads.ToolOfferSnapshot{
					Offered: []threads.ToolSpec{
						{
							Name:        "alpha_once",
							Description: "Records the alpha token.",
							Payload: threads.ToolPayloadJSONSchema(gschema.Schema{Type: "object", Properties: map[string]*gschema.Schema{
								"token": {Type: "string", Pattern: "^alpha$"},
							}, Required: []string{"token"}}),
						},
						{
							Name:        "beta_once",
							Description: "Records the beta token.",
							Payload: threads.ToolPayloadJSONSchema(gschema.Schema{Type: "object", Properties: map[string]*gschema.Schema{
								"token": {Type: "string", Pattern: "^beta$"},
							}, Required: []string{"token"}}),
						},
					},
					Allowed:  []string{"alpha_once", "beta_once"},
					Parallel: &parallel,
				},
			}

			for attempt := 1; attempt <= 3; attempt++ {
				items := collectLiveItems(t, h, req)
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
}

func collectLiveItems(t testing.TB, h LiveHarness, req threads.Req) []threads.Item {
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

func assistantTextStats(items []threads.Item) (int, string) {
	chunks := 0
	var text strings.Builder
	for _, item := range items {
		if v, ok := item.(threads.AssistantText); ok {
			chunks++
			text.WriteString(string(v))
		}
	}
	return chunks, text.String()
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
