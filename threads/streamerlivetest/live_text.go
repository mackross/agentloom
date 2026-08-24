package streamerlivetest

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mackross/agentloom/threads"
)

// SupportsAssistantTextChunking opts a live harness into assistant text chunking tests.
type SupportsAssistantTextChunking struct{}

func (SupportsAssistantTextChunking) _SupportsAssistantTextChunking() {}

type supportsAssistantTextChunking interface{ _SupportsAssistantTextChunking() }

func runAssistantTextChunking(t *testing.T, h LiveHarness) {
	t.Helper()
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

			items := collectItems(t, h, req)
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
