# Executor Recovery And Tool-Call Semantics

This note describes the intended recovery model for `threads` once tool calls,
durability, and model migration are all considered together.

It now reflects the current codebase state as of March 2026. Some prerequisites
from the original note have landed; the attach-time recovery model itself has
not.

## Summary

The target design still stands:

- durable restore should reconstruct the exact persisted thread history and control-block state
- restore should not itself resume execution
- executor attachment should become the public recovery hook

What has already landed since the first draft:

- durable `ToolsSnapshot` persistence, including per-tool handler load data
- `ToolDispatch`, including:
  - `Started`
  - `Continue`
  - returned thread items
- durable `ToolCallStarted`
- control-block derivation of outstanding tool calls using the nearest preceding
  `ToolsSnapshot`
- synchronous tool resolution on `Thread` from the derived outstanding-call view

What is still missing:

- `SetExecutor(...)` attach/recovery semantics
- recovery policy selection and application
- rollback operations for retained thread history/state
- durable per-call recovery metadata chosen by dispatch
- streamer capability reporting such as `AssistantPrefix`
- live cancellation when replacing an executor on an active thread
- recovery behavior for started-but-unfinished tools
- a late-result policy for tool work that finishes after recovery has already
  canceled that call

Important current mismatch with the target design:

- `SetExecutor(...)` only assigns the executor field today
- `RestoreFromCheckpointAndWAL(...)` still trims an unsafe inflight tail by default
- outstanding started tool calls are detectable but are not retried, canceled, or
  converted into recovery results
- `ToolDispatch` currently returns no durable recovery metadata for the started call

## Current Model

Today the relevant pieces are:

- `Thread`
  - owns the append-only thread history
  - owns the derived `controlBlock`
  - owns durability state such as checkpoint and WAL
  - owns the current synchronous tool-resolution loop
- `controlBlock`
  - tracks `IP`
  - tracks thread state:
    - `idle`
    - `construct_llm_request`
    - `receiving_stream`
    - `stream_complete`
  - derives outstanding tool calls from thread history
- `ThreadExecutor`
  - watches control-block state transitions
  - when the thread enters `construct_llm_request`, it builds a request and streams it
  - does not currently own tool dispatch or recovery policy
- `ToolProvider`
  - supplies a `ToolsSnapshot`
- `ToolResolver`
  - resolves `(tool call, opaque handler load data)` to a `ToolDispatch`
- `ToolDispatch`
  - reports whether execution has started durably
  - reports whether follow-up continuation is auto or manual
  - returns thread items to append
- `ToolsSnapshot`
  - durable control item in thread history
  - contains model-facing `ToolOfferSnapshot`
  - contains per-tool opaque handler load data

The current restore behavior is still too low-level for full recovery semantics.
It now preserves enough information to distinguish requested vs started tool
calls, but restore still does not answer:

- should outstanding tools be rerun?
- should they be canceled?
- can the current model continue from partial assistant output?
- can this thread be moved to another model after restore?

## Design Goals

- durable restore should preserve exact thread state and history
- restore should not perform execution by itself
- `SetExecutor(...)` should become the public recovery hook
- switching executors mid-request should be semantically equivalent to canceling
  the current execution and attaching a new executor
- recovery should be policy-driven
- tool recovery must distinguish:
  - tool call requested but not started
  - tool execution started
  - tool result durably recorded
- recovery should preserve existing continuation semantics for tool dispatches
- model migration should be possible when the new streamer supports the needed
  request format

## Non-Goals

- this note does not define the final rich `tool` package API
- this note does not require async tool execution yet
- this note does not require exact wire-level provider stream resumption; it only
  defines how restored thread state is recovered and what the next request may look like

## Why Recovery Belongs On The Executor

The durable layer cannot decide correct recovery on its own because the answer
depends on executor capabilities.

Example:

- a thread is restored in `receiving_stream`
- the thread history already contains partial assistant text
- a new executor is attached using a different model provider

Whether that restored thread history can continue without rollback depends on whether the
new provider can accept assistant-prefixed transcript. That is a model/executor
question, not a durability question.

The same is true for tool calls:

- a persisted `ToolCall` with no result may be safe to rerun
- or it may be side-effecting and must not be rerun
- or policy may say "cancel all outstanding tools" even for safe tools
- or a prior `ToolDispatch` may have requested manual rather than automatic
  follow-up continuation
