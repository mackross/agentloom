# Tool Dispatch Lifecycle

This document is for consumers implementing tool resolution for `threads`.
It focuses on the choices a `ToolResolver` makes and why those choices matter for
continuation and recovery.

## Consumer Boundary

The consumer-facing tool execution boundary is `ToolResolver`:

```go
ResolveTool(ctx, call, handlerLoadData) (ToolDispatch, error)
```

At this point:

- the model has emitted a `ToolCall`
- the thread has found matching handler load data from the nearest preceding
  `ToolsSnapshot`
- the resolver decides what durable thread items to append and what recovery
  guarantees apply

## Mental Model

A tool call has four important durable states:

- requested
  - a `ToolCall` exists
  - no `ToolCallResolving` exists
  - no `ToolCallStarted` exists
  - no tool result exists
  - recovery treats the resolver as not entered
- resolving
  - `ToolCallResolving` exists
  - no `ToolCallStarted` exists
  - no tool result exists
  - recovery treats the call as ambiguous and fails closed by default
- started
  - `ToolCallStarted` exists
  - no tool result exists
  - recovery must use started-tool policy
- completed
  - a matching `ToolCallResult` item exists
  - the tool is no longer outstanding

The key distinction is requested versus resolving/started. If only `ToolCall` is
durable, recovery assumes the resolver was not entered. Once `ToolCallResolving`
is durable, recovery knows runtime tool handling began even if no final dispatch
metadata was produced.

## Lifecycle

From a resolver consumer's point of view:

1. The model emits a `ToolCall`.
2. The thread finishes the model stream.
3. The thread derives outstanding tool calls from history.
4. The thread appends `ToolCallResolving` before entering consumer resolver code.
5. The thread calls `ResolveTool(ctx, call, handlerLoadData)`.
6. The resolver returns `ToolDispatch`.
7. If `dispatch.Started` is true, the thread appends `ToolCallStarted`.
8. The thread appends each item in `dispatch.Items`.
9. If an appended item is resultable and continuation is automatic, the thread
   queues `SendItem{}` for the model follow-up.

## `ToolDispatch` Fields

```go
type ToolDispatch struct {
    Started  bool
    Continue ToolContinue
    Recovery ToolRecovery
    Items    []Item
}
```

### `Started`

`Started` is the durable execution boundary.

Use `Started: true` when the tool execution has crossed, or is about to cross,
the point where recovery must know it began.

Use `Started: false` only when no real tool execution has begun. This is suitable
for cases such as validation failure, planning-only behavior, or synthetic thread
item insertion where recovery can safely treat the call as unstarted.

Why this matters:

- if `Started` is false, no `ToolCallStarted` is recorded
- if the process crashes inside resolver code, recovery will see
  `ToolCallResolving` and fail closed rather than treating the call as never
  entered
- if side effects happened before a result or `ToolCallStarted`, recovery cannot
  know final dispatch metadata and must use the ambiguous resolving state

Recommended default:

- return `Started: true` for real tool execution, including synchronous tools that
  also return a result immediately
- reserve `Started: false` for cases where no execution-relevant work happened

### `Recovery`

`Recovery` describes whether a started-but-unfinished dispatch can be retried.
It is only durable when `Started` is true, because it is copied onto
`ToolCallStarted`.

Choices:

- `ToolRecoverySafe`
  - retrying or replaying the dispatch is acceptable
  - good for pure calculations, deterministic formatting, idempotent reads, or
    operations where duplicate execution is acceptable
- `ToolRecoveryUnsafe`
  - retrying may duplicate side effects or otherwise violate semantics
  - good for writes, sends, mutations, payments, external actions, or anything
    where duplicate execution is not clearly acceptable

If a started dispatch leaves `Recovery` empty, recovery should treat that as
unknown and fail closed.

Why this is per dispatch rather than per tool name:

- the same tool may be safe for some inputs and unsafe for others
- handler load data or runtime mode may change safety
- user-selected options may change whether duplicate execution is acceptable

### `Continue`

`Continue` controls whether a tool result automatically schedules the next model
request.

Choices:

- `ToolContinueAuto`
  - zero value
  - if a resultable item is appended, the thread queues a follow-up `SendItem{}`
  - use for normal tools where the model should continue after seeing the result
- `ToolContinueManual`
  - suppresses automatic continuation
  - use when the application, UI, or user should decide when the model continues

Why this exists:

- completing a tool and continuing the conversation are separate decisions
- some tools produce durable output but should pause for user approval, review, or
  application orchestration before another model request

### `Items`

`Items` are the durable thread outcome of the dispatch.

The normal item is `ToolCallResult`. A result item with automatic continuation
queues a follow-up model request.

