package truncate

import (
	"fmt"
	"unicode/utf8"
)

const (
	DefaultMaxLines = 2000
	DefaultMaxBytes = 50 * 1024
)

// Result is the output of a truncation operation.
type Result struct {
	Content   string
	Truncated bool
	Notice    string
}

// Head keeps the beginning of s within the configured limits.
func Head(s string, maxLines, maxBytes int) Result {
	maxLines, maxBytes = limits(maxLines, maxBytes)
	out, truncated := headLines(s, maxLines)
	if len(out) > maxBytes {
		out = headBytes(out, maxBytes)
		truncated = true
	}
	return result(out, truncated)
}

// Tail keeps the end of s within the configured limits.
func Tail(s string, maxLines, maxBytes int) Result {
	maxLines, maxBytes = limits(maxLines, maxBytes)
	out, truncated := tailLines(s, maxLines)
	if len(out) > maxBytes {
		out = tailBytes(out, maxBytes)
		truncated = true
	}
	return result(out, truncated)
}

func limits(maxLines, maxBytes int) (int, int) {
	if maxLines <= 0 {
		maxLines = DefaultMaxLines
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	return maxLines, maxBytes
}

func headLines(s string, max int) (string, bool) {
	if max <= 0 || s == "" {
		return s, false
	}
	lines := 1
	for i := range s {
		if s[i] != '\n' {
			continue
		}
		if lines == max {
			if i+1 < len(s) {
				return s[:i+1], true
			}
			return s, false
		}
		lines++
	}
	return s, false
}

func tailLines(s string, max int) (string, bool) {
	if max <= 0 || s == "" {
		return s, false
	}
	lines := 1
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] != '\n' || i == len(s)-1 {
			continue
		}
		if lines == max {
			return s[i+1:], true
		}
		lines++
	}
	return s, false
}

func headBytes(s string, max int) string {
	for max > 0 && max < len(s) && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}

func tailBytes(s string, max int) string {
	start := len(s) - max
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}

func result(content string, truncated bool) Result {
	if !truncated {
		return Result{Content: content}
	}
	return Result{
		Content:   content,
		Truncated: true,
		Notice:    fmt.Sprintf("\n\n[output truncated to %d bytes]", len(content)),
	}
}
