package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/mackross/agentloom/programs"
	"github.com/mackross/agentloom/threads"
)

// ErrChatJSONNoOutput is returned when ChatJSON completes without a new assistant response.
var ErrChatJSONNoOutput = errors.New("programs/adapters: chat JSON produced no assistant output")

const defaultChatJSONMaxRetries = 10

// ChatJSON executes a Signature by sending JSON input in chat and parsing JSON
// from the next assistant response.
type ChatJSON[I, O any] struct {
	Signature programs.Signature[I, O]
	// MaxRetries is the number of invalid assistant JSON responses the model may
	// correct. The zero value uses the default of 10. Negative values disable
	// retries.
	MaxRetries int
}

// Run executes c on t.
func (c ChatJSON[I, O]) Run(ctx context.Context, t threads.Thread, input I) (O, error) {
	var zero O

	prompt, err := c.prompt(input)
	if err != nil {
		return zero, err
	}

	turnsBefore := len(t.CompletedTurns())
	if c.Signature.Instruction != "" {
		t.QueueItem(threads.AssistantInstruction(c.Signature.Instruction))
	}
	t.QueueItem(threads.UserText(prompt))
	t.QueueItem(threads.SendItem{})

	if err := t.WaitUntilIdle(ctx); err != nil {
		return zero, err
	}

	attempts := 0
	turns := t.CompletedTurns()
retry:
	for {
		for i := len(turns) - 1; i >= turnsBefore; i-- {
			if turns[i].Role() != threads.TurnAssistant {
				continue
			}
			out, err := parseJSONOutput[O](turns[i].Text())
			if err == nil {
				return out, nil
			}
			if attempts >= c.maxRetries() {
				return zero, err
			}
			attempts++
			turnsBefore = len(turns)
			t.QueueItem(threads.UserText(c.retryHint(err)))
			t.QueueItem(threads.SendItem{})
			if err := t.WaitUntilIdle(ctx); err != nil {
				return zero, err
			}
			turns = t.CompletedTurns()
			continue retry
		}
		break
	}

	return zero, ErrChatJSONNoOutput
}

func (c ChatJSON[I, O]) maxRetries() int {
	if c.MaxRetries == 0 {
		return defaultChatJSONMaxRetries
	}
	return c.MaxRetries
}

func (c ChatJSON[I, O]) retryHint(err error) string {
	return fmt.Sprintf("The previous assistant response could not be parsed as the required JSON output: %v\n\nReturn only JSON matching the output schema.", err)
}

func (c ChatJSON[I, O]) prompt(input I) (string, error) {
	inputJSON, err := c.Signature.InputJSON(input)
	if err != nil {
		return "", err
	}

	schema, err := c.Signature.OutputSchema()
	if err != nil {
		return "", err
	}
	schemaJSON, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal signature output schema: %w", err)
	}

	var b strings.Builder
	if c.Signature.Name != "" {
		fmt.Fprintf(&b, "Signature: %s\n\n", c.Signature.Name)
	}
	b.WriteString("Input JSON:\n")
	b.Write(inputJSON)
	b.WriteString("\n\nOutput JSON Schema:\n")
	b.Write(schemaJSON)
	b.WriteString("\n\nReturn only JSON matching the output schema.")
	return b.String(), nil
}

func parseJSONOutput[O any](text string) (O, error) {
	var out O
	if err := json.Unmarshal([]byte(extractJSON(text)), &out); err != nil {
		return out, fmt.Errorf("parse chat JSON output: %w", err)
	}
	return out, nil
}

func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		lines := strings.Split(s, "\n")
		if len(lines) >= 3 {
			lines = lines[1 : len(lines)-1]
			s = strings.TrimSpace(strings.Join(lines, "\n"))
		}
	}
	return s
}
