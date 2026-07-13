package adapters

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mackross/agentloom/programs"
	"github.com/mackross/agentloom/threads"
)

type signatureInput struct {
	Question string `json:"question" jsonschema:"question to answer"`
	Context  string `json:"context" jsonschema:"context to use"`
}

type signatureOutput struct {
	Answer string `json:"answer" jsonschema:"concise answer"`
}

func TestChatJSONPromptGolden(t *testing.T) {
	chat := ChatJSON[signatureInput, signatureOutput]{
		Signature: programs.Signature[signatureInput, signatureOutput]{
			Name:        "answer_question",
			Instruction: "Answer from the context.",
		},
	}
	got, err := chat.prompt(signatureInput{
		Question: "What changed?",
		Context:  "A programs package was added.",
	})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "chat_json_prompt.golden"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if got != string(want) {
		t.Fatalf("golden mismatch for chat_json_prompt.golden\n--- got ---\n%s\n--- want ---\n%s", got, string(want))
	}
}

func TestChatJSONUsesJSONSchemaTagsAndParsesJSONOutput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	branch := newSignatureTestBranch(t, ctx)
	streamer := &signatureTestStreamer{
		t:     t,
		reply: `{"answer":"AgentLoom adds programs."}`,
		assertRequest: func(t *testing.T, req threads.Req) {
			t.Helper()
			if req.Instruction != "Answer from the context." {
				t.Fatalf("Instruction = %q", req.Instruction)
			}
			if len(req.Items) != 1 {
				t.Fatalf("len(req.Items) = %d, want 1", len(req.Items))
			}
			input, ok := req.Items[0].(threads.UserText)
			if !ok {
				t.Fatalf("request item = %T, want threads.UserText", req.Items[0])
			}
			text := string(input)
			for _, want := range []string{
				`"question": "What changed?"`,
				`"context": "A programs package was added."`,
				`"answer"`,
				`concise answer`,
				`Return only JSON`,
			} {
				if !strings.Contains(text, want) {
					t.Fatalf("prompt missing %q:\n%s", want, text)
				}
			}
		},
	}
	branch.SetExecutor(threads.NewThreadExecutor(streamer))

	chat := ChatJSON[signatureInput, signatureOutput]{
		Signature: programs.Signature[signatureInput, signatureOutput]{
			Name:        "answer_question",
			Instruction: "Answer from the context.",
		},
	}
	out, err := chat.Run(ctx, branch, signatureInput{
		Question: "What changed?",
		Context:  "A programs package was added.",
	})
	if err != nil {
		t.Fatalf("ChatJSON.Run: %v", err)
	}
	if out.Answer != "AgentLoom adds programs." {
		t.Fatalf("Answer = %q", out.Answer)
	}
	if streamer.calls != 1 {
		t.Fatalf("stream calls = %d, want 1", streamer.calls)
	}
}

func newSignatureTestBranch(t *testing.T, ctx context.Context) *threads.Branch {
	t.Helper()
	store := threads.NewMemoryBranchStore()
	stored, err := store.CreateBranch(ctx, threads.BranchCreateOptions{ID: "predict"})
	if err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if err := stored.Close(); err != nil {
		t.Fatalf("Close stored branch: %v", err)
	}
	branch, err := threads.NewDefaultBranchManager(store, "test").Open(ctx, "/branch/predict")
	if err != nil {
		t.Fatalf("Open branch: %v", err)
	}
	t.Cleanup(func() { _ = branch.Close() })
	return branch
}

type signatureTestStreamer struct {
	t             *testing.T
	reply         string
	emit          threads.Item
	replies       []string
	assertRequest func(*testing.T, threads.Req)
	calls         int
	requests      []threads.Req
}

func (s *signatureTestStreamer) Capabilities() threads.StreamerCapabilities {
	return threads.StreamerCapabilities{}
}

func (*signatureTestStreamer) RegisterToolNormalizer(string, threads.ToolNormalizer) {}

func (*signatureTestStreamer) UnregisterToolNormalizer(string) {}

func (*signatureTestStreamer) SyntheticToolCallID() string { return "" }

func (s *signatureTestStreamer) StreamReq(req threads.Req, emit func(threads.Item) error) error {
	s.requests = append(s.requests, req)
	s.calls++
	if s.assertRequest != nil {
		s.assertRequest(s.t, req)
	}
	if s.emit != nil {
		return emit(s.emit)
	}
	if len(s.replies) > 0 {
		reply := s.replies[0]
		s.replies = s.replies[1:]
		return emit(threads.AssistantText(reply))
	}
	return emit(threads.AssistantText(s.reply))
}

func TestChatJSONParseOutputUnmarshalsJSON(t *testing.T) {
	out, err := parseJSONOutput[signatureOutput]("```json\n{\"answer\":\"ok\"}\n```")
	if err != nil {
		t.Fatalf("parseJSONOutput: %v", err)
	}
	if out.Answer != "ok" {
		t.Fatalf("Answer = %q, want ok", out.Answer)
	}
}

func TestChatJSONRetriesInvalidJSONByRollingForwardWithSteeringHint(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	branch := newSignatureTestBranch(t, ctx)
	streamer := &signatureTestStreamer{
		t:       t,
		replies: []string{`not json`, `{"answer":"fixed"}`},
	}
	branch.SetExecutor(threads.NewThreadExecutor(streamer))

	chat := ChatJSON[signatureInput, signatureOutput]{
		Signature: programs.Signature[signatureInput, signatureOutput]{
			Name: "answer_question",
		},
	}
	out, err := chat.Run(ctx, branch, signatureInput{
		Question: "What changed?",
		Context:  "A retry was needed.",
	})
	if err != nil {
		t.Fatalf("ChatJSON.Run: %v", err)
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
	if len(second.Items) == 0 {
		t.Fatal("second request has no items")
	}
	hint, ok := second.Items[len(second.Items)-1].(threads.UserText)
	if !ok {
		t.Fatalf("last second request item = %T, want UserText", second.Items[len(second.Items)-1])
	}
	for _, want := range []string{"previous assistant response", "parse chat JSON output", "Return only JSON"} {
		if !strings.Contains(string(hint), want) {
			t.Fatalf("retry hint missing %q:\n%s", want, string(hint))
		}
	}
}
