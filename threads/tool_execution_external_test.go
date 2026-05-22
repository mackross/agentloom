package threads_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	gschema "github.com/google/jsonschema-go/jsonschema"

	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/simpletool"
)

func TestCancelCurrentTurnCancelsBlockingToolResolver(t *testing.T) {
	thread := threads.New()
	thread.SetToolProvider(simpletool.ProviderFunc(func(_ *threads.Thread) threads.ToolsSnapshot {
		return testBoundToolsSnapshot("slow", "slow tool", `{"function":"tool/slow@v1"}`)
	}))
	resolverStarted := make(chan struct{}, 1)
	resolverDone := make(chan error, 1)
	thread.SetToolResolver(simpletool.ResolverFunc(func(ctx context.Context, _ *threads.Thread, call threads.ToolCall, _ json.RawMessage) (threads.ToolDispatch, error) {
		resolverStarted <- struct{}{}
		<-ctx.Done()
		resolverDone <- ctx.Err()
		return threads.ToolDispatch{}, ctx.Err()
	}))
	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
		b.Emit(threads.ToolCall{CallID: "c1", Name: "slow", Payload: `{}`})
	})
	thread.SetExecutor(threads.NewThreadExecutor(streamer))

	sendDone := make(chan struct{})
	go func() {
		defer close(sendDone)
		thread.QueueItem(threads.UserText("hello"))
		thread.QueueItem(threads.SendItem{})
	}()
	<-resolverStarted
	if !thread.CancelCurrentTurn() {
		t.Fatalf("expected active tool resolver to cancel")
	}
	if err := <-resolverDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("resolver context error = %v, want context canceled", err)
	}
	<-sendDone
	streamer.AssertCallCount(t)
}

func TestToolProviderAndResolverExecuteToolCallsEndToEnd(t *testing.T) {
	thread := threads.New()

	thread.SetToolProvider(simpletool.ProviderFunc(func(_ *threads.Thread) threads.ToolsSnapshot {
		return testBoundToolsSnapshot("calc", "calculate", `{"function":"tool/calc@v1","answer":"3"}`)
	}))
	resolveCalls := 0
	thread.SetToolResolver(simpletool.ResolverFunc(func(_ context.Context, _ *threads.Thread, call threads.ToolCall, handlerLoadData json.RawMessage) (threads.ToolDispatch, error) {
		resolveCalls++
		var cfg struct {
			Answer string `json:"answer"`
		}
		if err := json.Unmarshal(handlerLoadData, &cfg); err != nil {
			return threads.ToolDispatch{}, err
		}
		return threads.ToolDispatch{
			Started: true,
			Items:   []threads.Item{threads.ToolCallResult{CallID: call.CallID, Output: cfg.Answer}},
		}, nil
	}))

	streamer := newFakeStreamer().
		Reply(func(b *streamBuilder) {
			b.AssertRequest(func(req threads.Req) {
				if got := req.Items; !reflect.DeepEqual(got, []threads.Item{threads.UserText("hello")}) {
					t.Fatalf("unexpected first request items: %#v", got)
				}
			})
			b.Emit(threads.ToolCall{CallID: "c1", Name: "calc", Payload: `{"a":1,"b":2}`})
		}).
		Reply(func(b *streamBuilder) {
			b.AssertRequest(func(req threads.Req) {
				want := []threads.Item{
					threads.UserText("hello"),
					threads.ToolCall{CallID: "c1", Name: "calc", Payload: `{"a":1,"b":2}`},
					threads.ToolCallResult{CallID: "c1", Output: "3"},
				}
				if got := req.Items; !reflect.DeepEqual(got, want) {
					t.Fatalf("unexpected follow-up request items: %#v", got)
				}
			})
			b.Emit(threads.AssistantText("done"))
		})
	thread.SetExecutor(threads.NewThreadExecutor(streamer))

	thread.QueueItem(threads.UserText("hello"))
	thread.QueueItem(threads.SendItem{})

	if resolveCalls != 1 {
		t.Fatalf("expected one tool resolution, got %d", resolveCalls)
	}
	snap, err := thread.Snapshot()
	if err != nil {
		t.Fatalf("snapshot thread: %v", err)
	}
	order := []string{}
	for _, item := range snap.Items {
		if item.ID == "c1" {
			order = append(order, item.Type)
		}
	}
	if want := []string{"tool_call", "tool_call_resolving", "tool_call_started", "tool_result"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("unexpected tool lifecycle order: %#v", order)
	}
	streamer.AssertCallCount(t)
}

