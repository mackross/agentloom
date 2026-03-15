# threads

`threads` models a conversation as an append-only tape of items plus a small
control block that derives runtime state from that tape.

This document is intentionally about the package as it exists today. The notes
in [`SPEC.md`](./SPEC.md) still include forward-looking ideas; this file is the
implemented feature overview.

## Implemented Today

- Append-only thread state via `Thread` and `Item` values such as:
  - `UserText`
  - `AssistantText`
  - `AssistantInstruction`
  - `ToolCall`
  - `ToolCallResult`
  - `ToolsSnapshot`
  - `SendItem`
- A control-block-driven state machine with these runtime states:
  - `idle`
  - `construct_llm_request`
  - `receiving_stream`
  - `stream_complete`
- Request construction from the current tape:
  - the last `AssistantInstruction` becomes `Req.Instruction`
  - the last `ToolsSnapshot` becomes `Req.Tools`
  - adjacent `UserText` and `AssistantText` items coalesce
  - control items stay on the tape but do not become model-visible message
    content unless they are tool calls or tool results
- Streaming execution through `ThreadExecutor` and `LLMStreamer`
  - queueing `SendItem{}` moves the thread into request construction
  - streamed items are appended back onto the tape in order
  - `ToolCallChunk` values are accumulated by call id until a final `ToolCall`
    arrives
- Tool execution with durable hydration boundaries:
  - `ToolProvider` supplies a `ToolsSnapshot`
  - `ToolResolver` turns `(tool name, handler load data)` into a handler
  - tool calls resolve against the nearest preceding `ToolsSnapshot`
  - tool results are appended to the same tape and automatically followed by a
    new `SendItem{}`
- Delegate hooks for thread lifecycle events:
  - request start
  - streamed item appended
  - idle
- Durability support:
  - full thread snapshots
  - append-only WAL diffs
  - checkpoint policies for safe, waiting, or unsafe capture
  - restore from checkpoint alone or checkpoint plus WAL
  - a file-backed durable store in [`durability`](./durability)
- Tool helper packages:
  - [`simpletool`](./simpletool) for small provider/resolver adapters
  - [`tool`](./tool) for catalogs, typed JSON handlers, and result helpers
- Working integrations and examples:
  - OpenAI Responses streamer
  - Anthropic Messages streamer
  - interactive chat example in [`examples/chat`](./examples/chat)

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

- `threads` owns the thread tape, control-block transitions, request
  construction, durability, and the minimal tool-routing boundary.
- `threads/tool` owns higher-level tool helpers such as catalogs and typed JSON
  handlers.
- `threads/simpletool` is a small adapter layer for tests and lightweight
  integrations.

## Not Implemented Yet

- Fork/join/merge behavior described in [`SPEC.md`](./SPEC.md) is still design
  work, not current package behavior.
- Unsafe inflight restore exists, but full executor-resume recovery is still an
  active design area documented in [`EXECUTOR_RECOVERY.md`](./EXECUTOR_RECOVERY.md).

## Further Reading

- [`SPEC.md`](./SPEC.md)
- [`DURABILITY.md`](./DURABILITY.md)
- [`TOOL_HYDRATION.md`](./TOOL_HYDRATION.md)
- [`examples/chat/main.go`](./examples/chat/main.go)
