package htmltomdwasm

import (
	"context"
	"strings"
	"testing"
)

func TestHtmlToMdSkipsImagesAndSVG(t *testing.T) {
	const html = `<article>
<h1>Images</h1>
<p>before</p>
<img alt="pixel" src="data:image/png;base64,AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA">
<svg width="10" height="10"><path d="M0 0h10v10z"></path></svg>
<p>after</p>
</article>`
	out, err := HtmlToMd(context.Background(), html)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"data:image", "base64", "AAAAAAAA", "<svg", "<path"} {
		if strings.Contains(out, bad) {
			t.Fatalf("output contains %q:\n%s", bad, out)
		}
	}
	for _, want := range []string{"# Images", "before", "after"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}
