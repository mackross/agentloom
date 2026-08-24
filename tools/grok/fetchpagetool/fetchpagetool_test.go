package fetchpagetool

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/mackross/agentloom/threads"
)

func TestSnapshotExposesOnlyWebFetchURL(t *testing.T) {
	snapshot := New(Config{}).ToolsSnapshot(nil)
	if len(snapshot.Snapshot.Offered) != 1 || snapshot.Snapshot.Offered[0].Name != Name {
		t.Fatalf("offered tools = %#v", snapshot.Snapshot.Offered)
	}
	if strings.Contains(mustJSON(t, snapshot.Snapshot.Offered[0].Payload), "maxChars") {
		t.Fatal("model-facing schema exposes maxChars")
	}
	schema := mustJSON(t, snapshot.Snapshot.Offered[0].Payload)
	for _, want := range []string{`"url"`, `"required":["url"]`, `"additionalProperties":false`} {
		if !strings.Contains(schema, want) {
			t.Fatalf("schema %s does not contain %s", schema, want)
		}
	}
	if len(snapshot.Snapshot.Allowed) != 1 || snapshot.Snapshot.Allowed[0] != Name ||
		len(snapshot.Handlers) != 1 || snapshot.Handlers[0].Name != Name {
		t.Fatalf("snapshot still routes canonical name: %#v", snapshot)
	}
}

func TestResolveInjectsConfiguredLimitAndDelegates(t *testing.T) {
	oldClient := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"text/plain"}},
			Body:       ioNopCloser{strings.NewReader(strings.Repeat("word ", 1000))},
			Request:    req,
		}, nil
	})}
	t.Cleanup(func() { http.DefaultClient = oldClient })

	tool := New(Config{MaxChars: 1_000})
	snapshot := tool.ToolsSnapshot(nil)
	dispatch, err := tool.ResolveTool(context.Background(), nil, threads.ToolCall{
		CallID:  "call-1",
		Name:    Name,
		Payload: `{"url":"https://example.com/page"}`,
	}, snapshot.Handlers[0].HandlerLoadData)
	if err != nil {
		t.Fatal(err)
	}
	result, ok := dispatch.Items[0].(threads.ToolCallResult)
	if !ok {
		t.Fatalf("result type = %T", dispatch.Items[0])
	}
	if !strings.Contains(result.Output, "content truncated") {
		t.Fatalf("configured character limit was not applied: %q", result.Output)
	}
}

func TestResolveRetainsCanonicalPublicURLChecks(t *testing.T) {
	tool := New(Config{})
	snapshot := tool.ToolsSnapshot(nil)
	dispatch, err := tool.ResolveTool(context.Background(), nil, threads.ToolCall{
		CallID:  "call-1",
		Name:    Name,
		Payload: `{"url":"http://127.0.0.1/private"}`,
	}, snapshot.Handlers[0].HandlerLoadData)
	if err != nil {
		t.Fatal(err)
	}
	result := dispatch.Items[0].(threads.ToolCallResult)
	if !strings.Contains(result.Output, "private/internal addresses") {
		t.Fatalf("unexpected result: %q", result.Output)
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type ioNopCloser struct{ *strings.Reader }

func (ioNopCloser) Close() error { return nil }
