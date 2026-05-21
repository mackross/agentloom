//go:build live

package cerebras

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	gschema "github.com/google/jsonschema-go/jsonschema"
	openaiapi "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/shared"

	cachecerebras "github.com/mackross/agentloom/llms/cache/cerebras"
	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/streamertest"
)

func TestLiveChatCompletionsStreamerCapabilities(t *testing.T) {
	requireLiveCerebras(t)

	streamer := NewChatCompletionsStreamer(DefaultModel)
	streamertest.RunLiveCapabilityTests(t, cerebrasLiveHarness{streamer: streamer})
}

func TestLiveChatCompletionsCerebrasUniqueOptions(t *testing.T) {
	requireLiveCerebras(t)

	t.Run("reasoning_hidden_raw_and_effort", func(t *testing.T) {
		cases := []struct {
			name   string
			format string
			effort string
			allow400 bool
		}{
			{name: "hidden_low", format: "hidden", effort: "low"},
			{name: "raw_low", format: "raw", effort: "low"},
			{name: "format_none", format: "none", effort: "low", allow400: true},
			{name: "parsed_low", format: "parsed", effort: "low", allow400: true},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				streamer := NewChatCompletionsStreamer(DefaultModel)
				streamer.ReasoningFormat = tc.format
				streamer.ReasoningEffort = tc.effort
				items, err := liveTextRequestErr(streamer, "Reply with only: ok")
				if tc.allow400 && err != nil && strings.Contains(err.Error(), "400 Bad Request") {
					t.Skipf("Cerebras rejected reasoning_format=%q for this model: %v", tc.format, err)
				}
				if err != nil {
					t.Fatalf("live Cerebras request failed: %v", err)
				}
				if text := assistantText(items); strings.TrimSpace(text) == "" {
					t.Fatalf("expected visible assistant text, got items %#v", items)
				}
			})
		}
	})

	t.Run("predicted_output", func(t *testing.T) {
		var calls int
		streamer := NewChatCompletionsStreamer(DefaultModel)
		streamer.ReasoningFormat = "hidden"
		streamer.Prediction = func(req threads.Req) (string, bool, error) {
			calls++
			return "ok", true, nil
		}
		liveTextRequest(t, streamer, "Reply with only: ok")
		if calls != 1 {
			t.Fatalf("prediction callback calls = %d, want 1", calls)
		}
	})

	t.Run("clear_thinking_glm", func(t *testing.T) {
		clearThinking := false
		streamer := NewChatCompletionsStreamer(GLM47Model)
		streamer.ReasoningFormat = "hidden"
		streamer.ReasoningEffort = "none"
		streamer.ClearThinking = &clearThinking
		liveTextRequest(t, streamer, "Reply with only: ok")
	})

	t.Run("service_tier_queue_threshold_and_prompt_cache_key", func(t *testing.T) {
		streamer := NewChatCompletionsStreamer(DefaultModel)
		streamer.ReasoningFormat = "hidden"
		streamer.ServiceTier = "default"
		streamer.QueueThreshold = "1000"
		req := threads.Req{
			Items: []threads.Item{
				threads.UserText("Reply with only: ok"),
			},
			ItemMeta: []map[string]any{
				cachecerebras.PromptCacheKey("agentloom-live-" + time.Now().UTC().Format("20060102T150405")),
			},
		}
		if err := streamer.StreamReq(req, func(threads.Item) error { return nil }); err != nil {
			t.Fatalf("live service tier/queue threshold/prompt cache request failed: %v", err)
		}
	})

	t.Run("msgpack_gzip_payload", func(t *testing.T) {
		streamer := NewChatCompletionsStreamer(DefaultModel)
		streamer.ReasoningFormat = "hidden"
		liveTextRequest(t, streamer, "Reply with only: ok")
	})
}