func TestRollbackableToolFailureRequestProjection(t *testing.T) {
	steeringHint := "\n\n<tool_call_hint tool=\"calc\">\nCall calc again with valid JSON.\n</tool_call_hint>"
	tests := []struct {
		name         string
		caps         threads.StreamerCapabilities
		wantFollowup []threads.Item
	}{
		{
			name: "assistant prefix safely rolls back failed sole tool call",
			caps: threads.StreamerCapabilities{AssistantPrefix: true},
			wantFollowup: []threads.Item{
				threads.UserText("hello" + steeringHint),
			},
		},
		{
			name: "without assistant prefix falls back to tool result",
			caps: threads.StreamerCapabilities{},
			wantFollowup: []threads.Item{
				threads.UserText("hello"),
				threads.ToolCall{CallID: "c1", Name: "calc", Payload: `bad`},
				threads.ToolCallResult{
					CallID: "c1",
					Output: "invalid JSON",
					SafeRollback: &threads.ToolCallSafeRollback{
						SteeringHint: steeringHint,
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			thread := threads.New()
			thread.SetToolProvider(simpletool.ProviderFunc(func(_ *threads.Thread) threads.ToolsSnapshot {
				return testBoundToolsSnapshot("calc", "calculate", `{"function":"tool/calc@v1"}`)
			}))
			thread.SetToolResolver(simpletool.ResolverFunc(func(_ context.Context, _ *threads.Thread, call threads.ToolCall, _ json.RawMessage) (threads.ToolDispatch, error) {
				return threads.ToolDispatch{
					Started: true,
					Items: []threads.Item{threads.ToolCallResult{
						CallID: call.CallID,
						Output: "invalid JSON",
						SafeRollback: &threads.ToolCallSafeRollback{
							SteeringHint: steeringHint,
						},
					}},
				}, nil
			}))

			streamer := newFakeStreamer()
			streamer.capabilities = tt.caps
			streamer.
				Reply(func(b *streamBuilder) {
					b.AssertRequest(func(req threads.Req) {
						if got := req.Items; !reflect.DeepEqual(got, []threads.Item{threads.UserText("hello")}) {
							t.Fatalf("unexpected first request items: %#v", got)
						}
					})
					b.Emit(threads.ToolCall{CallID: "c1", Name: "calc", Payload: `bad`})
				}).
				Reply(func(b *streamBuilder) {
					b.AssertRequest(func(req threads.Req) {
						if got := req.Items; !reflect.DeepEqual(got, tt.wantFollowup) {
							t.Fatalf("unexpected follow-up request items:\n got: %#v\nwant: %#v", got, tt.wantFollowup)
						}
					})
					b.Emit(threads.AssistantText("done"))
				})
			thread.SetExecutor(threads.NewThreadExecutor(streamer))

			thread.QueueItem(threads.UserText("hello"))
			thread.QueueItem(threads.SendItem{})

			streamer.AssertCallCount(t)
		})
	}
}

func TestToolResolutionIgnoresOutOfOrderChunksAfterFinalCall(t *testing.T) {
	thread := threads.New()

	thread.SetToolProvider(simpletool.ProviderFunc(func(_ *threads.Thread) threads.ToolsSnapshot {
		return testBoundToolsSnapshot("calc", "calculate", `{"function":"tool/calc@v1","answer":"3"}`)
	}))
	resolveCalls := 0
	thread.SetToolResolver(simpletool.ResolverFunc(func(_ context.Context, _ *threads.Thread, call threads.ToolCall, handlerLoadData json.RawMessage) (threads.ToolDispatch, error) {
		resolveCalls++
		return threads.ToolDispatch{
			Started: true,
			Items:   []threads.Item{threads.ToolCallResult{CallID: call.CallID, Output: "3"}},
		}, nil
	}))

	streamer := newFakeStreamer().
		Reply(func(b *streamBuilder) {
			b.Emit(threads.ToolCall{CallID: "c1", Name: "calc", Payload: `{"a":1}`})
			b.Emit(threads.ToolCallChunk{CallID: "c1", PayloadDelta: `{"stale":true}`})
		}).
		Reply(func(b *streamBuilder) {
			b.AssertRequest(func(req threads.Req) {
				want := []threads.Item{
					threads.UserText("hello"),
					threads.ToolCall{CallID: "c1", Name: "calc", Payload: `{"a":1}`},
					threads.ToolCallResult{CallID: "c1", Output: "3"},
				}
				if got := req.Items; !reflect.DeepEqual(got, want) {
					t.Fatalf("unexpected follow-up request items: %#v", got)
				}
			})
			b.Emit(threads.AssistantText("done"))
		})
	thread.SetExecutor(threads.NewThreadExecutor(streamer))

	thread.QueueItem(threads.UserText("hello"))
	thread.QueueItem(threads.SendItem{})

	if resolveCalls != 1 {
		t.Fatalf("expected one tool resolution, got %d", resolveCalls)
	}
	streamer.AssertCallCount(t)
}

func TestToolResolutionUsesUpdatedProviderForLaterToolRequests(t *testing.T) {
	thread := threads.New()

	providerA := simpletool.ProviderFunc(func(_ *threads.Thread) threads.ToolsSnapshot {
		return testBoundToolsSnapshot("write_file", "write contents", `{"function":"tool/write-file@v1","filename":"old.txt"}`)
	})
	providerB := simpletool.ProviderFunc(func(_ *threads.Thread) threads.ToolsSnapshot {
		return testBoundToolsSnapshot("write_file", "write contents", `{"function":"tool/write-file@v1","filename":"new.txt"}`)
	})
	thread.SetToolProvider(providerA)

	seenLoadData := []string{}
	thread.SetToolResolver(simpletool.ResolverFunc(func(_ context.Context, _ *threads.Thread, call threads.ToolCall, handlerLoadData json.RawMessage) (threads.ToolDispatch, error) {
		seenLoadData = append(seenLoadData, string(handlerLoadData))
		var cfg struct {
			Filename string `json:"filename"`
		}
		if err := json.Unmarshal(handlerLoadData, &cfg); err != nil {
			return threads.ToolDispatch{}, err
		}
		if call.CallID == "c1" {
			thread.SetToolProvider(providerB)
		}
		return threads.ToolDispatch{
			Started: true,
			Items:   []threads.Item{threads.ToolCallResult{CallID: call.CallID, Output: cfg.Filename}},
		}, nil
	}))

	streamer := newFakeStreamer().
		Reply(func(b *streamBuilder) {
			b.AssertRequest(func(req threads.Req) {
				want := testBoundToolsSnapshot("write_file", "write contents", `{"function":"tool/write-file@v1","filename":"old.txt"}`).Snapshot
				if !reflect.DeepEqual(req.Tools, want) {
					t.Fatalf("unexpected first request tools: %#v", req.Tools)
				}
			})
			b.Emit(threads.ToolCall{CallID: "c1", Name: "write_file", Payload: `{"contents":"old"}`})
		}).
		Reply(func(b *streamBuilder) {
			b.AssertRequest(func(req threads.Req) {
				wantTools := testBoundToolsSnapshot("write_file", "write contents", `{"function":"tool/write-file@v1","filename":"new.txt"}`).Snapshot
				if !reflect.DeepEqual(req.Tools, wantTools) {
					t.Fatalf("unexpected second request tools: %#v", req.Tools)
				}
				wantItems := []threads.Item{
					threads.UserText("hello"),
					threads.ToolCall{CallID: "c1", Name: "write_file", Payload: `{"contents":"old"}`},
					threads.ToolCallResult{CallID: "c1", Output: "old.txt"},
				}
				if got := req.Items; !reflect.DeepEqual(got, wantItems) {
					t.Fatalf("unexpected second request items: %#v", got)
				}
			})
			b.Emit(threads.ToolCall{CallID: "c2", Name: "write_file", Payload: `{"contents":"new"}`})
		}).
		Reply(func(b *streamBuilder) {
			b.AssertRequest(func(req threads.Req) {
				wantItems := []threads.Item{
					threads.UserText("hello"),
					threads.ToolCall{CallID: "c1", Name: "write_file", Payload: `{"contents":"old"}`},
					threads.ToolCallResult{CallID: "c1", Output: "old.txt"},
					threads.ToolCall{CallID: "c2", Name: "write_file", Payload: `{"contents":"new"}`},
					threads.ToolCallResult{CallID: "c2", Output: "new.txt"},
				}
				if got := req.Items; !reflect.DeepEqual(got, wantItems) {
					t.Fatalf("unexpected third request items: %#v", got)
				}
			})
			b.Emit(threads.AssistantText("done"))
		})
	thread.SetExecutor(threads.NewThreadExecutor(streamer))

	thread.QueueItem(threads.UserText("hello"))
	thread.QueueItem(threads.SendItem{})

	wantLoadData := []string{
		`{"function":"tool/write-file@v1","filename":"old.txt"}`,
		`{"function":"tool/write-file@v1","filename":"new.txt"}`,
	}
	if !reflect.DeepEqual(seenLoadData, wantLoadData) {
		t.Fatalf("unexpected resolver load data: %#v", seenLoadData)
	}
	streamer.AssertCallCount(t)
}

func TestCancelCurrentTurnSuppressesToolResultAutoSend(t *testing.T) {
	thread := threads.New()
	thread.SetToolProvider(simpletool.ProviderFunc(func(_ *threads.Thread) threads.ToolsSnapshot {
		return testBoundToolsSnapshot("calc", "calculate", `{"function":"tool/calc@v1","answer":"3"}`)
	}))
	thread.SetToolResolver(simpletool.ResolverFunc(func(_ context.Context, _ *threads.Thread, call threads.ToolCall, handlerLoadData json.RawMessage) (threads.ToolDispatch, error) {
		return threads.ToolDispatch{
			Started: true,
			Items:   []threads.Item{threads.ToolCallResult{CallID: call.CallID, Output: "3"}},
		}, nil
	}))

	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
		b.Emit(threads.ToolCall{CallID: "c1", Name: "calc", Payload: `{"a":1,"b":2}`})
		b.Do(func() {
			if !thread.CancelCurrentTurn() {
				t.Fatalf("expected active turn to cancel")
			}
		})
	})
	thread.SetExecutor(threads.NewThreadExecutor(streamer))

	thread.QueueItem(threads.UserText("hello"))
	thread.QueueItem(threads.SendItem{})

	streamer.AssertCallCount(t)
}

