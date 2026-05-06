package threads

import (
	"context"
	"errors"
	"sync"
)

type Req struct {
	Instruction string
	Items       []Item
	ItemMeta    []map[string]any
	Tools       ToolOfferSnapshot
}

type StreamerCapabilities struct {
	AssistantPrefix      bool
	ToolResultSendPolicy ToolResultSendPolicy
}

type ToolResultSendPolicy string

const (
	ToolResultSendPermissive       ToolResultSendPolicy = ""
	ToolResultSendRequiresComplete ToolResultSendPolicy = "requires_complete"
)

type LLMStreamer interface {
	Capabilities() StreamerCapabilities
	RegisterToolNormalizer(name string, normalizer ToolNormalizer)
	UnregisterToolNormalizer(name string)
	StreamReq(req Req, emit func(Item) error) error
}

type ContextLLMStreamer interface {
	Capabilities() StreamerCapabilities
	RegisterToolNormalizer(name string, normalizer ToolNormalizer)
	UnregisterToolNormalizer(name string)
	StreamReqContext(ctx context.Context, req Req, emit func(Item) error) error
}

type ThreadExecutor struct {
	streamer       LLMStreamer
	requestBuilder RequestBuilder
	mu             sync.Mutex
	cancelStream   context.CancelFunc
	cancelToken    *struct{}
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

func (x *ThreadExecutor) RequestTokenEstimator() RequestTokenEstimator {
	if x == nil || x.streamer == nil {
		return nil
	}
	if estimator, ok := x.streamer.(RequestTokenEstimator); ok {
		return estimator
	}
	return nil
}

func (x *ThreadExecutor) TextTokenEstimator() TextTokenEstimator {
	if x == nil || x.streamer == nil {
		return nil
	}
	if estimator, ok := x.streamer.(TextTokenEstimator); ok {
		return estimator
	}
	return nil
}

func (x *ThreadExecutor) OnControlBlockStateChange(t *Thread, _, to State) error {
	if to != StateConstructLLMRequest || x.streamer == nil {
		return nil
	}
	t.policy = x.StreamerCapabilities().ToolResultSendPolicy
	in := t.requestSnapshotWithBuilder(x.requestBuilder)
	if err := t.beginStreaming(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	token := &struct{}{}
	x.mu.Lock()
	x.cancelStream = cancel
	x.cancelToken = token
	x.mu.Unlock()
	defer func() {
		x.mu.Lock()
		if x.cancelToken == token {
			x.cancelStream = nil
			x.cancelToken = nil
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
