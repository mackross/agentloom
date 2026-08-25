package programtest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractSetupSourceIncludesReferencedTypes(t *testing.T) {
	source := `package fixture

type Input struct {
	Value string
}

type Output struct {
	Input Input
}

type Unused struct {
	Value string
}

func TestFixture(t *testing.T) {
	Golden(t, "fixture.golden", func() (Output, error) {
		in := Input{Value: "hello"}
		out := Output{Input: in}
		return out, nil
	}, func(Output) string {
		return ""
	})
}
`
	path := filepath.Join(t.TempDir(), "fixture_test.go")
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	got, err := extractSetupSource(path, 16)
	if err != nil {
		t.Fatalf("extractSetupSource: %v", err)
	}
	for _, want := range []string{"type Input struct", "type Output struct", `in := Input{Value: "hello"}`} {
		if !strings.Contains(got, want) {
			t.Errorf("setup missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "type Unused") {
		t.Fatalf("setup includes unused type:\n%s", got)
	}
}
