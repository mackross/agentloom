//go:build live

package threads_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	openaiwrap "github.com/mackross/agentloom/llms/openai"
	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/simpletool"
	"github.com/mackross/agentloom/threads/tool"
	"github.com/mackross/agentloom/threads/tool/multitool"
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
	thread.SetToolProvider(simpletool.ProviderFunc(func(_ *threads.Thread) threads.ToolsSnapshot {
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
	thread.SetToolResolver(simpletool.ResolverFunc(func(_ context.Context, _ *threads.Thread, call threads.ToolCall, handlerLoadData json.RawMessage) (threads.ToolDispatch, error) {
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

func TestLiveMultitoolLarkJSONRepairWithOpenAIResponses(t *testing.T) {
	if os.Getenv("RUN_LIVE_API_TESTS") != "1" {
		t.Skip("set RUN_LIVE_API_TESTS=1 to run live API tests")
	}
	if strings.TrimSpace(os.Getenv("OPENAI_API_KEY")) == "" {
		t.Skip("OPENAI_API_KEY is not set")
	}
	model := strings.TrimSpace(os.Getenv("OPENAI_MULTITOOL_LIVE_MODEL"))
	if model == "" {
		model = "gpt-5.5"
	}

	type ticketArgs struct {
		Title    string `json:"title" jsonschema:"ticket title"`
		Priority int    `json:"priority" jsonschema:"integer priority from 1 to 5"`
	}

	successes := 0
	mt := multitool.New(multitool.Setup{
		Name: "multi",
		Description: "Call one command. Available command: create-ticket. Use the command name on the first line and JSON input after a blank line.",
		Mode: multitool.ModeLark,
	}, multitool.Config{Subtools: []multitool.Subtool{
		multitool.JSONHandler[ticketArgs](multitool.SubtoolSpec{
			Command:     "create-ticket",
			Description: "Create a ticket. The JSON input must contain title as a string and priority as an integer.",
			Usage:       `{"title":"rollback live test","priority":3}`,
		}, "create_ticket", tool.JSONHandler(func(_ context.Context, _ *threads.Thread, call tool.Call, args ticketArgs) tool.Item {
			successes++
			return tool.ResultText(call, fmt.Sprintf("created ticket %q with priority %d", args.Title, args.Priority))
		})),
	}})

	thread := threads.New()
	streamer := debugStreamer{t: t, inner: openaiwrap.NewResponsesStreamer(model)}
	thread.SetExecutor(threads.NewThreadExecutor(streamer))
	thread.SetToolProvider(mt)
	thread.SetToolResolver(mt)

	var out strings.Builder
	thread.SetDelegate(threads.ThreadDelegateFuncs{
		OnStreamItemAppended: func(_ *threads.Thread, item threads.Item) {
			t.Logf("stream item: %T %#v", item, item)
			if text, ok := item.(threads.AssistantText); ok {
				out.WriteString(string(text))
			}
		},
	})

	thread.QueueItem(threads.AssistantInstruction("Use the multi tool. On the first create-ticket call, intentionally make the JSON schema wrong by setting priority to the string \"urgent\". If a tool_call_hint is present, that means the wrong initial attempt already happened; do not intentionally make it wrong again. Read the hint, repair the JSON, and call create-ticket with priority as the integer 3. After a successful tool result, reply exactly DONE."))
	thread.QueueItem(threads.UserText("Create a ticket titled \"rollback live test\" with priority 3. This is a repair-loop test: first call create-ticket with priority as the string \"urgent\", then fix it after the tool reports the schema error."))
	thread.QueueItem(threads.SendItem{})

	snapshot, err := thread.Snapshot()
	if err != nil {
		t.Fatalf("snapshot thread: %v", err)
	}
	var toolCalls, rollbackableFailures int
	for _, item := range snapshot.Items {
		switch item.Type {
		case "tool_call":
			if item.Name == "multi" {
				toolCalls++
			}
		case "tool_result":
			if item.SafeRollback != nil {
				rollbackableFailures++
				t.Logf("rollbackable result: %s", item.Output)
				t.Logf("steering hint: %s", item.SafeRollback.SteeringHint)
			}
		}
	}

	if rollbackableFailures == 0 {
		t.Fatalf("expected at least one rollbackable JSON parse/schema failure; toolCalls=%d successes=%d final=%q", toolCalls, successes, out.String())
	}
	if successes != 1 {
		t.Fatalf("expected exactly one successful create-ticket call after repair, got %d; toolCalls=%d final=%q", successes, toolCalls, out.String())
	}
	if toolCalls < 2 {
		t.Fatalf("expected initial failed call and repaired call, got %d", toolCalls)
	}
	if got := strings.TrimSpace(out.String()); got != "DONE" {
		t.Fatalf("expected final assistant output DONE, got %q", got)
	}
}

type debugStreamer struct {
	t     *testing.T
	inner threads.LLMStreamer
}

func (s debugStreamer) Capabilities() threads.StreamerCapabilities {
	return s.inner.Capabilities()
}

func (s debugStreamer) RegisterToolNormalizer(name string, normalizer threads.ToolNormalizer) {
	s.inner.RegisterToolNormalizer(name, normalizer)
}

func (s debugStreamer) UnregisterToolNormalizer(name string) {
	s.inner.UnregisterToolNormalizer(name)
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