- or safety may depend on the specific call/config rather than the public tool name

Those are execution semantics, not storage semantics.

## Public Semantics Of `SetExecutor`

`SetExecutor` should become the public attach and recovery hook.

Desired semantics:

- `SetExecutor(nil)`
  - detach the current executor
  - do not mutate thread history
- `SetExecutor(x)` when the thread is `idle`
  - install `x`
  - do nothing else
- `SetExecutor(x)` when the thread is not `idle`
  - treat the current execution as interrupted
  - if there is an existing live executor, cancel its active request/tool work
  - install `x`
  - ask `x` to choose a recovery policy for the current thread history and control-block state
  - apply that policy through a recovery API on `Thread`

This gives one coherent meaning to:

- restore and then attach an executor
- switch models mid-request
- shutdown and restart

Current status:

- not implemented
- today `SetExecutor` only stores the executor reference
- attaching an executor to a restored inflight thread does not inspect state,
  choose policy, or resume anything
- replacing an executor on a live thread does not cancel the prior request

### Internal Note

Low-level restore and WAL replay should continue to avoid running executor logic.
That part is already true today: replay suppresses executor and delegate callbacks.

What is still missing is moving recovery decisions out of restore defaults and into
executor attachment.

## `Thread.ApplyRecoveryPolicy(...)`

The executor should not have arbitrary rights to rewrite the thread.

Instead:

- the executor chooses a recovery policy
- `threads` applies that policy

That keeps structural mutation rules owned by `threads` while still letting the
executor make model-specific decisions.

Conceptually:

```go
func (t *Thread) ApplyRecoveryPolicy(policy RecoveryPolicy) error
```

The exact API can differ, but the important design rule is:

- executor decides
- thread mutates itself through narrow recovery operations

Current status:

- not implemented
- there is no recovery policy type
- there is no rollback or cancellation-result API on `Thread`

## Streamer Capability Boundary

Recovery depends on what request shapes the attached streamer can support.

The most important capability is:

- `AssistantPrefix`
  - `true` means the streamer can send a request whose history already ends with
    assistant output and continue from there
  - `false` means the executor must roll back to a boundary that avoids sending
    assistant-prefixed continuation

This is the capability that matters for "restore inflight and move to another model".

Current status:

- missing
- `LLMStreamer` currently exposes only `StreamReq(...)`
- there is no capability-reporting surface today

## Tool Recovery Model

Tool recovery must reason about what has happened durably, not just what is
"pending" in a loose sense.

The thread can already distinguish these states for each tool call:

1. `requested`
   - `ToolCall` exists in thread history
   - no `ToolCallStarted`
   - no result
   - the tool has definitely not begun execution

2. `started`
   - `ToolCallStarted` exists
   - no result
   - the tool may have executed partially or fully

3. `completed`
   - some `ToolCallResultable` exists
   - no longer inflight

The important crash-recovery distinction is between `requested` and `started`.

There is one more piece of durable state now that recovery will likely need to
preserve:

- `Continue`
  - persisted on `ToolCallStarted`
  - records whether the original dispatch expected automatic or manual follow-up
  - recovery should not silently convert manual continuation into auto continuation

### `ToolCallStarted`

`ToolCallStarted` is now a durable thread item.

Current behavior:

- it is appended only when a `ToolDispatch` reports `Started: true`
- that means the runtime found a concrete dispatch path before the marker was written
- if the process dies before dispatch resolution, the tool is still definitively
  "not started"

The older wording that "the executor" writes this marker is now stale. In the
current codebase the marker is written by the synchronous tool-resolution path on `Thread`,
not by `ThreadExecutor` directly. The underlying semantic rule is still the same.

## Dispatch-Owned Recovery Metadata

Recovery still needs durable metadata describing whether a concrete dispatch
attempt is safe to retry.

The original draft put this on the tool binding. That now looks too coarse.

Why binding-level safety is not enough:

- the same public tool may be safe for some calls and unsafe for others
- the same handler may be side-effect free for one config and side-effecting for another
- dispatch may make a per-call decision after inspecting:
  - tool payload
  - handler load data
  - runtime mode
  - user-selected options

The better home is the dispatch boundary for a concrete call.