func TestToolDispatchManualContinueDoesNotAutoSend(t *testing.T) {
	thread := threads.New()
	thread.SetToolProvider(simpletool.ProviderFunc(func(_ *threads.Thread) threads.ToolsSnapshot {
		return testBoundToolsSnapshot("calc", "calculate", `{"function":"tool/calc@v1","answer":"3"}`)
	}))
	thread.SetToolResolver(simpletool.ResolverFunc(func(_ context.Context, _ *threads.Thread, call threads.ToolCall, handlerLoadData json.RawMessage) (threads.ToolDispatch, error) {
		return threads.ToolDispatch{
			Started:  true,
			Continue: threads.ToolContinueManual,
			Items:    []threads.Item{threads.ToolCallResult{CallID: call.CallID, Output: "3"}},
		}, nil
	}))

	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
		b.Emit(threads.ToolCall{CallID: "c1", Name: "calc", Payload: `{"a":1,"b":2}`})
	})
	thread.SetExecutor(threads.NewThreadExecutor(streamer))

	thread.QueueItem(threads.UserText("hello"))
	thread.QueueItem(threads.SendItem{})

	streamer.AssertCallCount(t)
}

func TestCancelCurrentTurnWithoutActiveStreamSuppressesLateToolResultAutoSend(t *testing.T) {
	thread := threads.New()
	thread.SetToolProvider(simpletool.ProviderFunc(func(_ *threads.Thread) threads.ToolsSnapshot {
		return testBoundToolsSnapshot("calc", "calculate", `{"function":"tool/calc@v1","answer":"3"}`)
	}))
	thread.SetToolResolver(simpletool.ResolverFunc(func(_ context.Context, _ *threads.Thread, call threads.ToolCall, handlerLoadData json.RawMessage) (threads.ToolDispatch, error) {
		return threads.ToolDispatch{Started: true}, nil
	}))

	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
		b.Emit(threads.ToolCall{CallID: "c1", Name: "calc", Payload: `{"a":1,"b":2}`})
	})
	thread.SetExecutor(threads.NewThreadExecutor(streamer))

	thread.QueueItem(threads.UserText("hello"))
	thread.QueueItem(threads.SendItem{})
	if !thread.CancelCurrentTurn() {
		t.Fatalf("expected cancel to be accepted for pending tool result")
	}
	thread.QueueItem(threads.ToolCallResult{CallID: "c1", Output: "3"})

	streamer.AssertCallCount(t)
}

