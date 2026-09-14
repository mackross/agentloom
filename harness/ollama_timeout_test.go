package harness_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mackross/agentloom/harness"
	"github.com/mackross/agentloom/llms"
	"github.com/mackross/agentloom/llms/providers/ollama"
)

func TestOllamaTimeoutFromModelFile(t *testing.T) {
	dir := t.TempDir()
	const config = `
[[models]]
provider = "ollama"
name = "slow"
id = "glm-5.3-flash"
host = "http://ollama.test:11434"
stream_progress_timeout = "30m"
[models.options]
num_ctx = 65536

[[models]]
provider = "ollama"
name = "default"
id = "qwen3"
host = "http://ollama.test:11434"

[[models]]
provider = "ollama"
name = "disabled"
id = "qwen3"
host = "http://ollama.test:11434"
stream_progress_timeout = "0s"
`
	if err := os.WriteFile(filepath.Join(dir, "models.loom.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	settings, err := harness.Files(dir, "weaver").Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(settings.Models) != 3 {
		t.Fatalf("models = %d, want 3", len(settings.Models))
	}
	for _, model := range settings.Models {
		streamer, err := ollama.Provider.Open(context.Background(), model, llms.Options{})
		if err != nil {
			t.Fatal(err)
		}
		cs := streamer.(*ollama.ChatStreamer)
		switch model.Name {
		case "default":
			if cs.StreamProgressTimeout != nil {
				t.Fatalf("default model inherited another model's timeout: %v", cs.StreamProgressTimeout)
			}
		case "slow", "disabled":
			want := 30 * time.Minute
			if model.Name == "disabled" {
				want = 0
			}
			if cs.StreamProgressTimeout == nil || *cs.StreamProgressTimeout != want {
				t.Fatalf("%s timeout = %v, want %v", model.Name, cs.StreamProgressTimeout, want)
			}
		default:
			t.Fatalf("unexpected model %q", model.Name)
		}
		if _, sentToOllama := cs.Options["stream_progress_timeout"]; sentToOllama {
			t.Fatal("client timeout leaked into Ollama inference options")
		}
	}
}
