package streamerutil

import (
	"bufio"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
	"time"
)

func TestSSEDataFramesPreservesDataAndFraming(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"comments", ": keep-alive\n\n\n", ""},
		{"metadata_only", "event: ping\r\nid: 1\r\n\r\n", ""},
		{"data", ": ping\n\nevent: response\r\ndata: {}\r\n\r\n", "event: response\r\ndata: {}\r\n\r\n"},
		{"comment_with_data", ": ping\ndata: {}\n\n", ": ping\ndata: {}\n\n"},
		{"empty_data", "data: \n\n", "data: \n\n"},
		{"bare_data", "data\n\n", "data\n\n"},
		{"invalid_json", "data: {\n\n", "data: {\n\n"},
		{"multi_line", "event: response\ndata: {\ndata: }\n\n", "event: response\ndata: {\ndata: }\n\n"},
		{"unterminated", "data: {", "data: {"},
		{"long_line", "data: " + strings.Repeat("x", 128*1024) + "\n\n", "data: " + strings.Repeat("x", 128*1024) + "\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := io.NopCloser(strings.NewReader(tc.input))
			reader := &sseDataFrames{source: source, reader: bufio.NewReader(source)}
			if n, err := reader.Read(nil); n != 0 || err != nil {
				t.Fatalf("zero-byte Read = %d, %v", n, err)
			}
			got, err := io.ReadAll(reader)
			if err != nil || string(got) != tc.want {
				t.Fatalf("filtered SSE = %q, err=%v, want %q", got, err, tc.want)
			}
		})
	}
}

type keepaliveTestBody struct {
	io.Reader
	closed bool
	err    error
}

func (b *keepaliveTestBody) Close() error { b.closed = true; return b.err }

type keepaliveTestErrorReader struct{ err error }

func (r keepaliveTestErrorReader) Read([]byte) (int, error) { return 0, r.err }

func TestSSEDataFramesPreservesSourceErrorsAndClose(t *testing.T) {
	want := errors.New("source interrupted")
	for _, prefix := range []string{": ping\n\n", "data: {}\n\n", "data: {"} {
		source := &keepaliveTestBody{Reader: io.MultiReader(strings.NewReader(prefix), keepaliveTestErrorReader{want}), err: want}
		reader := &sseDataFrames{source: source, reader: bufio.NewReader(source)}
		var got strings.Builder
		p := make([]byte, 1)
		for {
			n, err := reader.Read(p)
			got.Write(p[:n])
			if err != nil {
				if !errors.Is(err, want) {
					t.Fatalf("source error = %v, want %v", err, want)
				}
				break
			}
		}
		expected := prefix
		if strings.HasPrefix(prefix, ":") {
			expected = ""
		}
		if got.String() != expected {
			t.Fatalf("data before error = %q, want %q", got.String(), expected)
		}
		if err := reader.Close(); !errors.Is(err, want) || !source.closed {
			t.Fatalf("source Close not preserved: closed=%v err=%v", source.closed, err)
		}
	}
}

func TestSSEKeepaliveMiddlewareOnlyWrapsSuccessfulSSE(t *testing.T) {
	for _, tc := range []struct {
		name, contentType string
		status            int
		wrap              bool
	}{
		{"sse", "text/event-stream; charset=utf-8", 200, true},
		{"json", "application/json", 200, false},
		{"missing", "", 200, false},
		{"invalid", "text/event-stream; broken", 200, false},
		{"failure", "text/event-stream", 500, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := &keepaliveTestBody{Reader: strings.NewReader(": ping\n\n")}
			resp := &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: source}
			resp.Header.Set("Content-Type", tc.contentType)
			got, err := SSEKeepaliveMiddleware(nil, func(*http.Request) (*http.Response, error) { return resp, nil })
			if err != nil || got != resp || (got.Body != source) != tc.wrap {
				t.Fatalf("response=%v err=%v wrapped=%v, want %v", got, err, got.Body != source, tc.wrap)
			}
			body := got.Body
			filterSSEKeepaliveResponse(got)
			if got.Body != body {
				t.Fatal("response body wrapped twice")
			}
			_ = got.Body.Close()
			if !source.closed {
				t.Fatal("middleware did not close source")
			}
		})
	}
	want := errors.New("transport failed")
	got, err := SSEKeepaliveMiddleware(nil, func(*http.Request) (*http.Response, error) { return nil, want })
	if got != nil || !errors.Is(err, want) {
		t.Fatalf("transport failure = %v, %v", got, err)
	}
	for _, resp := range []*http.Response{nil, {StatusCode: 200, Header: make(http.Header)}} {
		got, err := SSEKeepaliveMiddleware(nil, func(*http.Request) (*http.Response, error) { return resp, nil })
		if got != resp || err != nil {
			t.Fatalf("nil response/body changed: %v, %v", got, err)
		}
	}
}

type keepaliveTestTransport struct {
	closed bool
	fail   error
	source *keepaliveTestBody
}

func (t *keepaliveTestTransport) RoundTrip(*http.Request) (*http.Response, error) {
	if t.fail != nil {
		return nil, t.fail
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: t.source}, nil
}
func (t *keepaliveTestTransport) CloseIdleConnections() { t.closed = true }

func TestSSEKeepaliveHTTPClientPreservesConfigurationAndCleanup(t *testing.T) {
	source := &keepaliveTestBody{Reader: strings.NewReader(": ping\n\ndata: {}\n\n")}
	transport := &keepaliveTestTransport{source: source}
	jar, _ := cookiejar.New(nil)
	redirectErr := errors.New("no redirect")
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second, Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return redirectErr }}
	filtered := SSEKeepaliveHTTPClient(client)
	if filtered == client || client.Transport != transport || filtered.Timeout != client.Timeout || filtered.Jar != jar || !errors.Is(filtered.CheckRedirect(nil, nil), redirectErr) {
		t.Fatal("client clone lost configuration or mutated source")
	}
	twice := SSEKeepaliveHTTPClient(filtered)
	if twice.Transport != filtered.Transport {
		t.Fatal("transport wrapped twice")
	}
	req, _ := http.NewRequest("GET", "http://stream.test", nil)
	resp, err := filtered.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil || string(got) != "data: {}\n\n" {
		t.Fatalf("transport SSE = %q, %v", got, err)
	}
	_ = resp.Body.Close()
	filtered.CloseIdleConnections()
	if !source.closed || !transport.closed {
		t.Fatal("transport did not forward cleanup")
	}
	transport.fail = errors.New("transport interrupted")
	_, err = filtered.Do(req)
	if !errors.Is(err, transport.fail) {
		t.Fatalf("transport error hidden: %v", err)
	}
	if SSEKeepaliveHTTPClient(nil).Transport == nil {
		t.Fatal("nil client did not use default transport")
	}
}
