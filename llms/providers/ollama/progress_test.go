package ollama

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/mackross/agentloom/llms"
	"github.com/mackross/agentloom/threads"
)

// Virtual-time transport exercises the complete request/decoder path without
// network scheduling or real multi-minute sleeps.
type progressTestTransport func(*http.Request) (*http.Response, error)

func (f progressTestTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type progressReadStep struct {
	delay time.Duration
	data  string
}

type progressTestBody struct {
	ctx   context.Context
	steps []progressReadStep
	stall bool
}

func (b *progressTestBody) Read(p []byte) (int, error) {
	if len(b.steps) == 0 {
		if b.stall {
			<-b.ctx.Done()
			return 0, b.ctx.Err()
		}
		return 0, io.EOF
	}
	step := &b.steps[0]
	if step.delay > 0 {
		select {
		case <-b.ctx.Done():
			return 0, b.ctx.Err()
		case <-time.After(step.delay):
		}
		step.delay = 0
	}
	n := copy(p, step.data)
	step.data = step.data[n:]
	if step.data == "" {
		b.steps = b.steps[1:]
	}
	return n, nil
}

func (*progressTestBody) Close() error { return nil }

func progressTestClient(headerDelay time.Duration, stall bool, steps ...progressReadStep) *http.Client {
	return &http.Client{Transport: progressTestTransport(func(r *http.Request) (*http.Response, error) {
		if headerDelay > 0 {
			select {
			case <-r.Context().Done():
				return nil, r.Context().Err()
			case <-time.After(headerDelay):
			}
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       &progressTestBody{ctx: r.Context(), steps: append([]progressReadStep(nil), steps...), stall: stall},
			Request:    r,
		}, nil
	})}
}

func TestStreamProgressResetsOnResponseBytes(t *testing.T) {
	for _, field := range []string{"content", "thinking", "tool_calls"} {
		t.Run(field, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var steps []progressReadStep
				for range 6 {
					chunk := `{"message":{"` + field + `":"x"},"done":false}` + "\n"
					if field == "tool_calls" {
						chunk = `{"message":{"tool_calls":[{"id":"call_1","function":{"name":"read","arguments":{}}}]},"done":false}` + "\n"
					}
					// Each complete JSON object takes longer than the timeout;
					// partial JSON reads must count as progress too.
					half := len(chunk) / 2
					steps = append(steps,
						progressReadStep{time.Minute, chunk[:half]},
						progressReadStep{time.Minute, chunk[half:]},
					)
				}
				steps = append(steps, progressReadStep{time.Minute, `{"done":true}`})
				client := progressTestClient(0, false, steps...)
				s := NewChatStreamerWithClient(client, mustURL(t, "http://ollama.test"), "test")
				timeout := 90 * time.Second
				s.StreamProgressTimeout = &timeout
				start := time.Now()
				items, err := collectOllamaItems(s)
				if err != nil {
					t.Fatalf("active %s stream failed: %v", field, err)
				}
				if len(items) == 0 || time.Since(start) <= timeout {
					t.Fatalf("expected successful long stream with items: %v", items)
				}
			})
		})
	}
}

func TestStreamProgressTimeoutAfterLastRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := progressTestClient(0, true,
			progressReadStep{time.Minute, `{"message":{"thinking":"first"}}`},
			progressReadStep{time.Minute, `{"message":{"thinking":"second"}}`},
			progressReadStep{time.Minute, `{"message":{"thinking":"third"}}`},
		)
		s := NewChatStreamerWithClient(client, mustURL(t, "http://ollama.test"), "test")
		timeout := 2 * time.Minute
		s.StreamProgressTimeout = &timeout
		start := time.Now()
		_, err := collectOllamaItems(s)
		if !errors.Is(err, ErrStreamProgressTimeout) || errors.Is(err, context.Canceled) {
			t.Fatalf("inactivity error = %v", err)
		}
		if elapsed := time.Since(start); elapsed != 5*time.Minute {
			t.Fatalf("timed out after %v, want 3m of progress + 2m of silence", elapsed)
		}
	})
}