func TestLateToolResultForStartedAutoContinueDispatchQueuesSend(t *testing.T) {
	thread := threads.New()
	thread.SetToolProvider(simpletool.ProviderFunc(func(_ *threads.Thread) threads.ToolsSnapshot {
		return testBoundToolsSnapshot("calc", "calculate", `{"function":"tool/calc@v1","answer":"3"}`)
	}))
	thread.SetToolResolver(simpletool.ResolverFunc(func(_ context.Context, _ *threads.Thread, call threads.ToolCall, handlerLoadData json.RawMessage) (threads.ToolDispatch, error) {
		return threads.ToolDispatch{
			Started: true,
		}, nil
	}))

	streamer := newFakeStreamer().
		Reply(func(b *streamBuilder) {
			b.Emit(threads.ToolCall{CallID: "c1", Name: "calc", Payload: `{"a":1,"b":2}`})
		}).
		Reply(func(b *streamBuilder) {
			b.AssertRequest(func(req threads.Req) {
				want := []threads.Item{
					threads.UserText("hello"),
					threads.ToolCall{CallID: "c1", Name: "calc", Payload: `{"a":1,"b":2}`},
					threads.ToolCallResult{CallID: "c1", Output: "3"},
				}
				if got := req.Items; !reflect.DeepEqual(got, want) {
					t.Fatalf("unexpected follow-up request items: %#v", got)
				}
			})
			b.Emit(threads.AssistantText("done"))
		})
	thread.SetExecutor(threads.NewThreadExecutor(streamer))

	thread.QueueItem(threads.UserText("hello"))
	thread.QueueItem(threads.SendItem{})
	thread.QueueItem(threads.ToolCallResult{CallID: "c1", Output: "3"})

	streamer.AssertCallCount(t)
}

