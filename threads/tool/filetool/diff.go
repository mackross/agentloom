package filetool

import "strings"

const maxDiffMatrixCells = 4_000_000

func unifiedContext(oldName, newName, oldText, newText string, contextLines int) string {
	oldLines := diffSplitLines(normalizeNewlines(oldText))
	newLines := diffSplitLines(normalizeNewlines(newText))
	var b strings.Builder
	b.WriteString("--- ")
	b.WriteString(oldName)
	b.WriteByte('\n')
	b.WriteString("+++ ")
	b.WriteString(newName)
	b.WriteByte('\n')
	b.WriteString("@@\n")
	if len(oldLines)*len(newLines) > maxDiffMatrixCells {
		writeAllDiffLines(&b, '-', oldLines)
		writeAllDiffLines(&b, '+', newLines)
		return strings.TrimRight(b.String(), "\n")
	}
	writeLCSDiff(&b, oldLines, newLines, contextLines)
	return strings.TrimRight(b.String(), "\n")
}

func diffSplitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

func writeAllDiffLines(b *strings.Builder, prefix byte, lines []string) {
	for _, line := range lines {
		b.WriteByte(prefix)
		b.WriteString(line)
		b.WriteByte('\n')
	}
}

type diffLine struct {
	prefix byte
	text   string
}

func writeLCSDiff(b *strings.Builder, oldLines, newLines []string, contextLines int) {
	n, m := len(oldLines), len(newLines)
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if oldLines[i] == newLines[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	lines := make([]diffLine, 0, n+m)
	for i, j := 0, 0; i < n || j < m; {
		switch {
		case i < n && j < m && oldLines[i] == newLines[j]:
			lines = append(lines, diffLine{prefix: ' ', text: oldLines[i]})
			i++
			j++
		case i < n && j < m && dp[i][j+1] > dp[i+1][j]:
			lines = append(lines, diffLine{prefix: '+', text: newLines[j]})
			j++
		case i < n:
			lines = append(lines, diffLine{prefix: '-', text: oldLines[i]})
			i++
		case j < m:
			lines = append(lines, diffLine{prefix: '+', text: newLines[j]})
			j++
		}
	}
	writeDiffLines(b, lines, contextLines)
}

func writeDiffLines(b *strings.Builder, lines []diffLine, contextLines int) {
	if contextLines < 0 {
		for _, line := range lines {
			writeDiffLine(b, line)
		}
		return
	}
	include := make([]bool, len(lines))
	for i, line := range lines {
		if line.prefix == ' ' {
			continue
		}
		start := i - contextLines
		if start < 0 {
			start = 0
		}
		end := i + contextLines + 1
		if end > len(lines) {
			end = len(lines)
		}
		for j := start; j < end; j++ {
			include[j] = true
		}
	}
	omitted := false
	for i, line := range lines {
		if !include[i] {
			omitted = true
			continue
		}
		if omitted {
			b.WriteString("...\n")
			omitted = false
		}
		writeDiffLine(b, line)
	}
}

func writeDiffLine(b *strings.Builder, line diffLine) {
	b.WriteByte(line.prefix)
	b.WriteString(line.text)
	b.WriteByte('\n')
}