func TestLiveChatCompletionsPromptCacheUsage(t *testing.T) {
	requireLiveCerebras(t)

	prefix := strings.Repeat("Stable Cerebras prompt cache prefix for agentloom live tests. ", 180)

	t.Run("omitted_then_hidden_reasoning_format_reuses_cache", func(t *testing.T) {
		key := "agentloom-live-cache-omitted-hidden-" + time.Now().UTC().Format("20060102T150405.000000000")
		first := livePromptCacheCachedTokens(t, key, "", prefix)
		t.Logf("omitted reasoning_format first cached_tokens=%d", first)
		second := livePromptCacheCachedTokens(t, key, "", prefix)
		if second <= 0 {
			t.Fatalf("second identical prompt-cache request cached_tokens=%d, want > 0", second)
		}
		hidden := livePromptCacheCachedTokens(t, key, "hidden", prefix)
		if hidden <= 0 {
			t.Fatalf("hidden reasoning_format after warmed omitted request cached_tokens=%d, want > 0", hidden)
		}
	})

	t.Run("hidden_then_omitted_reasoning_format_reuses_cache", func(t *testing.T) {
		key := "agentloom-live-cache-hidden-omitted-" + time.Now().UTC().Format("20060102T150405.000000000")
		first := livePromptCacheCachedTokens(t, key, "hidden", prefix)
		t.Logf("hidden reasoning_format first cached_tokens=%d", first)
		second := livePromptCacheCachedTokens(t, key, "hidden", prefix)
		if second <= 0 {
			t.Fatalf("second hidden prompt-cache request cached_tokens=%d, want > 0", second)
		}
		omitted := livePromptCacheCachedTokens(t, key, "", prefix)
		if omitted <= 0 {
			t.Fatalf("omitted reasoning_format after warmed hidden request cached_tokens=%d, want > 0", omitted)
		}
	})
}

func TestLiveChatCompletionsToolCallStreaming(t *testing.T) {
	requireLiveCerebras(t)

	streamer := NewChatCompletionsStreamer(DefaultModel)
	var items []threads.Item
	err := streamer.StreamReq(threads.Req{
		Instruction: "Call the requested tool exactly once. Do not output text.",
		Items: []threads.Item{
			threads.UserText(`Call echo_token with token "alpha".`),
		},
		Tools: threads.ToolOfferSnapshot{
			Offered: []threads.ToolSpec{{
				Name:        "echo_token",
				Description: "Records a token.",
				Payload: threads.ToolPayloadJSONSchema(gschema.Schema{
					Type: "object",
					Properties: map[string]*gschema.Schema{
						"token": {Type: "string"},
					},
					Required: []string{"token"},
				}),
			}},
			Allowed: []string{"echo_token"},
		},
	}, func(item threads.Item) error {
		items = append(items, item)
		return nil
	})
	if err != nil {
		t.Fatalf("live tool-call request failed: %v", err)
	}

	var chunks []threads.ToolCallChunk
	var finals []threads.ToolCall
	for _, item := range items {
		switch v := item.(type) {
		case threads.ToolCallChunk:
			chunks = append(chunks, v)
		case threads.ToolCall:
			finals = append(finals, v)
		}
	}
	if len(finals) == 0 {
		t.Fatalf("expected final tool call, got items %#v", items)
	}
	final := finals[0]
	if final.CallID == "" || final.Name != "echo_token" {
		t.Fatalf("unexpected final tool call: %#v", final)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(final.Payload), &payload); err != nil {
		t.Fatalf("final tool payload is not valid JSON: %q: %v", final.Payload, err)
	}
	if len(chunks) == 0 {
		t.Logf("Cerebras returned final tool call without a separate non-empty chunk; final=%#v", final)
	}
}

func TestLiveChatCompletionsToolSchemaAdditionalPropertiesOnlyObjectField(t *testing.T) {
	requireLiveCerebras(t)

	streamer := NewChatCompletionsStreamer(GLM47Model)
	err := streamer.StreamReq(threads.Req{
		Instruction: "Reply with only: ok. Do not call tools.",
		Items: []threads.Item{
			threads.UserText("Say ok."),
		},
		Tools: threads.ToolOfferSnapshot{
			Offered: []threads.ToolSpec{{
				Name:        "run_with_env",
				Description: "Runs with optional environment overrides.",
				Payload: threads.ToolPayloadJSONSchema(gschema.Schema{
					Type: "object",
					Properties: map[string]*gschema.Schema{
						"env": {
							Type:                 "object",
							AdditionalProperties: &gschema.Schema{Type: "string"},
						},
					},
				}),
			}},
		},
	}, func(threads.Item) error { return nil })
	if err != nil {
		t.Fatalf("live additionalProperties-only object field tool schema request failed: %v", err)
	}
}

