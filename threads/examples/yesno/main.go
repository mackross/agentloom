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

type answerDelegate struct {
	answer   yesNoAnswer
	answered bool
}

func (d *answerDelegate) OnStructToolCall(_ context.Context, _ *threads.Thread, call tool.Call, v yesNoAnswer) tool.Item {
	d.answer = v
	d.answered = true
	return tool.ResultText(call, "ok")
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

	delegate := &answerDelegate{}
	answerTool := tool.NewStructTool[yesNoAnswer]("answer_yes_no", "Answer the user's question with yes or no and a short reason.", delegate)

	thread := threads.New()
	streamer := openaiwrap.NewResponsesStreamer(os.Getenv("OPENAI_MODEL"))
	streamer.Transport = openaiwrap.ResponsesTransportSSE
	thread.SetExecutor(threads.NewThreadExecutor(streamer))
	thread.SetToolProvider(answerTool)
	thread.SetToolResolver(answerTool)
	thread.QueueItem(threads.AssistantInstruction("Answer by calling the required tool. The answer field must be exactly yes or no."))
	thread.QueueItem(threads.UserText(question))
	thread.QueueItem(threads.SendItem{})
	if !delegate.answered {
		fmt.Fprintln(os.Stderr, "model did not return a structured yes/no answer")
		os.Exit(1)
	}

	fmt.Printf("%s\n%s\n", delegate.answer.Answer, delegate.answer.Reason)
}
