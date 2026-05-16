package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	openaiwrap "github.com/mackross/agentloom/llms/openai"
	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/tool"
)

type yesNoAnswer struct {
	Answer string `json:"answer" jsonschema:"exactly yes or no"`
	Reason string `json:"reason" jsonschema:"one short sentence explaining the answer"`
}

func main() {
	question := strings.TrimSpace(strings.Join(os.Args[1:], " "))
	if question == "" {
		fmt.Fprintln(os.Stderr, "usage: yesno <question>")
		os.Exit(2)
	}
	if strings.TrimSpace(os.Getenv("OPENAI_API_KEY")) == "" {
		fmt.Fprintln(os.Stderr, "set OPENAI_API_KEY")
		os.Exit(1)
	}

	var answer yesNoAnswer
	answered := false
	answerTool := tool.NewStructTool[yesNoAnswer]("answer_yes_no", "Answer the user's question with yes or no and a short reason.", func(_ context.Context, _ tool.Call, v yesNoAnswer) {
		answer = v
		answered = true
	})

	thread := threads.New()
	streamer := openaiwrap.NewResponsesStreamer(os.Getenv("OPENAI_MODEL"))
	streamer.Transport = openaiwrap.ResponsesTransportSSE
	thread.SetExecutor(threads.NewThreadExecutor(streamer))
	thread.SetToolProvider(answerTool)
	thread.SetToolResolver(answerTool)
	thread.QueueItem(threads.AssistantInstruction("Answer by calling the required tool. The answer field must be exactly yes or no."))
	thread.QueueItem(threads.UserText(question))
	thread.QueueItem(threads.SendItem{})
	if !answered {
		fmt.Fprintln(os.Stderr, "model did not return a structured yes/no answer")
		os.Exit(1)
	}

	fmt.Printf("%s\n%s\n", answer.Answer, answer.Reason)
}
