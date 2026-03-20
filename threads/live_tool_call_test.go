//go:build live

package threads_test

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"

	openaiwrap "github.com/mackross/agentloom/llms/openai"
	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/simpletool"
)

func TestLiveThreadExecutesCalculatorToolWithOpenAIResponses(t *testing.T) {
	if os.Getenv("RUN_LIVE_API_TESTS") != "1" {
		t.Skip("set RUN_LIVE_API_TESTS=1 to run live API tests")
	}
	if strings.TrimSpace(os.Getenv("OPENAI_API_KEY")) == "" {
		t.Skip("OPENAI_API_KEY is not set")
	}
	model := strings.TrimSpace(os.Getenv("OPENAI_MODEL"))
	if model == "" {
		model = "gpt-5.2"
	}

	thread := threads.New()
	streamer := debugStreamer{t: t, inner: openaiwrap.NewResponsesStreamer(model)}
	thread.SetExecutor(threads.NewThreadExecutor(streamer))
	thread.SetToolProvider(simpletool.ProviderFunc(func() threads.ToolsSnapshot {
		return threads.ToolsSnapshot{
			Snapshot: threads.ToolOfferSnapshot{
				Offered: []threads.ToolSpec{{
					Name:        "calculator",
					Description: "Add two integers and return the sum as plain text.",
					Payload: threads.ToolPayloadFor[struct {
						A int `json:"a"`
						B int `json:"b"`
					}](),
				}},
				Allowed: []string{"calculator"},
			},
			Handlers: []threads.ToolHandlerBinding{{
				Name:            "calculator",
				HandlerLoadData: json.RawMessage(`{"operation":"add"}`),
			}},
		}
	}))

	resolveCalls := 0
	thread.SetToolResolver(simpletool.ResolverFunc(func(_ context.Context, call threads.ToolCall, handlerLoadData json.RawMessage) (threads.ToolDispatch, error) {
		resolveCalls++
		var args struct {
			A int `json:"a"`
			B int `json:"b"`
		}
		if err := call.UnmarshalJSON(&args); err != nil {
			return threads.ToolDispatch{
				Started: true,
				Items:   []threads.Item{threads.ToolCallResult{CallID: call.CallID, Output: err.Error()}},
			}, nil
		}
		return threads.ToolDispatch{
			Started: true,
			Items:   []threads.Item{threads.ToolCallResult{CallID: call.CallID, Output: strconv.Itoa(args.A + args.B)}},
		}, nil
	}))

	var out strings.Builder
	thread.SetDelegate(threads.ThreadDelegateFuncs{
		OnStreamItemAppended: func(_ *threads.Thread, item threads.Item) {
			t.Logf("stream item: %T %#v", item, item)
			if text, ok := item.(threads.AssistantText); ok {
				out.WriteString(string(text))
			}
		},
	})

	thread.QueueItem(threads.AssistantInstruction("Use the calculator tool exactly once. Do not do arithmetic yourself. After the tool returns, reply with only the final integer result."))
	thread.QueueItem(threads.UserText("What is 19 + 23?"))
	thread.QueueItem(threads.SendItem{})

	if resolveCalls != 1 {
		t.Fatalf("expected exactly one calculator tool call, got %d", resolveCalls)
	}
	if got := strings.TrimSpace(out.String()); !strings.Contains(got, "42") {
		t.Fatalf("expected final output to contain 42, got %q", got)
	}
}

type debugStreamer struct {
	t     *testing.T
	inner threads.LLMStreamer
}

func (s debugStreamer) Capabilities() threads.StreamerCapabilities {
	return s.inner.Capabilities()
}

func (s debugStreamer) StreamReq(req threads.Req, emit func(threads.Item) error) error {
	s.t.Logf("request instruction: %q", req.Instruction)
	s.t.Logf("request items: %#v", req.Items)
	s.t.Logf("request tools: %#v", req.Tools)
	return s.inner.StreamReq(req, func(item threads.Item) error {
		s.t.Logf("emit item: %T %#v", item, item)
		return emit(item)
	})
}
