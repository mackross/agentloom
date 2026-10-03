package threads

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestCancelAllRecoversInterruptedToolHistory(t *testing.T) {
	call := ToolCall{CallID: "c1", Name: "calc", Payload: `{"a":1}`}
	chunk := ToolCallChunk{CallID: "c2", Name: "bash", PayloadDelta: `{"command":"echo`}
	completed := ToolCallResult{CallID: "c1", Output: "known result"}
	for _, prefix := range []bool{false, true} {
		for _, tc := range []struct {
			name       string
			before     []Item
			streamed   []Item
			queued     []Item
			construct  bool
			wantResult string
			wantChunk  bool
			wantKnown  bool
		}{
			{name: "partial tool first", streamed: []Item{chunk}, wantChunk: true},
			{name: "partial after text", streamed: []Item{AssistantText("Checking now."), chunk}, wantChunk: true},
			{name: "complete tool first", streamed: []Item{call}, wantResult: "before this tool was run"},
			{name: "completed sibling", streamed: []Item{call, completed, chunk}, wantKnown: true, wantChunk: true},
			{name: "resolving", before: []Item{call, ToolCallResolving{CallID: "c1"}}, streamed: []Item{chunk}, wantResult: "cannot confirm whether the action completed", wantChunk: true},
			{name: "started unsafe", before: []Item{call, ToolCallStarted{CallID: "c1", Recovery: ToolRecoveryUnsafe}}, streamed: []Item{chunk}, wantResult: "retrying may duplicate an external action", wantChunk: true},
			{name: "queued real result", before: []Item{call, ToolCallStarted{CallID: "c1", Recovery: ToolRecoveryUnsafe}}, streamed: []Item{chunk}, queued: []Item{completed}, wantKnown: true, wantChunk: true},
			{name: "construct request", before: []Item{call, ToolCallStarted{CallID: "c1", Recovery: ToolRecoveryUnsafe}}, construct: true, wantResult: "retrying may duplicate an external action"},
		} {
			name := tc.name + "/no-prefix"
			if prefix {
				name = tc.name + "/prefix"
			}
			t.Run(name, func(t *testing.T) {
				restored, store, cleanup := restoreReceivingStreamFromDurableStore(t, tc.before, tc.streamed...)
				defer cleanup()
				for _, v := range tc.queued {
					restored.QueueItem(v)
				}
				if tc.construct {
					restored.cb.setState(StateConstructLLMRequest)
					restored.cb.streamInsertionPoint = nil
					restored.cb.streamToolCalls = nil
				}
				restored.SetToolResolver(toolResolverFunc(func(context.Context, Thread, ToolCall, json.RawMessage) (ToolDispatch, error) {
					t.Fatal("recovery must not dispatch tools")
					return ToolDispatch{}, nil
				}))
				var request Req
				streamer := newFakeStreamer().Reply(func(b *streamBuilder) {
					b.AssertRequest(func(req Req) {
						request = req
						var results []ToolCallResult
						for _, v := range req.Items {
							switch v := v.(type) {
							case ToolCallChunk:
								t.Fatal("incomplete tool protocol leaked into request")
							case ToolCallResult:
								results = append(results, v)
							}
						}
						notice, ok := req.Items[len(req.Items)-1].(UserText)
						if !ok || !strings.Contains(string(notice), "[Runtime recovery notice") {
							t.Fatalf("request must end in a recovery user message: %#v", req.Items)
						}
						if tc.wantChunk {
							for _, text := range []string{"bash", "c2", "not dispatched", "incomplete", `\"command\"`} {
								if !strings.Contains(string(notice), text) {
									t.Fatalf("notice missing %q: %s", text, notice)
								}
							}
						}
						if tc.wantKnown {
							if !reflect.DeepEqual(results, []ToolCallResult{completed}) {
								t.Fatalf("lost or replaced completed result: %#v", results)
							}
						} else if tc.wantResult != "" {
							if len(results) != 1 || results[0].CallID != call.CallID || !results[0].Recovered || !strings.Contains(results[0].Output, tc.wantResult) {
								t.Fatalf("unexpected recovery results: %#v", results)
							}
						} else if len(results) != 0 {
							t.Fatalf("fabricated results for incomplete calls: %#v", results)
						}
						if tc.wantKnown || tc.wantResult != "" {
							found := false
							for _, v := range req.Items {
								found = found || reflect.DeepEqual(v, call)
							}
							if !found {
								t.Fatal("complete call was dropped")
							}
						}
					})
					b.Emit(AssistantText("done"))
				})
				streamer.capabilities = StreamerCapabilities{AssistantPrefix: prefix}
				opts := RecoveryOptions{ToolChunkPolicy: ToolChunkRecoveryKeepAssistantPrefix, ToolCallPolicy: ToolCallRecoveryCancelAll}
				before := store.replaceCount
				if err := restored.AttachExecutorForRecoveryWithOptions(NewThreadExecutor(streamer.Streamer()), opts); err != nil {
					t.Fatal(err)
				}
				streamer.AssertCallCount(t)
				if store.replaceCount != before+1 {
					t.Fatal("repair was not saved as one snapshot before resuming")
				}
				cp, wal := store.Load()
				reloaded, err := RestoreFromCheckpointAndWAL(cp, wal, RestoreOptions{AllowUnsafe: true})
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(reloaded.items.Slice(), restored.items.Slice()) {
					t.Fatal("repaired history did not survive reload")
				}
				// Simulate a second crash immediately after the repair snapshot,
				// before any retry stream WAL: the notice must not be duplicated.
				again, err := RestoreCheckpoint(cp, RestoreOptions{AllowUnsafe: true})
				if err != nil {
					t.Fatal(err)
				}
				retry := newFakeStreamer().Reply(func(b *streamBuilder) {
					b.AssertRequest(func(req Req) {
						if !reflect.DeepEqual(req.Items, request.Items) {
							t.Fatalf("second restart changed recovery request: %#v", req.Items)
						}
					})
					b.Emit(AssistantText("done"))
				})
				retry.capabilities = streamer.capabilities
				if err := again.AttachExecutorForRecoveryWithOptions(NewThreadExecutor(retry.Streamer()), opts); err != nil {
					t.Fatal(err)
				}
				retry.AssertCallCount(t)
			})
		}
	}
}
