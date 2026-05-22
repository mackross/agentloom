package multitool

import (
	"encoding/json"
	"fmt"
	"strings"
)

type jsonPayload struct {
	Command *string `json:"command,omitempty" jsonschema:"shell-like command line; first word selects the command and remaining words are arguments"`
	Input   *string `json:"input,omitempty" jsonschema:"optional freeform input for the command"`
}

func Parse(mode Mode, payload string) (Call, error) {
	switch mode {
	case ModeJSON:
		return ParseJSON(payload)
	case ModeLark, "":
		return ParseLark(payload)
	default:
		return Call{}, fmt.Errorf("unknown mode %q", mode)
	}
}

func Format(mode Mode, call Call) (string, error) {
	switch mode {
	case ModeJSON:
		return FormatJSON(call)
	case ModeLark, "":
		return FormatLark(call), nil
	default:
		return "", fmt.Errorf("unknown mode %q", mode)
	}
}

func ParseJSON(payload string) (Call, error) {
	if strings.TrimSpace(payload) == "" {
		return Call{}, nil
	}
	var raw jsonPayload
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		return Call{}, err
	}
	return parseCommandLine(raw.Command, raw.Input)
}

func FormatJSON(call Call) (string, error) {
	cmd := commandLine(call)
	buf, err := json.Marshal(jsonPayload{Command: cmd, Input: call.Input})
	if err != nil {
		return "", err
	}
	return string(buf), nil
}

func ParseLark(payload string) (Call, error) {
	if payload == "" {
		return Call{}, nil
	}
	head := payload
	var input *string
	if before, after, ok := strings.Cut(payload, "\n\n"); ok {
		head = before
		input = &after
	}
	head = strings.TrimSpace(strings.ReplaceAll(head, "\r\n", "\n"))
	var commandLine *string
	if head != "" || input != nil {
		commandLine = &head
	}
	return parseCommandLine(commandLine, input)
}

func FormatLark(call Call) string {
	cmd := commandLine(call)
	var head string
	if cmd != nil {
		head = *cmd
	}
	if call.Input == nil {
		return head
	}
	return head + "\n\n" + *call.Input
}

func parseCommandLine(line *string, input *string) (Call, error) {
	if line == nil {
		return Call{Input: input}, nil
	}
	fields, err := splitCommandLine(*line)
	if err != nil {
		return Call{}, err
	}
	if len(fields) == 0 {
		empty := ""
		return Call{Command: &empty, Input: input}, nil
	}
	cmd := fields[0]
	return Call{Command: &cmd, Args: fields[1:], Input: input}, nil
}

func commandLine(call Call) *string {
	if call.Command == nil {
		return nil
	}
	parts := append([]string{*call.Command}, call.Args...)
	line := strings.Join(parts, " ")
	return &line
}

func splitCommandLine(s string) ([]string, error) {
	var out []string
	var b strings.Builder
	inQuote := rune(0)
	escaped := false
	hadToken := false
	flush := func() {
		if hadToken {
			out = append(out, b.String())
			b.Reset()
			hadToken = false
		}
	}
	for _, r := range s {
		if escaped {
			b.WriteRune(r)
			hadToken = true
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			hadToken = true
			continue
		}
		if inQuote != 0 {
			if r == inQuote {
				inQuote = 0
				continue
			}
			b.WriteRune(r)
			hadToken = true
			continue
		}
		switch r {
		case '\'', '"':
			inQuote = r
			hadToken = true
		case ' ', '\t', '\n', '\r':
			flush()
		default:
			b.WriteRune(r)
			hadToken = true
		}
	}
	if escaped {
		b.WriteRune('\\')
	}
	if inQuote != 0 {
		return nil, fmt.Errorf("unterminated quote")
	}
	flush()
	return out, nil
}