At minimum, dispatch should be able to classify the call as:

- `safe`
  - replaying the call after interruption is acceptable
  - examples: pure calculation, deterministic formatting, read-only queries with
    acceptable duplication
- `unsafe`
  - replaying the call may cause duplicate side effects or otherwise violate
    semantics
  - examples: writing a file, sending email, mutating external systems

This metadata must be durably associated with the started call visible to the
recovered thread. It cannot live only in process memory.

Likely home:

- `ToolDispatch`
- durably copied onto `ToolCallStarted`

Current status:

- missing
- `ToolDispatch` currently has no recovery metadata fields
- `ToolCallStarted` currently persists only:
  - call id
  - continuation mode

### Consequence For Requested Calls

If a call is still only `requested`, there is no durable started marker and
therefore no durable dispatch-owned recovery metadata yet.

That is acceptable, but it means recovery semantics become:

- `requested` calls are allowed to rerun dispatch selection/classification because
  the runtime knows they did not start
- `started` calls must rely on the metadata already persisted on `ToolCallStarted`

### Contract Requirement

If dispatch owns recovery metadata, then it must decide that metadata before
side effects begin.

Otherwise a crash could still leave thread history in the ambiguous state:

- no `ToolCallStarted`
- no persisted safety classification
- side effects may already have happened

So one of these must become true:

- `ResolveTool(...)` becomes a side-effect-free planning step that returns durable
  recovery metadata before execution begins
- or the tool-dispatch API splits into:
  - prepare/classify
  - durably append `ToolCallStarted`
  - execute

Without that sequencing guarantee, moving safety onto `ToolDispatch` does not
fully solve crash recovery.

## Outstanding Tool Call Derivation

This phase is now mostly landed.

The control block already derives outstanding tool calls and resolves them against
the nearest preceding `ToolsSnapshot`.

Current internal shape:

```go
type pendingToolCall struct {
    call    ToolCall
    load    json.RawMessage
    started bool
    bound   bool
}

func (cb *controlBlock) pendingToolCalls(items cbItems) []pendingToolCall
```

What this already gives us:

- which tool calls are currently outstanding
- whether each call is requested vs started
- the nearest bound handler load data
- whether the call is currently bound at all

What is still missing from this derived view for recovery:

- durable per-call recovery metadata for started calls
- continuation mode in the derived view
- an explicit recovery-facing API if attach-time recovery should live outside
  `thread.go`

The exact API shape can still change. The important capability now exists.

### Binding Resolution Rule

Outstanding tool calls should resolve their handler metadata from the nearest
preceding `ToolsSnapshot`, using the same rule already used for actual execution.

That means catalog switching stays correct:

- old calls keep old handler load data
- new calls see the new snapshot

This part is already implemented.

## Recovery Policies

Recovery policy should have two axes:

1. rollback boundary
2. handling of outstanding tool calls in the retained thread history

### Rollback Boundary

The useful boundaries are:

- `Exact`
  - keep the restored thread history and current control-block state as-is
- `LastAssistantResponse`
  - roll back any trailing partial assistant response state
  - useful when the new streamer cannot continue from assistant-prefixed history
- `LastIdle`
  - roll back to the last fully settled idle boundary

Current status:

- not implemented
- today there is no rollback API at all
- restore-time trimming in `RestoreFromCheckpointAndWAL(...)` is a coarse durability
  default, not a recovery-policy implementation

### Outstanding Tool Call Handling

The useful modes are:

- `RunSafe`
  - outstanding `requested` calls may be classified and then run if dispatch says `safe`
  - outstanding safe `started` calls may be retried
  - the presence of any outstanding unsafe call, whether discovered during
    requested-call classification or already persisted on a started call, means
    exact recovery is not allowed under this mode
  - the executor must roll back further or fail
- `CancelUnsafe`
  - safe outstanding calls may run
  - unsafe outstanding calls are converted into canonical cancellation results
  - `requested` unsafe calls can report `execution_status = "not_started"`
  - `started` unsafe calls must report `execution_status = "unknown"`
- `CancelAll`
  - all outstanding calls are converted into canonical cancellation results

Current status:

- not implemented
- current tool resolution only runs `requested` calls
- current tool resolution always skips `started` calls
- there is no cancellation-result path for either case

## Recovery Result Items