Other items are possible, but consumers should prefer `ToolCallResult` for normal
tool execution because it preserves the model-visible call/result relationship.

Rules:

- every item must be non-nil
- result items must reference the correct `CallID`
- returning no result after `Started: true` means the call remains outstanding and
  future recovery must handle it

## Side Effects And Crash Windows

The current API records `ToolCallResolving` before calling `ResolveTool`, then
records `ToolCallStarted` only after `ResolveTool` returns a `ToolDispatch` with
`Started: true`.

That means a crash inside resolver code is durable but ambiguous:

1. resolver begins side-effecting work
2. process crashes before returning `ToolDispatch{Started: true}`
3. `ToolCallResolving` is durable
4. no `ToolCallStarted` or result is durable
5. recovery treats the call as ambiguous and fails closed by default

Consumers should still avoid unnecessary side effects before final dispatch
metadata is known, but the runtime no longer confuses a resolver crash with a
tool call that was never entered.

Practical guidance:

- classify and plan before side effects
- return `Started: true` for real execution
- use `ToolRecoveryUnsafe` when duplicate execution is not acceptable
- design side-effecting handlers to be idempotent when possible

Longer-term, the API may still need a split lifecycle for side-effecting tools
that need precise recovery metadata before execution:

1. prepare and classify
2. durably append `ToolCallStarted`
3. execute
4. append result

Until then, consumers should treat side effects inside `ResolveTool` as
recoverable only through the conservative resolving state unless they return a
more precise `ToolCallStarted` marker and result.

## Synchronous Errors And Panics

There are two different failure cases for synchronous tools.

If failure happens before execution starts, returning an error leaves the durable
state at `ToolCallResolving` because the runtime has already entered resolver
code:

```go
return threads.ToolDispatch{}, err
```

Why:

- no `ToolCallStarted` is recorded
- recovery can tell resolver code was entered
- recovery should fail closed or use explicit application policy rather than
  blindly retrying as if the resolver never ran

If failure happens after execution starts, returning a plain error is still less
precise than returning a dispatch. The thread will not append `ToolCallStarted`
when `ResolveTool` returns an error, so recovery sees ambiguous resolving rather
than concrete started metadata.

For started execution failures, a helper can be useful if it converts ordinary
errors or recovered panics into a `ToolDispatch` rather than returning them as
resolver errors. For example, such a helper could return:

```go
return threads.ToolDispatch{
    Started:  true,
    Recovery: threads.ToolRecoveryUnsafe,
}, nil
```

That records `ToolCallStarted` durably and leaves the call outstanding with
explicit recovery metadata instead of only the ambiguous resolving marker.

If the desired behavior is to report the failure to the model, the helper could
instead return a result item using the project's chosen tool-error convention:

```go
return threads.ToolDispatch{
    Started:  true,
    Recovery: threads.ToolRecoveryUnsafe,
    Items: []threads.Item{
        threads.ToolCallResult{CallID: call.CallID, Output: failureMessage},
    },
}, nil
```

Important limitation:

- a resolver-local helper cannot protect against process crash before
  `ResolveTool` returns
- such a crash leaves `ToolCallResolving` without `ToolCallStarted`
- closing that ambiguity for side-effecting tools requires final recovery metadata
  before execution begins

Likely future shape:

```go
plan, err := resolver.PrepareTool(ctx, call, handlerLoadData)
// thread durably appends ToolCallStarted using plan metadata
items, err := plan.Execute(ctx)
// thread durably appends result items
```

In that shape, helper code can recover panics from `Execute` after the started
marker is already durable.

## Common Patterns

### Pure Synchronous Tool

Use this for deterministic work where rerunning is acceptable.

```go
return threads.ToolDispatch{
    Started:  true,
    Recovery: threads.ToolRecoverySafe,
    Items: []threads.Item{
        threads.ToolCallResult{CallID: call.CallID, Output: result},
    },
}, nil
```

Why:

- execution really happened
- retrying is safe if the process dies before the result is durable
- automatic continuation is appropriate by default

### Side-Effecting Synchronous Tool

Use this for writes, sends, mutations, or external actions.

```go
return threads.ToolDispatch{
    Started:  true,
    Recovery: threads.ToolRecoveryUnsafe,
    Items: []threads.Item{
        threads.ToolCallResult{CallID: call.CallID, Output: output},
    },
}, nil
```

Why:

- execution really happened
- retrying may duplicate effects
- recovery should not blindly rerun this call

### Manual Continuation

Use this when the application should pause after the tool result.

```go
return threads.ToolDispatch{
    Started:  true,
    Recovery: threads.ToolRecoveryUnsafe,
    Continue: threads.ToolContinueManual,
    Items: []threads.Item{
        threads.ToolCallResult{CallID: call.CallID, Output: output},
    },
}, nil
```

