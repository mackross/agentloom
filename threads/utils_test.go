package threads

import (
	"sync"
	"testing"

	gschema "github.com/google/jsonschema-go/jsonschema"
)

type fakeStreamer struct {
	runtime  fakeStreamerRuntime
	replies  []streamReply
	calls    int
	requests []Req

	mu    sync.Mutex
	waits map[string]*waitGate
}

type fakeStreamerRuntime struct {
	owner *fakeStreamer
}

type waitGate struct {
	ch     chan struct{}
	closed bool
}

type streamReply struct {
	assertRequest func(Req)
	steps         []streamStep
}

type streamStepKind int

const (
	streamStepDo streamStepKind = iota
	streamStepEmit
	streamStepWait
)

type streamStep struct {
	kind streamStepKind
	do   func()
	emit Item
	wait string
}

type streamBuilder struct {
	reply *streamReply
}

func (b *streamBuilder) AssertRequest(fn func(Req)) {
	b.reply.assertRequest = fn
}

func (b *streamBuilder) Do(fn func()) {
	b.reply.steps = append(b.reply.steps, streamStep{kind: streamStepDo, do: fn})
}

func (b *streamBuilder) Emit(v Item) {
	b.reply.steps = append(b.reply.steps, streamStep{kind: streamStepEmit, emit: v})
}

func (b *streamBuilder) Wait(name string) {
	b.reply.steps = append(b.reply.steps, streamStep{kind: streamStepWait, wait: name})
}

func newFakeStreamer() *fakeStreamer {
	f := &fakeStreamer{}
	f.runtime.owner = f
	return f
}

func (f *fakeStreamer) Reply(fn func(b *streamBuilder)) *fakeStreamer {
	r := streamReply{}
	b := &streamBuilder{reply: &r}
	fn(b)
	f.replies = append(f.replies, r)
	return f
}

func (f *fakeStreamer) Streamer() LLMStreamer {
	return &f.runtime
}

func (f *fakeStreamer) CallCount() int {
	return f.calls
}

func (f *fakeStreamer) AssertCallCount(t *testing.T) {
	t.Helper()
	want := len(f.replies)
	if f.calls != want {
		t.Fatalf("expected %d stream calls, got %d", want, f.calls)
	}
}

func (f *fakeStreamer) Requests() []Req {
	out := make([]Req, 0, len(f.requests))
	for _, req := range f.requests {
		out = append(out, Req{
			Instruction: req.Instruction,
			Items:       append([]Item(nil), req.Items...),
			Tools:       cloneToolOfferSnapshot(req.Tools),
		})
	}
	return out
}

func (f *fakeStreamer) Resolve(name string) {
	g := f.waitGate(name)
	f.mu.Lock()
	if !g.closed {
		close(g.ch)
		g.closed = true
	}
	f.mu.Unlock()
}

func (r *fakeStreamerRuntime) StreamReq(req Req, emit func(Item) error) error {
	return r.owner.streamReq(req, emit)
}

func (f *fakeStreamer) streamReq(req Req, emit func(Item) error) error {
	f.calls++
	callIdx := f.calls - 1
	f.requests = append(f.requests, Req{
		Instruction: req.Instruction,
		Items:       append([]Item(nil), req.Items...),
		Tools:       cloneToolOfferSnapshot(req.Tools),
	})
	if callIdx >= len(f.replies) {
		return nil
	}
	reply := f.replies[callIdx]
	if reply.assertRequest != nil {
		reply.assertRequest(req)
	}
	for _, step := range reply.steps {
		switch step.kind {
		case streamStepDo:
			if step.do != nil {
				step.do()
			}
		case streamStepWait:
			<-f.waitGate(step.wait).ch
		case streamStepEmit:
			if err := emit(step.emit); err != nil {
				return err
			}
		}
	}
	return nil
}

func (f *fakeStreamer) waitGate(name string) *waitGate {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.waits == nil {
		f.waits = map[string]*waitGate{}
	}
	g, ok := f.waits[name]
	if ok {
		return g
	}
	g = &waitGate{ch: make(chan struct{})}
	f.waits[name] = g
	return g
}

func testToolsSnapshot(name, description string) ToolsSnapshot {
	return ToolsSnapshot{
		Snapshot: ToolOfferSnapshot{Offered: []ToolSpec{{
			Name:        name,
			Description: description,
			Payload:     ToolPayloadJSONSchema(gschema.Schema{Type: "object"}),
		}}},
		Handlers: []ToolHandlerBinding{{
			Name:            name,
			HandlerLoadData: []byte(`{"function":"test/default@v1"}`),
		}},
	}
}