Recovery sometimes needs to tell the LLM that a tool call could not be completed
normally.

This should still be represented by normal thread items, not by out-of-band executor
state.

### Unknown Prior Execution

There is still a real semantic need for the ambiguous case:

- a `ToolCallStarted` exists
- no durable tool result exists
- it is unknown whether the tool finished or caused side effects before interruption

The runtime needs a canonical result shape that tells the model:

- the requested tool call is no longer going to be executed by the runtime
- the runtime cannot guarantee whether the earlier execution already happened
- the model should reason about the safest next step

Suggested structured data:

```json
{
  "status": "unknown_recovery",
  "execution_status": "unknown",
  "side_effects_status": "unknown"
}
```

### Definite Non-Execution

If no `ToolCallStarted` exists, the runtime knows the tool did not begin.

That case can use a normal tool result payload that clearly communicates:

- the call was not executed
- no side effects occurred

Suggested structured data:

```json
{
  "status": "canceled_recovery",
  "execution_status": "not_started",
  "side_effects_status": "none"
}
```

### Current Durability Constraint

The older note proposed a dedicated `ToolUnknownRecoveryResult` item type. That is
not obviously required anymore.

Current durability and request building already canonicalize every
`ToolCallResultable` to a generic `ToolCallResult` shape on snapshot/WAL restore.

That means there are two viable approaches:

- simplest:
  - emit ordinary `ToolCallResult` values with structured `Data`
  - rely on the structured data contract rather than concrete Go type identity
- richer:
  - introduce dedicated in-memory result types for convenience or UI purposes
  - accept that snapshot/WAL formats must widen if concrete type identity needs to
    survive restore

For recovery semantics, the structured result contract matters more than the
concrete item type.

## Late Tool Results After Recovery Cancellation

Recovery also needs a rule for this race:

- recovery decides an outstanding tool call will not be allowed to complete normally
- the thread durably appends a recovery cancellation/unknown result for that call
- later, a real tool result from the pre-recovery execution still arrives

This is primarily a live executor-replacement problem. A pure restore-then-attach
flow has no still-running prior executor, so no late result can arrive from the
old process. But if "replace executor" is meant to become equivalent to
"interrupt, then recover", the design still needs to say what happens when the
interruption is imperfect and an old tool completes anyway.

Important semantic rule:

- once recovery has durably canceled a call, a later ordinary tool result for the
  same call id cannot be silently appended as if nothing happened

That would produce contradictory durable history:

- the thread history would first say "this call was canceled/unknown during recovery"
- then later say "this call completed normally"

The final policy is still open, but it must be explicit. Plausible options are:

- drop the late result
- reject it as a runtime error
- record it only as conflict/audit metadata outside the normal tool-result flow

What matters for this note is the requirement, not the chosen mechanism:

- recovery cancellation is not the end of the problem unless late completions are
  also handled

## Executor Attach Algorithm

This section describes the target attach algorithm. It is not implemented today.

When `SetExecutor(x)` attaches a non-nil executor to a non-idle thread:

1. cancel any old live execution if one exists
2. inspect the current control-block state
3. ask the control block for outstanding tool calls and their status
4. inspect the executor's recovery policy
5. inspect streamer capabilities
6. choose a rollback boundary if needed
7. after rollback, re-read outstanding tool calls from the retained thread history
8. call `Thread.ApplyRecoveryPolicy(...)`
9. during policy application:
   - classify requested calls before starting them
   - run eligible calls
   - append unknown-recovery results for ambiguous started calls when policy says
     to cancel them rather than rerun them
   - append ordinary non-execution results for calls known not to have started
10. if the retained state is still ready to request the model, continue execution

The key design point is that the decision is made on attach, not on restore.

## Behavior By Control-Block State

These are target recovery semantics, not current behavior.

### `idle`

- attaching an executor does not need recovery
- if thread history contains no outstanding tool calls, nothing happens
- if a future state model allows `idle` with outstanding tool calls, the attached
  executor may still need to apply tool-call recovery policy

### `construct_llm_request`

- the next action is usually to build and send a request
- if policy rolls back further, the send may be dropped
- if policy keeps exact state, the attached executor can send using the retained thread history

### `receiving_stream`

- exact recovery is only possible if the streamer can support the retained transcript
- if `AssistantPrefix` is false, exact recovery must roll back to
  `LastAssistantResponse` or `LastIdle`
