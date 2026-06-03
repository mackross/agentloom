package filetool

import "fmt"

func applyConfiguredTruncation(text string, maxLines int, startLine int) string {
	if maxLines <= 0 {
		return text
	}
	limited, visible, truncated := firstLines(text, maxLines)
	if !truncated {
		return text
	}
	return limited + truncationNotice(visible, countLines(text), startLine)
}

func truncationNotice(visibleLines, totalLines, startLine int) string {
	return fmt.Sprintf("\n[Output truncated after %d of %d lines. Call read again with offset %d to continue.]",
		visibleLines, totalLines, startLine+visibleLines)
}

func firstLines(s string, max int) (string, int, bool) {
	if max <= 0 {
		return s, countLines(s), false
	}
	if s == "" {
		return s, 0, false
	}
	lines := 0
	for i, r := range s {
		if r != '\n' {
			continue
		}
		lines++
		if lines == max {
			return s[:i+1], lines, i+1 < len(s)
		}
	}
	if lines == 0 || s[len(s)-1] != '\n' {
		lines++
	}
	return s, lines, false
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	lines := 0
	for _, r := range s {
		if r == '\n' {
			lines++
		}
	}
	if s[len(s)-1] != '\n' {
		lines++
	}
	return lines
}

