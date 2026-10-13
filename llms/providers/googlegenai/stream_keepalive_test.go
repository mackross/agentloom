package googlegenai

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"google.golang.org/genai"
)

func TestGenerateContentStreamerKeepaliveClientPreservesConfig(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("constructor made an HTTP request") }))
	defer server.Close()
	httpClient := server.Client()
	httpClient.Timeout = 3 * time.Second
	transport := httpClient.Transport
	original, err := genai.NewClient(t.Context(), &genai.ClientConfig{
		APIKey: "test", Backend: genai.BackendGeminiAPI, HTTPClient: httpClient,
		HTTPOptions: genai.HTTPOptions{BaseURL: server.URL, APIVersion: "v1beta", Headers: http.Header{"X-Test": []string{"preserved"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	streamer := NewGenerateContentStreamerWithClient(original, "gemini-3.1-flash-lite")
	if streamer.clientErr != nil {
		t.Fatal(streamer.clientErr)
	}
	got, before := streamer.client.ClientConfig(), original.ClientConfig()
	if streamer.client == original || got.HTTPClient == httpClient || httpClient.Transport != transport || before.HTTPClient.Transport != transport {
		t.Fatal("constructor mutated the supplied client or did not clone it")
	}
	if got.APIKey != before.APIKey || got.Backend != before.Backend || got.HTTPOptions.BaseURL != before.HTTPOptions.BaseURL || got.HTTPOptions.APIVersion != before.HTTPOptions.APIVersion || got.HTTPOptions.Headers.Get("X-Test") != "preserved" || got.HTTPClient.Timeout != httpClient.Timeout {
		t.Fatal("constructor changed SDK or HTTP client configuration")
	}
}