Why:

- the result is durable
- the model should not automatically continue
- the application can queue `SendItem{}` later if continuation is desired

### No Execution Happened

Use this when the resolver cannot or should not execute the tool.

```go
return threads.ToolDispatch{}, err
```

Why:

- no `ToolCallStarted` is recorded
- recovery can see resolver code was entered but no dispatch started
- the application can decide whether to retry, report, or replace the resolver

### In-Flight Or Asynchronous Work

Current support is limited. A resolver can return `Started: true` with no result
items, leaving the call outstanding:

```go
return threads.ToolDispatch{
    Started:  true,
    Recovery: threads.ToolRecoveryUnsafe,
}, nil
```

Why:

- the thread can durably know the tool started
- future recovery can fail closed or apply a started-tool policy

Caveat:

- there is not yet a complete public async completion or late-result policy
- consumers should avoid relying on this as a finished async API

If an external completion later queues a matching `ToolCallResult` through
`Thread.QueueItem`, the thread uses the `Continue` mode persisted on
`ToolCallStarted`: automatic continuation queues a follow-up `SendItem{}` when no
send is already pending, while `ToolContinueManual` records the result without
continuing. This is a compatibility path for simple late completions, not a full
async safety API.

Safe async completion still needs more than appending `ToolCallResult` manually
later.

A safe async API should provide a completion handle after the start marker is
durable:

```go
run, err := thread.StartToolCall(callID, ToolStartOptions{
    Continue: threads.ToolContinueAuto,
    Recovery: threads.ToolRecoveryUnsafe,
})

go func() {
    result, err := doWork()
    if err != nil {
        _ = run.Fail(err)
        return
    }
    _ = run.Complete(threads.ToolCallResult{CallID: callID, Output: result})
}()
```

The exact API may differ, but it needs these properties:

- `StartToolCall` durably appends `ToolCallStarted` before the async work begins
- the returned handle identifies the specific started attempt
- `Complete` validates that the call is still outstanding and matches the handle
- `Complete` appends result items durably
- `Complete` preserves the original automatic/manual continuation mode
- late completions after recovery cancellation or rerun are rejected or recorded by
  an explicit late-result policy
- thread mutation is serialized so async goroutines do not race with normal thread
  execution

Without such a handle, manually calling `QueueItem(ToolCallResult{...})` from an
async worker is not a safe public pattern. It bypasses validation, continuation
policy, late-result policy, and thread serialization.

## Anti-Patterns

- performing side effects before recovery can durably know the tool started
- returning `Started: false` for a tool that executed real work
- marking a dispatch `safe` only because retry is convenient
- using automatic continuation when user approval or application orchestration is
  required
- returning a result for the wrong `CallID`
- treating tool-name-level safety as enough when safety depends on payload or
  handler configuration

## Recovery Summary

Recovery can safely distinguish:

- `ToolCall` without `ToolCallResolving`
  - requested but resolver was not entered
  - safe to re-run dispatch selection or drop interrupted stream output
- `ToolCallResolving` without `ToolCallStarted` or result
  - resolver was entered but did not produce a durable dispatch/result
  - ambiguous; recovery should fail closed by default
- `ToolCallStarted` without result
  - started but unfinished
  - requires started-tool recovery policy
- resultable item with matching call id
  - completed
  - no longer outstanding

The consumer's job is to ensure those durable markers accurately describe what
actually happened.

## Implementation Audit Notes (2026-05-13)

- Async support has moved beyond the "not yet a complete public async
  completion" wording. Current code includes `EventLoop.StartToolResult` and
  `Thread.ReturnAsyncToolItem`; they serialize late returns through the
  `EventLoop`, validate that a call is still started and incomplete, preserve the
  persisted continuation mode for automatic sends, and drop ordinary late
  results after a recovered result exists. A richer attempt handle, explicit
  failure API, and conflict/audit policy are still future work. Decision: doc
  update plus future for the richer handle API.
- Recovery policy support is more limited than the target descriptions imply.
  The zero-value policy fails closed, and `ToolCallRecoveryCancelAll` can append
  recovered status results for outstanding resolving/started calls. The
  `run_safe` and `cancel_unsafe` policy constants currently panic as
  unimplemented. Decision: doc update plus future for those modes.
- Automatic continuation has two important implementation details not captured
  above: when a send is already pending, tool-resolution items are inserted
  before that send instead of always queuing a new one; and streamers that report
  `ToolResultSendRequiresComplete` can place the thread in
  `awaiting_tool_results` until all pending tool calls have results. Decision:
  doc update.
