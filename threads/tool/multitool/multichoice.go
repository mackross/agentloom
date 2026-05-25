package multitool

import (
	"context"
	"fmt"
	"strings"

	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/tool"
)

type Choice struct {
	Command     string
	Description string
}

type ChoiceFunc func(context.Context, threads.Thread, string, tool.ReturnItem) (tool.Handling, error)

type MultipleChoiceBuilder struct {
	question     string
	choices      []Choice
	fn           ChoiceFunc
	storeAnswer  *string
	staticOutput *string
}

// MultipleChoice starts a builder for a command-per-choice multitool config.
//
// Each choice becomes a subtool command. If the model calls the multitool
// without a command, or with an unknown command, the generated fallback explains
// the question and lists the valid commands.
//
// A yes/no choice tool can be built with the default generated fallback:
//
//	config := multitool.MultipleChoice("Is the sky blue on a clear day?").
//		Choice("yes", "the answer is yes").
//		Choice("no", "the answer is no").
//		Config()
//
// Or with static fallback text:
//
//	config := multitool.MultipleChoice("Is the sky blue on a clear day?").
//		Choice("yes", "the answer is yes").
//		Choice("no", "the answer is no").
//		Config().
//		WithFallback(multitool.StaticFallback("Call this tool with command `yes` or `no`."))
func MultipleChoice(question string) MultipleChoiceBuilder {
	return MultipleChoiceBuilder{question: question}
}

func (b MultipleChoiceBuilder) Choice(command, description string) MultipleChoiceBuilder {
	b.choices = append(append([]Choice(nil), b.choices...), Choice{Command: command, Description: description})
	return b
}

func (b MultipleChoiceBuilder) Choices(choices ...Choice) MultipleChoiceBuilder {
	b.choices = append(append([]Choice(nil), b.choices...), choices...)
	return b
}

func (b MultipleChoiceBuilder) Handle(fn ChoiceFunc) MultipleChoiceBuilder {
	b.fn = fn
	return b
}

func (b MultipleChoiceBuilder) StoreAnswer(dst *string) MultipleChoiceBuilder {
	b.storeAnswer = dst
	return b
}

func (b MultipleChoiceBuilder) ReturnStatic(output string) MultipleChoiceBuilder {
	b.staticOutput = &output
	return b
}

func (b MultipleChoiceBuilder) Config() Config {
	return MultipleChoiceConfig(b.question, b.choices, b.choiceFunc())
}

func (b MultipleChoiceBuilder) choiceFunc() ChoiceFunc {
	return func(ctx context.Context, thread threads.Thread, answer string, ret tool.ReturnItem) (tool.Handling, error) {
		if b.storeAnswer != nil {
			*b.storeAnswer = answer
		}
		if b.fn != nil {
			return b.fn(ctx, thread, answer, ret)
		}
		output := answer
		if b.staticOutput != nil {
			output = *b.staticOutput
		}
		return tool.Handling{Continue: threads.ToolContinueManual}, ret(threads.ToolCallResult{CallID: "", Output: output})
	}
}

func MultipleChoiceConfig(question string, choices []Choice, fn ChoiceFunc) Config {
	if fn == nil {
		fn = func(_ context.Context, _ threads.Thread, answer string, ret tool.ReturnItem) (tool.Handling, error) {
			return tool.Handling{Continue: threads.ToolContinueManual}, ret(threads.ToolCallResult{CallID: "", Output: answer})
		}
	}
	subtools := make([]Subtool, 0, len(choices))
	for _, choice := range choices {
		choice := choice
		subtools = append(subtools, Func(SubtoolSpec{Command: choice.Command, Description: choice.Description}, func(ctx context.Context, thread threads.Thread, call ToolCall, ret tool.ReturnItem) (tool.Handling, error) {
			return fn(ctx, thread, choice.Command, func(item tool.Item) error {
				if r, ok := item.(threads.ToolCallResult); ok && r.CallID == "" {
					r.CallID = call.CallID
					item = r
				}
				return ret(item)
			})
		}))
	}
	return Config{
		Subtools: subtools,
		Fallback: FallbackFunc(func(_ context.Context, _ threads.Thread, raw ToolCall, fallback Fallback, ret tool.ReturnItem) (tool.Handling, error) {
			return tool.Handling{}, ret(threads.ToolCallResult{CallID: raw.CallID, Output: multipleChoiceFallbackText(question, fallback.Subtools)})
		}),
	}
}

func multipleChoiceFallbackText(question string, choices []SubtoolSpec) string {
	var b strings.Builder
	question = strings.TrimSpace(question)
	if question != "" {
		fmt.Fprintf(&b, "Question:\n%s\n\n", question)
	}
	b.WriteString("Choose one answer by calling this tool with one of these commands:")
	if len(choices) == 0 {
		b.WriteString("\n  (none)")
		return b.String()
	}
	for _, choice := range choices {
		fmt.Fprintf(&b, "\n  %s", choice.Command)
		if choice.Description != "" {
			fmt.Fprintf(&b, " - %s", choice.Description)
		}
	}
	b.WriteString("\n\nDo not put the answer in input; use the command itself as the answer.")
	return b.String()
}
