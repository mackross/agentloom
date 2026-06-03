package filetool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTextGoldenFixturesEndWithNewline(t *testing.T) {
	roots := []string{
		filepath.Join("testdata", "read", "golden"),
		filepath.Join("testdata", "applypatch", "golden"),
		filepath.Join("testdata", "edit", "golden"),
		filepath.Join("testdata", "postprocess", "golang", "golden"),
	}
	for _, root := range roots {
		root := root
		t.Run(strings.ReplaceAll(root, string(filepath.Separator), "/"), func(t *testing.T) {
			entries, err := os.ReadDir(root)
			if err != nil {
				t.Fatalf("read golden dir %s: %v", root, err)
			}
			for _, entry := range entries {
				if entry.IsDir() || filepath.Ext(entry.Name()) != ".txt" {
					continue
				}
				path := filepath.Join(root, entry.Name())
				buf, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read golden %s: %v", path, err)
				}
				trailingNewlines := 0
				for i := len(buf) - 1; i >= 0 && buf[i] == '\n'; i-- {
					trailingNewlines++
				}
				if trailingNewlines != 1 {
					t.Fatalf("text golden %s must end with exactly one fixture newline, got %d; assertion helpers remove one newline before comparing non-diff tool output", path, trailingNewlines)
				}
			}
		})
	}
}