func TestStreamProgressBoundsInitialSilence(t *testing.T) {
	for _, headers := range []bool{false, true} {
		name := "body"
		if headers {
			name = "headers"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				delay := time.Duration(0)
				if headers {
					delay = time.Hour
				}
				s := NewChatStreamerWithClient(progressTestClient(delay, true), mustURL(t, "http://ollama.test"), "test")
				start := time.Now()
				_, err := collectOllamaItems(s)
				if !errors.Is(err, ErrStreamProgressTimeout) {
					t.Fatalf("initial silence error = %v", err)
				}
				if time.Since(start) != DefaultStreamProgressTimeout {
					t.Fatalf("initial silence lasted %v", time.Since(start))
				}
			})
		})
	}
}

func TestModelStreamProgressTimeout(t *testing.T) {
	for _, configured := range []string{"30m", "0s"} {
		t.Run(configured, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client := progressTestClient(10*time.Minute, false,
					progressReadStep{10 * time.Minute, `{"message":{"content":"OK"},"done":true}`},
				)
				s, err := Provider.Open(context.Background(), llms.Model{
					Name: "slow-model", ID: "test",
					Options: map[string]any{"host": "http://ollama.test", "stream_progress_timeout": configured},
				}, llms.Options{HTTPClient: client})
				if err != nil {
					t.Fatal(err)
				}
				var text string
				err = s.StreamReq(threads.Req{}, func(item threads.Item) error {
					if chunk, ok := item.(threads.AssistantText); ok {
						text += string(chunk)
					}
					return nil
				})
				if err != nil || text != "OK" {
					t.Fatalf("configured %s: text=%q err=%v", configured, text, err)
				}
			})
		})
	}
}

func TestStreamProgressPreservesParentDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		s := NewChatStreamerWithClient(progressTestClient(0, true), mustURL(t, "http://ollama.test"), "test")
		err := s.StreamReqContext(ctx, threads.Req{}, func(threads.Item) error { return nil })
		if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrStreamProgressTimeout) {
			t.Fatalf("parent deadline error = %v", err)
		}
	})
}

func TestStreamProgressResetsAfterHeaders(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := progressTestClient(4*time.Minute, false,
			progressReadStep{4 * time.Minute, `{"message":{"content":"OK"},"done":true}`},
		)
		s := NewChatStreamerWithClient(client, mustURL(t, "http://ollama.test"), "test")
		if _, err := collectOllamaItems(s); err != nil {
			t.Fatalf("response headers did not reset inactivity timeout: %v", err)
		}
	})
}

func TestStreamProgressWatchdogStaleCallbackAndCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, w := newStreamProgressWatchdog(context.Background(), time.Minute)
		defer w.stop()
		time.Sleep(45 * time.Second)
		w.progress()
		time.Sleep(20 * time.Second)
		w.expire() // Simulate an old callback that started before the reset.
		if ctx.Err() != nil {
			t.Fatal("stale callback canceled active stream")
		}
		w.stop()
		w.progress() // Must not rearm a stopped watchdog.
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		if !errors.Is(context.Cause(ctx), context.Canceled) {
			t.Fatalf("cleanup cause = %v", context.Cause(ctx))
		}
	})
}

func TestOpenRejectsInvalidProgressTimeout(t *testing.T) {
	for _, raw := range []any{"", "30", "forever", "-1s", "999999999999999999999h", 30, true} {
		_, err := Provider.Open(context.Background(), llms.Model{
			Name: "bad-timeout", ID: "test",
			Options: map[string]any{"host": "http://ollama.test", "stream_progress_timeout": raw},
		}, llms.Options{})
		if err == nil || !strings.Contains(err.Error(), "bad-timeout: stream_progress_timeout:") {
			t.Errorf("timeout %v: expected named configuration error, got %v", raw, err)
		}
	}
}
