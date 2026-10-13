package streamerutil

import (
	"bufio"
	"bytes"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/mackross/openai-go/v3/option"
)

// SSEKeepaliveMiddleware filters data-less SSE frames before openai-go decodes
// them. Actual data, including malformed JSON and empty data fields, is kept.
func SSEKeepaliveMiddleware(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
	resp, err := next(req)
	if err == nil {
		filterSSEKeepaliveResponse(resp)
	}
	return resp, err
}

// SSEKeepaliveHTTPClient returns a shallow copy with a filtering transport.
// The caller's transport, timeout, cookie jar, and redirect policy are retained;
// neither the original client nor its transport is mutated.
func SSEKeepaliveHTTPClient(client *http.Client) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	clone := *client
	transport := clone.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	if _, filtered := transport.(sseKeepaliveTransport); !filtered {
		clone.Transport = sseKeepaliveTransport{transport}
	}
	return &clone
}

type sseKeepaliveTransport struct{ inner http.RoundTripper }

func (t sseKeepaliveTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.inner.RoundTrip(req)
	if err == nil {
		filterSSEKeepaliveResponse(resp)
	}
	return resp, err
}

func (t sseKeepaliveTransport) CloseIdleConnections() {
	if closer, ok := t.inner.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

func filterSSEKeepaliveResponse(resp *http.Response) {
	if resp == nil || resp.Body == nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return
	}
	if _, filtered := resp.Body.(*sseDataFrames); filtered {
		return
	}
	mediaType, _, parseErr := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if parseErr == nil && strings.EqualFold(mediaType, "text/event-stream") {
		resp.Body = &sseDataFrames{source: resp.Body, reader: bufio.NewReader(resp.Body)}
	}
}

type sseDataFrames struct {
	source  io.ReadCloser
	reader  *bufio.Reader
	pending []byte
	err     error
}

func (r *sseDataFrames) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(r.pending) == 0 && r.err == nil {
		var frame []byte
		hasData := false
		for {
			line, err := r.reader.ReadBytes('\n')
			frame = append(frame, line...)
			field := bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
			if bytes.HasPrefix(field, []byte("data:")) || bytes.Equal(field, []byte("data")) {
				hasData = true
			}
			if err != nil {
				r.err = err
				if hasData {
					r.pending = frame
				}
				break
			}
			if len(field) == 0 {
				if hasData {
					r.pending = frame
				}
				break
			}
		}
	}
	if len(r.pending) > 0 {
		n := copy(p, r.pending)
		r.pending = r.pending[n:]
		return n, nil
	}
	return 0, r.err
}

func (r *sseDataFrames) Close() error { return r.source.Close() }