func requireLiveCerebras(t testing.TB) {
	t.Helper()
	if os.Getenv("RUN_LIVE_API_TESTS") != "1" {
		t.Skip("set RUN_LIVE_API_TESTS=1 to run live API tests")
	}
	if cerebrasAPIKey() == "" {
		t.Skip("CEREBRAS_API_KEY is not set")
	}
}

func liveTextRequest(t testing.TB, streamer *ChatCompletionsStreamer, prompt string) []threads.Item {
	t.Helper()
	items, err := liveTextRequestErr(streamer, prompt)
	if err != nil {
		t.Fatalf("live Cerebras request failed: %v", err)
	}
	return items
}

func liveTextRequestErr(streamer *ChatCompletionsStreamer, prompt string) ([]threads.Item, error) {
	var items []threads.Item
	err := streamer.StreamReq(threads.Req{Items: []threads.Item{threads.UserText(prompt)}}, func(item threads.Item) error {
		items = append(items, item)
		return nil
	})
	return items, err
}

func assistantText(items []threads.Item) string {
	var text strings.Builder
	for _, item := range items {
		if v, ok := item.(threads.AssistantText); ok {
			text.WriteString(string(v))
		}
	}
	return text.String()
}

func livePromptCacheCachedTokens(t testing.TB, key, reasoningFormat, prefix string) int64 {
	t.Helper()
	streamer := NewChatCompletionsStreamer(GLM47Model)
	streamer.ReasoningEffort = "none"
	streamer.ReasoningFormat = reasoningFormat

	req := threads.Req{
		Instruction: prefix,
		Items:       []threads.Item{threads.UserText("Reply with only: ok")},
		ItemMeta: []map[string]any{
			cachecerebras.PromptCacheKey(key),
		},
	}
	messages, err := requestMessages(req)
	if err != nil {
		t.Fatalf("build prompt-cache messages: %v", err)
	}
	params := openaiapi.ChatCompletionNewParams{
		Model:               streamer.model,
		Messages:            messages,
		MaxCompletionTokens: openaiapi.Int(8),
		ReasoningEffort:     shared.ReasoningEffort(streamer.ReasoningEffort),
		StreamOptions: openaiapi.ChatCompletionStreamOptionsParam{
			IncludeUsage: openaiapi.Bool(true),
		},
		PromptCacheKey: openaiapi.String(key),
	}
	opts, err := streamer.requestOptions()
	if err != nil {
		t.Fatalf("build prompt-cache request options: %v", err)
	}

	stream := streamer.client.Chat.Completions.NewStreaming(t.Context(), params, opts...)
	defer stream.Close()

	var (
		sawUsage     bool
		cachedTokens int64
	)
	for stream.Next() {
		chunk := stream.Current()
		if !chunk.JSON.Usage.Valid() {
			continue
		}
		sawUsage = true
		if got := chunk.Usage.PromptTokensDetails.CachedTokens; got > cachedTokens {
			cachedTokens = got
		}
	}
	if err := stream.Err(); err != nil {
		if strings.Contains(err.Error(), "400 Bad Request") && strings.Contains(err.Error(), "stream_options") {
			t.Skipf("Cerebras rejected stream_options include_usage for prompt-cache probe: %v", err)
		}
		t.Fatalf("live prompt-cache usage request failed: %v", err)
	}
	if !sawUsage {
		t.Fatal("Cerebras prompt-cache probe did not return stream usage despite include_usage=true")
	}
	return cachedTokens
}

type cerebrasLiveHarness struct {
	streamer *ChatCompletionsStreamer
}

func (cerebrasLiveHarness) Capabilities() streamertest.Capabilities {
	return streamertest.Capabilities{
		AssistantTextChunks: true,
	}
}

func (h cerebrasLiveHarness) Stream(t testing.TB, req threads.Req, emit func(threads.Item) error) error {
	t.Helper()
	return h.streamer.StreamReq(req, emit)
}
