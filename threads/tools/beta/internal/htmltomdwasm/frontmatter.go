package htmltomdwasm

import "strings"

func stripMetaFrontMatter(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	if len(lines) == 0 {
		return s
	}
	if strings.TrimSpace(lines[0]) == "---" {
		for i := 1; i < len(lines); i++ {
			if strings.TrimSpace(lines[i]) == "---" {
				bodyStart := i + 1
				for bodyStart < len(lines) && strings.TrimSpace(lines[bodyStart]) == "" {
					bodyStart++
				}
				return joinTitleAndBody(extractMetaTitle(lines[1:i]), lines[bodyStart:])
			}
		}
		return s
	}

	i := 0
	for i < len(lines) {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			i++
			continue
		}
		if !looksLikeMetaLine(line) {
			break
		}
		i++
	}
	if i == 0 {
		return s
	}
	title := extractMetaTitle(lines[:i])
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	return joinTitleAndBody(title, lines[i:])
}

func joinTitleAndBody(title string, body []string) string {
	bodyText := strings.Join(body, "\n")
	if title == "" || bodyHasTitle(body, title) {
		return bodyText
	}
	if strings.TrimSpace(bodyText) == "" {
		return "# " + title
	}
	return "# " + title + "\n\n" + bodyText
}

func bodyHasTitle(body []string, title string) bool {
	want := strings.TrimSpace(title)
	for _, line := range body {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		return strings.TrimSpace(strings.TrimPrefix(line, "#")) == want
	}
	return false
}

func extractMetaTitle(lines []string) string {
	for _, line := range lines {
		key, value, ok := splitMetaLine(line)
		if ok && strings.EqualFold(key, "title") {
			return strings.Trim(strings.TrimSpace(value), `"'`)
		}
	}
	return ""
}

func looksLikeMetaLine(line string) bool {
	_, _, ok := splitMetaLine(line)
	return ok
}

func splitMetaLine(line string) (string, string, bool) {
	line = strings.TrimSpace(line)
	idx := strings.IndexByte(line, ':')
	if idx <= 0 {
		return "", "", false
	}
	key := line[:idx]
	for _, r := range key {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		return "", "", false
	}
	return key, line[idx+1:], true
}
