# agentloom

agentloom is a Go library for building durable agent conversation loops.

It provides a thread state machine, model-provider adapters, tool execution boundaries,
and persistence primitives for applications that need conversations to reliably survive restarts.
WAL with checkpointing data format aligns with databases with faster appends than updates.

## Packages

- `threads`: conversation state, request construction, streaming execution, tool calls,
  branches, checkpoints, and WAL recovery.
- `threads/tool`: helpers for defining catalogs, typed JSON handlers, and tool results.
- `threads/simpletool`: small adapters for lightweight tool providers and resolvers.
- `threads/durability`: local file-backed durable thread storage.
- `threads/durability/sqlitebranchstore`: SQLite branch, lease, checkpoint, and WAL storage.
- `llms/openai`: OpenAI Responses API streamer (websocket and previous message for super fast responses).
- `llms/anthropic`: Anthropic Messages API streamer.
- `llms/fireworks`: Fireworks chat-completions streamer.
- `llms/cache/*`: provider-specific prompt-cache metadata helpers.

## Install

```sh
go get github.com/mackross/agentloom
```

## Minimal example

```go
thread := threads.New()
thread.SetExecutor(threads.NewThreadExecutor(streamer))

thread.QueueItem(threads.AssistantInstruction("Be concise."))
thread.QueueItem(threads.UserText("Hello"))
thread.QueueItem(threads.SendItem{})
```

The executor builds a provider request from the thread, streams model output back into
history, resolves tool calls when configured, and returns the thread to idle.

## Provider examples

Interactive examples live in:

- `threads/examples/chat`
- `threads/examples/chat_event_loop`

They select OpenAI, Anthropic, or Fireworks based on the configured model and provider
environment variables.

## Durability

agentloom supports snapshots, append-only WAL diffs, checkpoint policies, restore, and
attach-time recovery for retained request construction. Start with
[`threads/DURABILITY.md`](./threads/DURABILITY.md) for the durability contract and crash
window expectations.

## Tooling

Tool execution is designed around durable boundaries. See:

- [`threads/TOOL_HYDRATION.md`](./threads/TOOL_HYDRATION.md)
- [`threads/TOOL_DISPATCH.md`](./threads/TOOL_DISPATCH.md)

## Development checks

```sh
go test ./...
```

## License

MIT. See [`LICENSE`](./LICENSE).
