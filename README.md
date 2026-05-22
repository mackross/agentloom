# agentloom

agentloom is a Go library for building agents for real workloads not just
prototypes.

It provides forkable conversation threads that can reliably recover from
interrupted streams with different recovery policies that provide different
guarantees.

The storage interface is based on a write-ahead log (WAL) with checkpoints,
which aligns with most storage patterns (fast append, slower update).

## Features

- Provider-agnostic request construction from concrete transcript items (switch models mid-conversation).
- Streaming model execution with tool-call chunk materialization.
- Tool execution lifecycle markers for resolving, started, recovered, and completed calls.
- Typed helpers for JSON-schema tools and structured tool results.
- Multitool support for stable provider-visible tool surfaces with non cache busting change of subcommands.
- Safe request projection for model-correctable tool failures.
- Branch storage and leases for durable multi-session workflows.
- Provider adapters for OpenAI, Anthropic, Fireworks, Google Gemini, and Cerebras.

## Packages

- `threads`: conversation state, request construction, streaming execution, tool calls,
  branches, checkpoints, WAL replay, and explicit recovery attach helpers.
- `threads/tool`: helpers for defining catalogs, typed JSON handlers, and tool results.
- `threads/tool/multitool`: one stable model-facing tool that routes command-style calls
  to hidden subtools, with Lark/custom-tool and JSON modes.
- `threads/simpletool`: small adapters for lightweight tool providers and resolvers.
- `threads/durability`: local file-backed durable thread storage.
- `threads/durability/sqlitebranchstore`: SQLite branch, lease, checkpoint, and WAL storage.
- `llms/openai`: OpenAI Responses API streamer with websocket/SSE transports,
  previous-response continuation, function tools, and custom grammar tools.
- `llms/anthropic`: Anthropic Messages API streamer.
- `llms/fireworks`: Fireworks chat-completions streamer.
- `llms/googlegenai`: Google Gemini generateContent streamer.
- `llms/cerebras`: Cerebras chat-completions streamer.
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

## Tooling

Tool execution is designed around durable boundaries. See:

- [`threads/TOOL_HYDRATION.md`](./threads/TOOL_HYDRATION.md)
- [`threads/TOOL_DISPATCH.md`](./threads/TOOL_DISPATCH.md)

Tool results are concrete `threads.ToolCallResult` values:

```go
type ToolCallResult struct {
    CallID       string
    Output       string
    Recovered    bool
    Data         map[string]any
    SafeRollback *ToolCallSafeRollback
}
```

`Output` is the model-visible result. `Data` is caller-owned structured data for UI,
debugging, and application state; agentloom does not use magic `Data` keys for control flow.

## Multitool

`threads/tool/multitool` exposes one stable provider-visible tool and routes calls to
hidden subtools.

This is useful when an application wants to keep the model-facing tool surface stable
while changing the command set behind it, or when a workflow should choose one command
from a set without advertising every command as a separate provider tool.

Multitool supports:

- `ModeLark`: custom grammar tool input shaped as:

  ```text
  command arg...

  input text
  ```

- `ModeJSON`: JSON input with `command` and `input` fields.
- JSON subtools that decode command input into typed Go structs.
- Fallback responses for empty or unknown commands.

Example:

```go
type ticketArgs struct {
    Title    string `json:"title"`
    Priority int    `json:"priority"`
}

mt := multitool.New(multitool.Setup{
    Name: "tool",
    Mode: multitool.ModeLark,
}, multitool.Config{
    Subtools: []multitool.Subtool{
        multitool.JSONHandler[ticketArgs](
            multitool.SubtoolSpec{
                Command:     "create-ticket",
                Description: "Create a ticket.",
                Usage:       `{"title":"Bug","priority":3}`,
            },
            "create_ticket",
            tool.JSONHandler(func(ctx context.Context, t *threads.Thread, call tool.Call, args ticketArgs) tool.Item {
                return tool.ResultText(call, "created")
            }),
        ),
    },
})

thread.SetToolProvider(mt)
thread.SetToolResolver(mt)
```

## Safe tool-call repair

Some tool failures are model-correctable, such as malformed JSON input. A tool result can
opt into safe rollback projection:

```go
threads.ToolCallResult{
    CallID: call.CallID,
    Output: "invalid JSON input: ...",
    SafeRollback: &threads.ToolCallSafeRollback{
        SteeringHint: `<tool_call_hint tool="tool">Call again with valid JSON.</tool_call_hint>`,
    },
}
```

For streamers that support assistant-prefix continuation, the default request builder may
project the failed tool call/result out of the next request and insert the steering hint as
user text at the retry point.

This does not rewrite durable history. Once a retry succeeds, the hint disappears from
future requests. `multitool.JSONHandler` uses this mechanism for JSON parse/schema failures
and includes the subtool description, usage, and JSON schema in the steering hint.

## Provider capabilities

Streamers report capabilities such as assistant-prefix continuation and tool-result send
policy. The request builder uses these capabilities to choose safe request projections,
including rollbackable tool failure repair.

## Durability and recovery

agentloom supports snapshots, append-only WAL diffs, checkpoint policies, restore, and
explicit executor attach recovery.

Recovery can resume retained request construction and has limited support for interrupted
streams:

- fail closed on retained tool-call stream material
- roll back interrupted stream output and retry
- keep assistant-prefix output when the target streamer supports it
- append recovered status results for unresolved tool calls with `ToolCallRecoveryCancelAll`

See [`threads/DURABILITY.md`](./threads/DURABILITY.md) and
[`threads/EXECUTOR_RECOVERY.md`](./threads/EXECUTOR_RECOVERY.md).

## Provider examples

Interactive examples live in:

- `threads/examples/chat`
- `threads/examples/chat_event_loop`

They select OpenAI, Anthropic, or Fireworks based on the configured model and provider
environment variables.

## Live provider tests

Most tests are offline. Live tests are behind the `live` build tag and provider environment
variables.

```sh
RUN_LIVE_API_TESTS=1 OPENAI_API_KEY=... go test -tags live ./llms/openai ./threads
```

For the OpenAI multitool repair test:

```sh
RUN_LIVE_API_TESTS=1 OPENAI_API_KEY=... OPENAI_MULTITOOL_LIVE_MODEL=gpt-5.5 \
  go test -tags live ./threads -run TestLiveMultitoolLarkJSONRepairWithOpenAIResponses -count=1 -v
```

## Development checks

```sh
go test ./...
```

## License

MIT. See [`LICENSE`](./LICENSE).
