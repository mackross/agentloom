//go:build live

package ollama

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/streamerlivetest"
)

// Ollama live tests differ from hosted-provider live tests: they are skipped
// unless a local endpoint is reachable and an explicitly configured model is
// already installed. They never pull a model.
func TestLiveCapabilities(t *testing.T) {
	streamer := requireLiveOllama(t)
	streamerlivetest.Run(t, ollamaLiveHarness{streamer: streamer})
}

func requireLiveOllama(t testing.TB) *ChatStreamer {
	t.Helper()
	model := strings.TrimSpace(os.Getenv("OLLAMA_MODEL"))
	if model == "" {
		t.Skip("OLLAMA_MODEL is not set; skipping local Ollama live test")
	}
	base, err := baseURLFromEnvironment()
	if err != nil {
		t.Skipf("OLLAMA_HOST is invalid; skipping local Ollama live test: %v", err)
	}

	client := &http.Client{Timeout: 3 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := probeOllamaModel(ctx, client, base, model); err != nil {
		t.Skipf("local Ollama model %q is unavailable at %s: %v", model, base, err)
	}
	return NewChatStreamerWithClient(http.DefaultClient, base, model)
}

func probeOllamaModel(ctx context.Context, client *http.Client, base *url.URL, model string) error {
	showURL := cloneURL(base)
	path := strings.TrimRight(showURL.Path, "/")
	if strings.HasSuffix(path, "/api") {
		showURL.Path = path + "/show"
	} else {
		showURL.Path = path + "/api/show"
	}
	body, err := json.Marshal(map[string]any{"model": model})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, showURL.String(), strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("connection failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("%s", resp.Status)
	}
	return nil
}

type ollamaLiveHarness struct {
	streamerlivetest.SupportsAssistantTextChunking
	streamerlivetest.SupportsParallelToolCalls
	streamerlivetest.SupportsAllowedTools
	streamer *ChatStreamer
}

func (h ollamaLiveHarness) Stream(t testing.TB, req threads.Req, emit func(threads.Item) error) error {
	t.Helper()
	h.streamer.AllowBestEffortToolControls = true
	return h.streamer.StreamReq(req, emit)
}