func TestLateToolResultForStartedManualContinueDispatchDoesNotQueueSend(t *testing.T) {
	thread := threads.New()
	thread.SetToolProvider(simpletool.ProviderFunc(func(_ *threads.Thread) threads.ToolsSnapshot {
		return testBoundToolsSnapshot("calc", "calculate", `{"function":"tool/calc@v1","answer":"3"}`)
	}))
	thread.SetToolResolver(simpletool.ResolverFunc(func(_ context.Context, _ *threads.Thread, call threads.ToolCall, handlerLoadData json.RawMessage) (threads.ToolDispatch, error) {
		return threads.ToolDispatch{
			Started:  true,
			Continue: threads.ToolContinueManual,
		}, nil
	}))

	streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
		b.Emit(threads.ToolCall{CallID: "c1", Name: "calc", Payload: `{"a":1,"b":2}`})
	})
	thread.SetExecutor(threads.NewThreadExecutor(streamer))

	thread.QueueItem(threads.UserText("hello"))
	thread.QueueItem(threads.SendItem{})
	thread.QueueItem(threads.ToolCallResult{CallID: "c1", Output: "3"})

	streamer.AssertCallCount(t)
}

func TestToolAutoContinueUsesExistingPendingSendBoundary(t *testing.T) {
	thread := threads.New()
	thread.SetToolProvider(simpletool.ProviderFunc(func(_ *threads.Thread) threads.ToolsSnapshot {
		return testBoundToolsSnapshot("calc", "calculate", `{"function":"tool/calc@v1","answer":"3"}`)
	}))
	thread.SetToolResolver(simpletool.ResolverFunc(func(_ context.Context, _ *threads.Thread, call threads.ToolCall, handlerLoadData json.RawMessage) (threads.ToolDispatch, error) {
		return threads.ToolDispatch{
			Started: true,
			Items:   []threads.Item{threads.ToolCallResult{CallID: call.CallID, Output: "3"}},
		}, nil
	}))

	streamer := newFakeStreamer().
		Reply(func(b *streamBuilder) {
			b.Do(func() { thread.QueueItem(threads.SendItem{}) })
			b.Emit(threads.ToolCall{CallID: "c1", Name: "calc", Payload: `{"a":1,"b":2}`})
		}).
		Reply(func(b *streamBuilder) {
			b.AssertRequest(func(req threads.Req) {
				want := []threads.Item{
					threads.UserText("hello"),
					threads.ToolCall{CallID: "c1", Name: "calc", Payload: `{"a":1,"b":2}`},
					threads.ToolCallResult{CallID: "c1", Output: "3"},
				}
				if got := req.Items; !reflect.DeepEqual(got, want) {
					t.Fatalf("unexpected follow-up request items: %#v", got)
				}
			})
			b.Emit(threads.AssistantText("done"))
		})
	thread.SetExecutor(threads.NewThreadExecutor(streamer))

	thread.QueueItem(threads.UserText("hello"))
	thread.QueueItem(threads.SendItem{})

	streamer.AssertCallCount(t)
}

