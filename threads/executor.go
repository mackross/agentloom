package threads

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

type ThreadExecutor struct {
	streamer       LLMStreamer
	requestBuilder RequestBuilder
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
	err := x.streamer.StreamReq(in, func(v Item) error {
		return t.appendStreamItem(v)
	})
	if err != nil {
		return err
	}
	return t.endStreaming()
}
