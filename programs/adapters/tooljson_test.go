package adapters

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mackross/agentloom/programs"
	"github.com/mackross/agentloom/threads"
	threadtool "github.com/mackross/agentloom/threads/tool"
)

func TestToolJSONCapturesToolArgumentsAndRestoresTools(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	branch := newSignatureTestBranch(t, ctx)
	oldTool := threadtool.NewStructTool[struct {
		Value string `json:"value"`
	}]("old_tool", "old tool", nil)
	branch.SetToolProvider(oldTool)
	branch.SetToolResolver(oldTool)

	streamer := &signatureTestStreamer{
		t:     t,
		reply: "",
		assertRequest: func(t *testing.T, req threads.Req) {
			t.Helper()
			if !req.Tools.Required {
				t.Fatalf("Tools.Required = false, want true")
			}
			if len(req.Tools.Offered) != 1 || req.Tools.Offered[0].Name != "submit_output" {
				t.Fatalf("Offered = %#v, want only submit_output", req.Tools.Offered)
			}
			if len(req.Tools.Allowed) != 1 || req.Tools.Allowed[0] != "submit_output" {
				t.Fatalf("Allowed = %#v, want submit_output", req.Tools.Allowed)
			}
		},
	}
	streamer.emit = threads.ToolCall{CallID: "out-1", Name: "submit_output", Payload: `{"answer":"via tool"}`}
	branch.SetExecutor(threads.NewThreadExecutor(streamer))

	toolJSON := ToolJSON[signatureInput, signatureOutput]{
		Signature: programs.Signature[signatureInput, signatureOutput]{
			Name:        "answer_question",
			Instruction: "Answer from the context.",
		},
	}
	out, err := toolJSON.Run(ctx, branch, signatureInput{Question: "What changed?", Context: "A tool was used."})
	if err != nil {
		t.Fatalf("ToolJSON.Run: %v", err)
	}
	if out.Answer != "via tool" {
		t.Fatalf("Answer = %q, want via tool", out.Answer)
	}
	if branch.ToolProvider() != oldTool {
		t.Fatal("ToolProvider was not restored")
	}
	if branch.ToolResolver() != oldTool {
		t.Fatal("ToolResolver was not restored")
	}
}

func TestToolJSONRetriesInvalidToolPayloadWithSafeRollbackHint(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	branch := newSignatureTestBranch(t, ctx)
	streamer := &toolJSONRetryStreamer{
		items: []threads.Item{
			threads.ToolCall{CallID: "bad-1", Name: "submit_output", Payload: `{"answer":1}`},
			threads.ToolCall{CallID: "good-1", Name: "submit_output", Payload: `{"answer":"fixed"}`},
		},
	}
	branch.SetExecutor(threads.NewThreadExecutor(streamer))

	toolJSON := ToolJSON[signatureInput, signatureOutput]{
		Signature: programs.Signature[signatureInput, signatureOutput]{
			Name: "answer_question",
		},
	}
	out, err := toolJSON.Run(ctx, branch, signatureInput{Question: "What changed?", Context: "A retry was needed."})
	if err != nil {
		t.Fatalf("ToolJSON.Run: %v", err)
	}
	if out.Answer != "fixed" {
		t.Fatalf("Answer = %q, want fixed", out.Answer)
	}
	if streamer.calls != 2 {
		t.Fatalf("stream calls = %d, want 2", streamer.calls)
	}
	if len(streamer.requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(streamer.requests))
	}
	second := streamer.requests[1]
	var sawHint bool
	for _, item := range second.Items {
		switch v := item.(type) {
		case threads.UserText:
			if strings.Contains(string(v), "invalid JSON arguments") {
				sawHint = true
			}
		case threads.ToolCall, threads.ToolCallResult:
			t.Fatalf("rollback projection kept failed tool item in second request: %#v", item)
		}
	}
	if !sawHint {
		t.Fatalf("second request did not include retry hint: %#v", second.Items)
	}
}

type toolJSONRetryStreamer struct {
	items    []threads.Item
	calls    int
	requests []threads.Req
}

func (*toolJSONRetryStreamer) Capabilities() threads.StreamerCapabilities {
	return threads.StreamerCapabilities{AssistantPrefix: true}
}

func (*toolJSONRetryStreamer) RegisterToolNormalizer(string, threads.ToolNormalizer) {}

func (*toolJSONRetryStreamer) UnregisterToolNormalizer(string) {}

func (*toolJSONRetryStreamer) SyntheticToolCallID() string { return "" }

func (s *toolJSONRetryStreamer) StreamReq(req threads.Req, emit func(threads.Item) error) error {
	s.requests = append(s.requests, req)
	i := s.calls
	s.calls++
	if i >= len(s.items) {
		return nil
	}
	return emit(s.items[i])
}
