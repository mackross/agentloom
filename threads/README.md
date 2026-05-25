# threads

`threads` models a conversation thread as an append-only history of items plus a small
control block that derives runtime state from that history.

This document is intentionally about the package as it exists today. The notes
in [`SPEC.md`](./SPEC.md) still include forward-looking ideas; this file is the
implemented feature overview.

## Implemented Today

- Append-only thread state via `Thread` and `Item` values such as:
  - `UserText`
  - `AssistantText`
  - `AssistantInstruction`
  - `ToolCall`
  - `ToolCallResolving`
  - `ToolCallResult`
  - `ToolsSnapshot`
  - `SendItem`
- A control-block-driven state machine with these runtime states:
  - `idle`
  - `construct_llm_request`
  - `receiving_stream`
  - `stream_complete`
- Request construction from the current thread history:
  - the last `AssistantInstruction` becomes `Req.Instruction`
  - the last `ToolsSnapshot` becomes `Req.Tools`
  - adjacent `UserText` and `AssistantText` items coalesce
  - control items stay in the thread history but do not become model-visible message
    content unless they are tool calls or tool results
- Streaming execution through `ThreadExecutor` and `LLMStreamer`
  - queueing `SendItem{}` moves the thread into request construction
  - `LLMStreamer.Capabilities()` reports provider request-shape support such as
    assistant-prefixed continuation
  - streamed items are appended back onto the thread in order
  - `ToolCallChunk` values are accumulated by call id until a final `ToolCall`
    arrives
- Tool execution with durable hydration boundaries:
  - `ToolProvider` supplies a `ToolsSnapshot`
  - `ToolResolver` turns `(tool name, handler load data)` into a handler
  - tool calls resolve against the nearest preceding `ToolsSnapshot`
  - tool results are appended to the same thread history and automatically followed by a
    new `SendItem{}`
- Delegate hooks for thread lifecycle events:
  - request start
  - streamed item appended
  - idle
  - when a thread is owned by an `EventLoop`, delegate callbacks already run on
    the event-loop mutation lane; use the supplied `Thread` directly inside the
    callback, and use a goroutine for any later `EventLoop.Do` /
    `Branch.RunOnEventLoop` call
- Durability support:
  - full thread snapshots
  - append-only WAL diffs
  - checkpoint policies for safe, waiting, or unsafe capture
  - restore from checkpoint alone or checkpoint plus WAL
  - attach-time resume from retained `construct_llm_request` state
  - a file-backed durable store in [`durability`](./durability)
- Branch storage and branch management:
  - `BranchStore` creates, opens, lists, deletes, and forks branch-local durable
    stores
  - `BranchManager` opens branch refs such as `/branch/<id>` and
    `/branch/<id>/turn/<n>`
  - `OpenAsEphemeralCopy` and `OpenAsDurableCopy` create child branches from a
    head or completed-turn checkpoint
  - copy opens read the parent without taking a writer lease, so callers can
    fork from a branch that is already open elsewhere in the process
- Tool helper packages:
  - [`simpletool`](./simpletool) for small provider/resolver adapters
  - [`tool`](./tool) for catalogs, typed JSON handlers, and result helpers
- Working integrations and examples:
  - OpenAI Responses streamer
  - Anthropic Messages streamer
  - interactive chat example in [`examples/chat`](./examples/chat)
  - event-loop chat example in [`examples/chat_event_loop`](./examples/chat_event_loop)

## Minimal Flow

```go
thread := threads.New()
thread.SetExecutor(threads.NewThreadExecutor(streamer))

thread.QueueItem(threads.AssistantInstruction("Be concise."))
thread.QueueItem(threads.UserText("Hello"))
thread.QueueItem(threads.SendItem{})
```

At that point the executor builds a `threads.Req`, streams model output back
into the thread, and returns to `idle` once the stream is complete.

If the stream contains a tool call and a `ToolResolver` is installed, the thread
will resolve the tool, append the tool result, and automatically send the
follow-up request.

## Current Boundaries

- `threads` owns thread history, control-block transitions, request
  construction, durability, and the minimal tool-routing boundary.
- `threads/tool` owns higher-level tool helpers such as catalogs and typed JSON
  handlers.
- `threads/simpletool` is a small adapter layer for tests and lightweight
  integrations.

## Not Implemented Yet

- Fork/join/merge behavior described in [`SPEC.md`](./SPEC.md) is still design
  work, not current package behavior.
- Unsafe inflight restore exists, and retained `construct_llm_request` state can
  resume when an executor is attached for recovery, but broader executor-resume
  recovery is still an active design area documented in
  [`EXECUTOR_RECOVERY.md`](./EXECUTOR_RECOVERY.md).

## Further Reading

- [`SPEC.md`](./SPEC.md)
- [`DURABILITY.md`](./DURABILITY.md)
- [`TOOL_HYDRATION.md`](./TOOL_HYDRATION.md)
- [`examples/chat/main.go`](./examples/chat/main.go)
- [`examples/chat_event_loop/main.go`](./examples/chat_event_loop/main.go)

## Implementation Audit Notes (2026-05-13)

- Runtime states: this overview lists `idle`, `construct_llm_request`,
  `receiving_stream`, and `stream_complete`, but current code also has
  `awaiting_tool_results`. That state is entered when a streamer requires all
  tool results before the pending follow-up send may advance. Decision: doc
  update.
- Tool continuation: the statement that tool results are automatically followed
  by a new `SendItem{}` describes only the default `ToolContinueAuto` path.
  `ToolContinueManual`, `CancelCurrentTurn`, or an already-pending send can
  suppress or replace the automatic send. Decision: doc update.
- Recovery overview: `SetExecutor` itself is still assignment-only, but
  `AttachExecutorForRecoveryWithOptions` now does more than retained
  `construct_llm_request` recovery: it can handle `receiving_stream` with
  tool-chunk policies and can convert unresolved tool calls to recovered status
  results with `ToolCallRecoveryCancelAll`. Decision: doc update.