func TestToolManualContinueUsesExistingPendingSendBoundary(t *testing.T) {
	thread := threads.New()
	thread.SetToolProvider(simpletool.ProviderFunc(func(_ *threads.Thread) threads.ToolsSnapshot {
		return testBoundToolsSnapshot("calc", "calculate", `{"function":"tool/calc@v1","answer":"3"}`)
	}))
	thread.SetToolResolver(simpletool.ResolverFunc(func(_ context.Context, _ *threads.Thread, call threads.ToolCall, handlerLoadData json.RawMessage) (threads.ToolDispatch, error) {
		return threads.ToolDispatch{
			Started:  true,
			Continue: threads.ToolContinueManual,
			Items:    []threads.Item{threads.ToolCallResult{CallID: call.CallID, Output: "3"}},
		}, nil
	}))

	streamer := newFakeStreamer().
		Reply(func(b *streamBuilder) {
			b.Do(func() { thread.QueueItem(threads.SendItem{}) })
			b.Emit(threads.ToolCall{CallID: "c1", Name: "calc", Payload: `{"a":1,"b":2}`})
		}).
		Reply(func(b *streamBuilder) {
			b.AssertRequest(func(req threads.Req) {
				want := []threads.Item{
					threads.UserText("hello"),
					threads.ToolCall{CallID: "c1", Name: "calc", Payload: `{"a":1,"b":2}`},
					threads.ToolCallResult{CallID: "c1", Output: "3"},
				}
				if got := req.Items; !reflect.DeepEqual(got, want) {
					t.Fatalf("unexpected follow-up request items: %#v", got)
				}
			})
			b.Emit(threads.AssistantText("done"))
		})
	thread.SetExecutor(threads.NewThreadExecutor(streamer))

	thread.QueueItem(threads.UserText("hello"))
	thread.QueueItem(threads.SendItem{})

	streamer.AssertCallCount(t)
}