- after rollback, tool recovery policy still applies

### `stream_complete`

- semantically this is post-stream but not yet fully settled
- recovery may remain local to the thread and not require provider support
- after settling, the executor may need to run or cancel outstanding tool calls

Current caveat:

- `stream_complete` is transient in practice today because `endStreaming()` moves
  directly to `idle`

## Model Migration

This design intentionally supports:

- shutting down mid-request
- restoring later
- attaching a different executor using a different model provider

Whether migration succeeds depends on capabilities:

- if the new provider supports assistant-prefixed continuation, exact recovery may work
- if not, rollback to `LastAssistantResponse` or `LastIdle` may be required

This is why recovery belongs to the executor and not to durability.

Current status:

- still design work
- there is no capability surface or attach-time recovery flow yet

## Examples

These examples describe target behavior after dispatch-owned recovery metadata and
recovery policy selection are implemented.

### Example 1: Safe Calculator Tool

Thread history contains:

- `ToolCall(calc, ...)`
- `ToolCallStarted(calc, ...)`
- no result yet

The dispatch persisted `safe` recovery metadata on `ToolCallStarted`.

If the process dies and the thread is restored, an attached executor using
`Exact + RunSafe` may rerun the calculator and continue.

### Example 2: Unsafe Write-File Tool

Tape contains:

- `ToolCall(write_file, ...)`
- `ToolCallStarted(write_file, ...)`
- no result yet

The dispatch persisted `unsafe` recovery metadata on `ToolCallStarted`.

If the process dies:

- `Exact + RunSafe` cannot continue this exactly; it must roll back or fail
- `Exact + CancelUnsafe` should append an unknown-recovery result and continue the
  thread from there
- `Exact + CancelAll` does the same

### Example 3: Tool Requested But Never Started

Tape contains:

- `ToolCall(write_file, ...)`
- no `ToolCallStarted`
- no result

Because no start marker exists, the executor knows the tool definitely did not run.

That means:

- running it is allowed only if requested-call classification says it is safe
- canceling it can report a normal non-execution result because the runtime knows
  the tool never started

### Example 4: Move To A Model Without Assistant Prefix Support

Thread is restored in `receiving_stream` with partial assistant text in thread history.

If the new executor's streamer does not support assistant-prefixed continuation:

- exact recovery is not legal
- the executor must roll back to `LastAssistantResponse` or `LastIdle`
- only after that should it apply tool-call recovery policy and continue

## Required New Concepts

The implementation now likely needs these additions:

- recovery metadata on `ToolDispatch`
- durable persistence of that recovery metadata on `ToolCallStarted`
- a recovery-facing outstanding-call view that includes any fields attach-time
  recovery needs beyond the current internal `pendingToolCalls(...)` shape
- `Thread.ApplyRecoveryPolicy(...)`
- rollback operations for retained thread history/control-block state
- executor recovery policy
- streamer capability reporting
- live-request cancellation support so replacing an executor on an active thread
  really behaves like interruption
- a late-result conflict policy for calls already canceled by recovery
- recovery-result conventions for:
  - unknown prior execution
  - definite non-execution

Notable items that are no longer "new concepts":

- durable `ToolCallStarted`
- durable `ToolsSnapshot` handler load data
- control-block derivation of outstanding tool calls and started status
- persisted tool-dispatch continuation mode

## Implementation Order

Recommended order from the current codebase:

1. done:
   - derive outstanding tool calls and started status from the control block
2. done:
   - persist `ToolCallStarted`
   - persist dispatch continuation mode
3. next:
   - add dispatch-owned recovery metadata
   - persist that metadata on `ToolCallStarted`
4. next:
   - define executor recovery policy and streamer capability surface
5. next:
   - add rollback and recovery-policy application on `Thread`
   - ensure dispatch classification happens before side effects
6. next:
   - move unsafe recovery decisions out of restore defaults and into executor attach
7. next:
   - add tests for:
     - restore then attach
     - switching executor mid-request
     - safe tool retry
     - unsafe tool cancel
     - cancel-all mode
     - late tool result after recovery cancellation
     - model migration with and without assistant-prefix capability

This keeps the already-landed prerequisites explicit while preserving the remaining
direction of the design.
