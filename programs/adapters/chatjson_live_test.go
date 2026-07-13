//go:build live

package adapters

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mackross/agentloom/llms/openai"
	"github.com/mackross/agentloom/programs"
	"github.com/mackross/agentloom/threads"
)

type liveSignatureInput struct {
	Question string `json:"question" jsonschema:"question to answer"`
}

type liveSignatureOutput struct {
	Answer string `json:"answer" jsonschema:"answer with exactly the requested word"`
}

func TestLiveChatJSONOpenAI(t *testing.T) {
	if os.Getenv("RUN_LIVE_API_TESTS") != "1" {
		t.Skip("set RUN_LIVE_API_TESTS=1 to run live API tests")
	}
	if strings.TrimSpace(os.Getenv("OPENAI_API_KEY")) == "" {
		t.Skip("OPENAI_API_KEY is not set")
	}

	model := strings.TrimSpace(os.Getenv("OPENAI_MODEL"))
	if model == "" {
		model = openai.DefaultModel
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	branch := newLiveSignatureBranch(t, ctx)
	branch.SetExecutor(threads.NewThreadExecutor(openai.NewResponsesStreamer(model)))

	chat := ChatJSON[liveSignatureInput, liveSignatureOutput]{
		Signature: programs.Signature[liveSignatureInput, liveSignatureOutput]{
			Name: "live_chat_json_smoke",
			Instruction: strings.Join([]string{
				"Return JSON only.",
				"The answer field must contain exactly the lowercase word requested by the question and no punctuation.",
			}, " "),
		},
	}

	out, err := chat.Run(ctx, branch, liveSignatureInput{Question: "Put exactly the word ok in the answer field."})
	if err != nil {
		t.Fatalf("ChatJSON.Run: %v", err)
	}
	if strings.TrimSpace(strings.ToLower(out.Answer)) != "ok" {
		t.Fatalf("Answer = %q, want ok", out.Answer)
	}
}

func newLiveSignatureBranch(t *testing.T, ctx context.Context) *threads.Branch {
	t.Helper()
	store := threads.NewMemoryBranchStore()
	stored, err := store.CreateBranch(ctx, threads.BranchCreateOptions{ID: "live-chat-json"})
	if err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if err := stored.Close(); err != nil {
		t.Fatalf("Close stored branch: %v", err)
	}
	branch, err := threads.NewDefaultBranchManager(store, "test").Open(ctx, "/branch/live-chat-json")
	if err != nil {
		t.Fatalf("Open branch: %v", err)
	}
	t.Cleanup(func() { _ = branch.Close() })
	return branch
}
