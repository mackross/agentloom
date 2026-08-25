package htmltomdwasm

import (
	"bytes"
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var updateGolden = flag.Bool("update", false, "update htmltomdwasm golden files")

func TestHtmlToMd(t *testing.T) {
	out, err := HtmlToMd(context.Background(), "<h1>Hello</h1><p>World</p>")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "# Hello") || !strings.Contains(out, "World") {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestHtmlToMdStripMetaFrontMatterKeepsTitle(t *testing.T) {
	const html = `<html><head><title>Hello</title><meta name="description" content="Greeting page"></head><body><main><p>World</p></main></body></html>`
	out, err := HtmlToMdWithOptions(context.Background(), html, Options{StripMetaFrontMatter: true})
	if err != nil {
		t.Fatal(err)
	}
	out = strings.TrimSpace(out)
	if strings.Contains(out, "meta-description:") || strings.Contains(out, "title:") || strings.Contains(out, "---") {
		t.Fatalf("metadata front matter was not stripped:\n%s", out)
	}
	if !strings.HasPrefix(out, "# Hello") {
		t.Fatalf("expected content to start with heading, got:\n%s", out)
	}
}

func TestHtmlToMdGoldenRealPages(t *testing.T) {
	for _, name := range []string{"github", "pkggo", "wikipedia"} {
		name := name
		t.Run(name, func(t *testing.T) {
			htmlPath := filepath.Join("testdata", "real", name+".html")
			goldenPath := filepath.Join("testdata", "real", name+".golden.md")

			html, err := os.ReadFile(htmlPath)
			if err != nil {
				t.Fatal(err)
			}
			got, err := HtmlToMd(context.Background(), string(html))
			if err != nil {
				t.Fatal(err)
			}
			got = normalizeMarkdown(got)

			if *updateGolden {
				if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}

			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("read golden file: %v; run go test ./internal/htmltomdwasm -update", err)
			}
			if !bytes.Equal([]byte(got), want) {
				t.Fatalf("golden mismatch for %s; run go test ./internal/htmltomdwasm -update", name)
			}
		})
	}
}

func BenchmarkHtmlToMdRealPages(b *testing.B) {
	for _, name := range []string{"github", "pkggo", "wikipedia"} {
		html, err := os.ReadFile(filepath.Join("testdata", "real", name+".html"))
		if err != nil {
			b.Fatal(err)
		}
		b.Run(name, func(b *testing.B) {
			ctx := context.Background()
			b.ReportAllocs()
			b.SetBytes(int64(len(html)))
			for i := 0; i < b.N; i++ {
				out, err := HtmlToMd(ctx, string(html))
				if err != nil {
					b.Fatal(err)
				}
				if len(out) == 0 {
					b.Fatal("empty output")
				}
			}
		})
	}
}

func normalizeMarkdown(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.TrimSpace(s)
	return s + "\n"
}
