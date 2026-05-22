package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	openaiwrap "github.com/mackross/agentloom/llms/openai"
	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/tool"
	"github.com/mackross/agentloom/threads/tool/multitool"
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
	args := os.Args[1:]
	useMulti := false
	filtered := args[:0]
	for _, arg := range args {
		if arg == "--multi" {
			useMulti = true
			continue
		}
		filtered = append(filtered, arg)
	}
	question := strings.TrimSpace(strings.Join(filtered, " "))
	if question == "" {
		fmt.Fprintln(os.Stderr, "usage: yesno [--multi] <question>")
		os.Exit(2)
	}
	if strings.TrimSpace(os.Getenv("OPENAI_API_KEY")) == "" {
		fmt.Fprintln(os.Stderr, "set OPENAI_API_KEY")
		os.Exit(1)
	}

	thread := threads.New()
	model := os.Getenv("OPENAI_MODEL")
	if useMulti && strings.TrimSpace(model) == "" {
		// Lark/custom tools require a model/API path that supports Responses
		// custom tools. GPT-4-family models commonly reject these with
		// "Invalid value: 'custom'".
		model = "gpt-5.5"
	}
	streamer := openaiwrap.NewResponsesStreamer(model)
	streamer.Transport = openaiwrap.ResponsesTransportSSE
	thread.SetExecutor(threads.NewThreadExecutor(streamer))

	if useMulti {
		var answer string
		answerTool := multitool.New(multitool.Setup{
			Name:        "answer_yes_no",
			Description: "Answer the user's question by choosing yes or no.",
			Mode:        multitool.ModeLark,
			Required:    true,
		}, multitool.MultipleChoice(question).
			Choice("yes", "the answer is yes").
			Choice("no", "the answer is no").
			Handle(func(_ context.Context, _ *threads.Thread, selected string, ret tool.ReturnItem) (tool.Handling, error) {
				answer = selected
				fmt.Fprintf(os.Stderr, "multitool selected answer: %s\n", selected)
				return tool.Handling{Continue: threads.ToolContinueManual}, ret(threads.ToolCallResult{Output: "ok"})
			}).
			Config())
		thread.SetToolProvider(answerTool)
		thread.SetToolResolver(answerTool)
		thread.SetDelegate(threads.ThreadDelegateFuncs{
			OnStreamItemAppended: func(_ *threads.Thread, item threads.Item) {
				fmt.Fprintf(os.Stderr, "stream item: %#v\n", item)
			},
			OnExecutorError: func(_ *threads.Thread, err error) {
				fmt.Fprintf(os.Stderr, "executor error: %v\n", err)
			},
		})
		thread.QueueItem(threads.AssistantInstruction("Answer by calling the required tool with command yes or no."))
		thread.QueueItem(threads.UserText(question))
		thread.QueueItem(threads.SendItem{})
		if answer == "" {
			fmt.Fprintln(os.Stderr, "model did not return a yes/no answer")
			os.Exit(1)
		}
		fmt.Printf("%s\n\n", answer)
		return
	}

	delegate := &answerDelegate{}
	answerTool := tool.NewStructTool[yesNoAnswer]("answer_yes_no", "Answer the user's question with yes or no and a short reason.", delegate)

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
