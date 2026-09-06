package harness

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mackross/agentloom/llms"
)

func TestFilesLoadLayersAndSaves(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("models.toml", `
default = "sol"
effort = "high"

[providers.openai]
model = "gpt-5.6-sol"

[[models]]
provider = "openai"
name = "gpt-5.6-sol"
aliases = ["sol"]
label = "flagship"
fast = true
efforts = ["low", "high"]

[[models]]
provider = "ollama"
name = "qwen"
id = "qwen3.5:9b-mlx"
host = "http://terminus.local:11434"
think = "high"
prefer_apply_patch = true
[models.options]
num_ctx = 65536
`)
	write("models.weaver.toml", `
default = "qwen"
fast = true
[providers.ollama]
model = "qwen"
`)
	write("auth.toml", `
[openai]
api_key = "sk-1"
[openai.subscription]
access_token = "a1"
refresh_token = "r1"
expires_at = 2030-01-02T03:04:05Z
[brave]
api_key = "b1"
`)
	store := Files(dir, "weaver")
	s, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.Default != "qwen" || s.Effort != llms.EffortHigh || !s.Fast {
		t.Fatalf("scalars: %+v", s)
	}
	if s.Providers["openai"].Model != "gpt-5.6-sol" || s.Providers["ollama"].Model != "qwen" {
		t.Fatalf("providers: %+v", s.Providers)
	}
	if len(s.Models) != 2 {
		t.Fatalf("models: %+v", s.Models)
	}
	sol, qwen := s.Models[0], s.Models[1]
	if sol.Provider != "openai" || sol.Aliases[0] != "sol" || !sol.Fast || len(sol.Efforts) != 2 || sol.Efforts[1] != llms.EffortHigh {
		t.Fatalf("sol: %+v", sol)
	}
	if qwen.ID != "qwen3.5:9b-mlx" || qwen.Options["host"] != "http://terminus.local:11434" || qwen.Options["think"] != "high" || qwen.Options["prefer_apply_patch"] != true {
		t.Fatalf("qwen: %+v", qwen)
	}
	if opts, ok := qwen.Options["options"].(map[string]any); !ok || opts["num_ctx"] != int64(65536) {
		t.Fatalf("qwen options: %+v", qwen.Options["options"])
	}
	if s.Auth["openai"].APIKey != "sk-1" || s.Auth["openai"].Token == nil || s.Auth["openai"].Token.Refresh != "r1" || !s.Auth["openai"].Token.Expiry.Equal(time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Fatalf("auth: %+v", s.Auth["openai"])
	}
	if s.Auth["brave"].APIKey != "b1" {
		t.Fatalf("brave: %+v", s.Auth["brave"])
	}

	err = store.Save(context.Background(), func(s *Settings) error {
		s.Default = "sol"
		s.Effort = llms.EffortDefault
		s.setProviderModel("openai", "gpt-5.6-luna")
		s.setAuth("openai", func(e *AuthEntry) { e.Token = nil })
		s.setAuth("xai", func(e *AuthEntry) {
			e.Token = &llms.Token{Access: "x1", Expiry: time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)}
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	app, _ := os.ReadFile(filepath.Join(dir, "models.weaver.toml"))
	shared, _ := os.ReadFile(filepath.Join(dir, "models.toml"))
	auth, _ := os.ReadFile(filepath.Join(dir, "auth.toml"))
	if !strings.Contains(string(app), `default = 'sol'`) || !strings.Contains(string(app), `model = 'gpt-5.6-luna'`) {
		t.Fatalf("app file:\n%s", app)
	}
	if strings.Contains(string(app), "effort") {
		t.Fatalf("default effort should be deleted, not written:\n%s", app)
	}
	if !strings.Contains(string(shared), `effort = "high"`) || strings.Contains(string(shared), "luna") {
		t.Fatalf("shared file must be untouched:\n%s", shared)
	}
	if strings.Contains(string(auth), "access_token = 'a1'") || !strings.Contains(string(auth), "api_key = 'sk-1'") || !strings.Contains(string(auth), "api_key = 'b1'") || !strings.Contains(string(auth), "access_token = 'x1'") {
		t.Fatalf("auth file:\n%s", auth)
	}
	s, _ = store.Load(context.Background())
	if s.Default != "sol" || s.Effort != llms.EffortHigh || s.Auth["openai"].Token != nil || s.Auth["xai"].Token.Access != "x1" {
		t.Fatalf("reload: %+v", s)
	}
}

func TestFilesMissingDirIsEmpty(t *testing.T) {
	store := Files(filepath.Join(t.TempDir(), "missing"), "")
	s, err := store.Load(context.Background())
	if err != nil || s.Default != "" || len(s.Models) != 0 {
		t.Fatalf("%+v %v", s, err)
	}
	if err := store.Save(context.Background(), func(s *Settings) error { s.Default = "x"; return nil }); err != nil {
		t.Fatal(err)
	}
	s, _ = store.Load(context.Background())
	if s.Default != "x" {
		t.Fatalf("%+v", s)
	}
}

func TestDefaultDir(t *testing.T) {
	t.Setenv("AGENTS_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg")
	if d, _ := DefaultDir(); d != "/tmp/xdg/agents" {
		t.Fatal(d)
	}
	t.Setenv("AGENTS_CONFIG_DIR", "/explicit")
	if d, _ := DefaultDir(); d != "/explicit" {
		t.Fatal(d)
	}
}