func testBoundToolsSnapshot(name, description, handlerLoadData string) threads.ToolsSnapshot {
	return threads.ToolsSnapshot{
		Snapshot: threads.ToolOfferSnapshot{Offered: []threads.ToolSpec{{
			Name:        name,
			Description: description,
			Payload:     threads.ToolPayloadJSONSchema(gschema.Schema{Type: "object"}),
		}}},
		Handlers: []threads.ToolHandlerBinding{{
			Name:            name,
			HandlerLoadData: json.RawMessage(handlerLoadData),
		}},
	}
}

type fakeStreamer struct {
	capabilities threads.StreamerCapabilities
	replies      []streamReply
	calls        int
	requests     []threads.Req
}

type streamReply struct {
	assertRequest func(threads.Req)
	steps         []streamStep
}

type streamStep struct {
	do   func()
	emit threads.Item
}

type streamBuilder struct {
	reply *streamReply
}

func newFakeStreamer() *fakeStreamer {
	return &fakeStreamer{capabilities: threads.StreamerCapabilities{AssistantPrefix: true}}
}

func (f *fakeStreamer) Reply(fn func(*streamBuilder)) *fakeStreamer {
	r := streamReply{}
	fn(&streamBuilder{reply: &r})
	f.replies = append(f.replies, r)
	return f
}

func (b *streamBuilder) AssertRequest(fn func(threads.Req)) {
	b.reply.assertRequest = fn
}

func (b *streamBuilder) Do(fn func()) {
	b.reply.steps = append(b.reply.steps, streamStep{do: fn})
}

func (b *streamBuilder) Emit(v threads.Item) {
	b.reply.steps = append(b.reply.steps, streamStep{emit: v})
}

func (f *fakeStreamer) Capabilities() threads.StreamerCapabilities {
	return f.capabilities
}

func (f *fakeStreamer) RegisterToolNormalizer(string, threads.ToolNormalizer) {}

func (f *fakeStreamer) UnregisterToolNormalizer(string) {}

func (f *fakeStreamer) StreamReq(req threads.Req, emit func(threads.Item) error) error {
	f.calls++
	f.requests = append(f.requests, cloneReq(req))
	idx := f.calls - 1
	if idx >= len(f.replies) {
		return nil
	}
	reply := f.replies[idx]
	if reply.assertRequest != nil {
		reply.assertRequest(req)
	}
	for _, step := range reply.steps {
		if step.do != nil {
			step.do()
			continue
		}
		if err := emit(step.emit); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeStreamer) AssertCallCount(t *testing.T) {
	t.Helper()
	if got, want := f.calls, len(f.replies); got != want {
		t.Fatalf("expected %d stream calls, got %d", want, got)
	}
}

func cloneReq(in threads.Req) threads.Req {
	return threads.Req{
		Instruction: in.Instruction,
		Items:       append([]threads.Item(nil), in.Items...),
		Tools:       cloneToolOfferSnapshot(in.Tools),
	}
}

func cloneToolOfferSnapshot(in threads.ToolOfferSnapshot) threads.ToolOfferSnapshot {
	buf, err := json.Marshal(in)
	if err != nil {
		panic(err)
	}
	var out threads.ToolOfferSnapshot
	if err := json.Unmarshal(buf, &out); err != nil {
		panic(err)
	}
	return out
}
