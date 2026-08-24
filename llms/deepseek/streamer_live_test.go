//go:build live

package deepseek

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/streamerlivetest"
	"github.com/mackross/agentloom/threads/tool/filetool"
)

func TestLiveCapabilities(t *testing.T) {
	if strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY")) == "" {
		t.Fatal("DEEPSEEK_API_KEY is not set")
	}
	model := strings.TrimSpace(os.Getenv("DEEPSEEK_MODEL"))
	if model == "" {
		model = DefaultModel
	}
	streamer := NewResponsesStreamer(model)
	streamerlivetest.Run(t, deepseekLiveHarness{
		SupportsReasoningToolLoop: streamerlivetest.SupportsReasoningToolLoop{Streamer: streamer},
		streamer:                  streamer,
	})
}

type deepseekLiveHarness struct {
	streamerlivetest.SupportsReasoningToolLoop
	streamer *ResponsesStreamer
}

func (h deepseekLiveHarness) Stream(t testing.TB, req threads.Req, emit func(threads.Item) error) error {
	t.Helper()
	return h.streamer.StreamReq(req, emit)
}

func TestLiveResponsesApplyPatchLarkToolLoop(t *testing.T) {
	if strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY")) == "" {
		t.Fatal("DEEPSEEK_API_KEY is not set")
	}
	model := strings.TrimSpace(os.Getenv("DEEPSEEK_MODEL"))
	if model == "" {
		model = DefaultModel
	}
	streamer := NewResponsesStreamer(model)
	dir := t.TempDir()
	patchTool := filetool.NewApplyPatchTool(filetool.ApplyPatchConfig{CWD: dir, Mode: filetool.ApplyPatchModeLark})
	tools := patchTool.ToolsSnapshot(nil).Snapshot
	if len(tools.Offered) != 1 {
		t.Fatalf("apply_patch tools = %#v", tools)
	}
	if _, ok := tools.Offered[0].Payload.(threads.ToolPayloadLark); !ok {
		t.Fatalf("apply_patch payload = %T, want Lark", tools.Offered[0].Payload)
	}
	build := func(items []threads.Item) threads.Req {
		req := threads.DefaultRequestBuilder.Build(items, streamer.Capabilities())
		req.Tools = tools
		return req
	}
	stream := func(req threads.Req) []threads.Item {
		var out []threads.Item
		if err := streamer.StreamReq(req, func(item threads.Item) error { out = append(out, item); return nil }); err != nil {
			t.Fatalf("live stream request: %v", err)
		}
		return out
	}

	history := []threads.Item{threads.UserText("Reason carefully about whether this patch satisfies the apply_patch grammar, then call apply_patch exactly once with this exact patch and wait for the result:\n*** Begin Patch\n*** Add File: deepseek_lark_probe.txt\n+deepseek-lark-ok\n*** End Patch")}
	var first []threads.Item
	var reasoning threads.ReasoningItem
	var call threads.ToolCall
	for attempt := 1; attempt <= 3; attempt++ {
		reasoning, call = threads.ReasoningItem{}, threads.ToolCall{}
		first = stream(build(history))
		for _, item := range first {
			if v, ok := item.(threads.ReasoningItem); ok {
				reasoning = v
			}
			if v, ok := item.(threads.ToolCall); ok {
				call = v
			}
		}
		if reasoning.Provider != "" && call.CallID != "" {
			break
		}
	}
	if reasoning.Provider != reasoningProvider || reasoning.ID == "" || reasoning.Text == "" || len(reasoning.Opaque) != 0 || call.CallID == "" {
		t.Fatalf("reasoning/tool call not observed: %#v", first)
	}
	if call.Name != "apply_patch" || !strings.Contains(call.Payload, "*** Begin Patch") || !strings.Contains(call.Payload, "*** Add File: deepseek_lark_probe.txt") || !strings.Contains(call.Payload, "*** End Patch") {
		t.Fatalf("apply_patch call = %#v", call)
	}
	dispatch, err := patchTool.ResolveTool(context.Background(), nil, call, nil)
	if err != nil || len(dispatch.Items) != 1 {
		t.Fatalf("resolve apply_patch: dispatch=%#v err=%v", dispatch, err)
	}
	result, ok := dispatch.Items[0].(threads.ToolCallResult)
	if !ok {
		t.Fatalf("apply_patch result = %#v", dispatch.Items)
	}
	content, err := os.ReadFile(filepath.Join(dir, "deepseek_lark_probe.txt"))
	if err != nil || string(content) != "deepseek-lark-ok\n" {
		t.Fatalf("applied file = %q, err=%v", content, err)
	}

	history = append(history, first...)
	history = append(history, result)
	secondReq := build(history)
	reasoningAt, callAt := -1, -1
	for i, item := range secondReq.Items {
		if reflect.DeepEqual(item, reasoning) {
			reasoningAt = i
		}
		if v, ok := item.(threads.ToolCall); ok && v.CallID == call.CallID {
			callAt = i
		}
	}
	if reasoningAt < 0 || callAt < 0 || reasoningAt >= callAt {
		t.Fatal("tool-result request did not retain compact reasoning before apply_patch")
	}
	for _, item := range stream(secondReq) {
		if text, ok := item.(threads.AssistantText); ok && strings.TrimSpace(string(text)) != "" {
			return
		}
	}
	t.Fatal("tool-result continuation contained no final assistant text")
}
