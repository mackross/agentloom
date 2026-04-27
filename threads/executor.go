package threads

import (
	"context"
	"errors"
	"sync"
)

type Req struct {
	Instruction string
	Items       []Item
	Tools       ToolOfferSnapshot
}

type StreamerCapabilities struct {
	AssistantPrefix bool
}

type LLMStreamer interface {
	Capabilities() StreamerCapabilities
	StreamReq(req Req, emit func(Item) error) error
}

type ContextLLMStreamer interface {
	Capabilities() StreamerCapabilities
	StreamReqContext(ctx context.Context, req Req, emit func(Item) error) error
}

type ThreadExecutor struct {
	streamer       LLMStreamer
	requestBuilder RequestBuilder
	mu             sync.Mutex
	cancelStream   context.CancelFunc
}

func NewThreadExecutor(streamer LLMStreamer) *ThreadExecutor {
	return &ThreadExecutor{streamer: streamer, requestBuilder: DefaultRequestBuilder}
}

func (x *ThreadExecutor) StreamerCapabilities() StreamerCapabilities {
	if x == nil || x.streamer == nil {
		return StreamerCapabilities{}
	}
	return x.streamer.Capabilities()
}

func (x *ThreadExecutor) OnControlBlockStateChange(t *Thread, _, to State) error {
	if to != StateConstructLLMRequest || x.streamer == nil {
		return nil
	}
	b := x.requestBuilder
	if b == nil {
		b = DefaultRequestBuilder
	}
	in := b.Build(t.items.SliceThrough(t.cb.IP()))
	if err := t.beginStreaming(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	x.mu.Lock()
	x.cancelStream = cancel
	x.mu.Unlock()
	defer func() {
		x.mu.Lock()
		if x.cancelStream == cancel {
			x.cancelStream = nil
		}
		x.mu.Unlock()
		cancel()
	}()

	err := x.streamReq(ctx, in, func(v Item) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return t.appendStreamItem(v)
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return t.endStreaming()
}

func (x *ThreadExecutor) CancelCurrentStream() bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.cancelStream == nil {
		return false
	}
	x.cancelStream()
	return true
}

func (x *ThreadExecutor) streamReq(ctx context.Context, req Req, emit func(Item) error) error {
	if s, ok := x.streamer.(ContextLLMStreamer); ok {
		return s.StreamReqContext(ctx, req, emit)
	}
	return x.streamer.StreamReq(req, func(v Item) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return emit(v)
	})
}
