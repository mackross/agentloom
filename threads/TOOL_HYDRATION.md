# Tool Hydration Design

This note explains the intended hydration flow for thread tools.
It is written for someone who has not read the codebase before.

The design has two layers:

- `threads`: owns durable thread state and the minimum tool-routing boundary.
- `threads/tool`: owns richer helper APIs such as catalogs and, later, a registry.

The key rule is that `threads` should not need to know helper-specific concepts such as registry keys, versions, or catalog internals. It should only persist opaque data and hand that data back to a resolver when tool execution needs to be rehydrated.

## Current Boundary

Today `threads` exposes these concepts:

- `ToolProvider`
  - returns a `ToolsSnapshot`
- `ToolResolver`
  - resolves `(tool name, opaque handler load data)` into a handler function
- `ToolsSnapshot`
  - the control item queued onto the thread
  - contains:
    - `Snapshot ToolOfferSnapshot`
    - `Handlers []ToolHandlerBinding`
- `ToolOfferSnapshot`
  - model-facing tool state only
  - currently `Offered`, `Allowed`, and `Parallel`
- `ToolHandlerBinding`
  - keyed by tool name
  - contains opaque `HandlerLoadData`
- `ToolCall`
  - stores the tool name and raw payload the model sent

Important current behavior:

- `Thread.SetToolProvider(...)` queues a `ToolsSnapshot` control item on the thread.
- request construction reads only `ToolsSnapshot.Snapshot`.
- durability persists the full `ToolsSnapshot`, including `Handlers`.
- helper execution hydration is not fully wired yet, but the durable boundary is now in place.

## Why The Boundary Looks Like This

We need to support all of these cases:

- tools survive process restart
- a thread can switch to a different catalog later
- old tool calls still resolve the same way they would have before the switch
- `threads` does not become coupled to helper implementation details
- users can still bypass the helper layer and handle tools via delegates later

That leads to three design choices:

1. `ToolOfferSnapshot` stays model-facing only.
2. handler rehydration data is stored separately in `ToolsSnapshot.Handlers`.
3. `threads` treats `HandlerLoadData` as opaque bytes.

## Intended Runtime Flow

The intended helper-backed flow is:

1. application startup builds a tool registry
2. the registry knows how to rebuild tool handlers from durable data
3. a catalog chooses which tools are exposed to the model and with what policy
4. the catalog produces a `ToolsSnapshot`
5. the thread queues that `ToolsSnapshot`
6. the model sees only the `ToolOfferSnapshot`
7. the model emits a `ToolCall`
8. tool execution looks up the nearest preceding `ToolsSnapshot`
9. execution finds the matching `ToolHandlerBinding` by tool name
10. execution asks the `ToolResolver` to hydrate a handler from `(name, HandlerLoadData)`
11. the hydrated handler runs using the raw `ToolCall.Payload`

After restart the same flow should work again because the `ToolsSnapshot` was persisted with the thread.

## The Nearest-Preceding-Snapshot Rule

Tool calls are interpreted in the context of the nearest preceding `ToolsSnapshot` in the thread history.

This matters because catalogs can change over time.

Example:

1. thread mounts catalog A
2. model emits `write_file`
3. thread later mounts catalog B
4. old `write_file` calls must still resolve using catalog A's handler load data
5. new `write_file` calls should resolve using catalog B's handler load data

This rule avoids any need for `threads` to track a global "current catalog id".
The thread history already records what we need.

## Proposed Helper Architecture

The likely helper shape is:

- `tool.Registry`
  - durable implementation registry
  - knows how to hydrate handlers from opaque data
  - should implement `threads.ToolResolver`
- `tool.Catalog`
  - model/tool helper for building offered tools
  - should be able to produce a `threads.ToolsSnapshot`
  - should implement `threads.ToolProvider`, or be wrapped by a provider

One likely division of responsibility:

- registry owns durable function identity and decode logic
- catalog owns:
  - which tools are offered
  - descriptions and payload schema
  - allow/disallow policy
  - parallel policy
  - per-tool opaque handler load data

In other words:

- the registry knows how to rebuild behavior
- the catalog knows what the model sees and which behavior each exposed tool should use

## What Goes In HandlerLoadData

`HandlerLoadData` is an opaque helper concern.

The expected shape is something like:

- function identity
- version
- durable config for that specific tool binding

Example:

```json
{
  "function": "tool/write-file@v1",
  "filename": "notes.txt"
}
```

Another example with different behavior under the same public tool name:

```json
{
  "function": "tool/write-file/atomic@v1",
  "filename": "notes.txt"
}
```

`threads` should not interpret any of this.

## Why Tool Calls Store Raw Payload

`ToolCall` stores the raw payload emitted by the model.

That is intentional.

The payload should be decoded by the hydrated handler chosen from the active `ToolsSnapshot`.
We should not attempt to reconstruct call arguments from the current tool definition, because:

- catalogs can switch
- implementations can version
- persisted tool calls may outlive process restarts and deploys

The raw tool call payload is the source of truth for the call itself.

## Example Helper Usage

The exact helper API is still open, but the intended flow looks roughly like this:

```go
reg := tool.NewRegistry()

reg.Register("tool/write-file@v1", func(cfg WriteFileConfig) tool.Handler {
	return func(ctx context.Context, call tool.Call) threads.Item {
		// decode call payload here and write to cfg.Filename
	}
})

cat := tool.NewCatalog().
	AddBound(
		tool.Spec{
			Name:        "write_file",
			Description: "Write contents to the configured file",
			Payload:     tool.PayloadFor[WriteFileArgs](),
		},
		json.RawMessage(`{"function":"tool/write-file@v1","filename":"notes.txt"}`),
	).
	AllowOnly("write_file")

thread.SetToolResolver(reg)
thread.SetToolProvider(cat)
```

Later, the helper package may hide the two setter calls behind a single helper so application code does not need to wire them separately.

## Catalog Switching

Switching catalog behavior should mean queueing a new `ToolsSnapshot`.

It should not mean mutating old handler load data in place.

Example:

1. mount a `write_file` tool that uses `tool/write-file@v1`
2. later mount a `write_file` tool that uses `tool/write-file/atomic@v1`
3. both may share the same public tool name
4. old calls still resolve via the old preceding snapshot
5. new calls resolve via the new preceding snapshot

This is the core reason the handler load data lives in the thread history.

## Restart / Reload Flow

On restart:

1. durable store restores the thread snapshot and WAL
2. restored thread history still contains `ToolsSnapshot` items
3. future tool execution scans left from the call to the nearest preceding `ToolsSnapshot`
4. the matching `ToolHandlerBinding` provides `HandlerLoadData`
5. the registry rehydrates the handler from that data
6. the handler executes the persisted raw `ToolCall.Payload`

No current "live catalog object" is needed to understand old calls after restore.

## Failure Modes

These cases should fail loudly:

- there is no preceding `ToolsSnapshot` for a helper-routed tool call
- the preceding snapshot has no `ToolHandlerBinding` for the tool name
- the resolver does not recognize the requested handler load data
- handler load data cannot be decoded
- the rehydrated handler returns an invalid result

Silent fallback to "whatever tool is currently mounted" should be avoided.

## Invariants

- `ToolOfferSnapshot` is model-facing only.
- `ToolsSnapshot` is the durable control item.
- `HandlerLoadData` is opaque to `threads`.
- a helper-routed tool call resolves against the nearest preceding `ToolsSnapshot`.
- switching behavior means queueing a new `ToolsSnapshot`.
- old snapshots must remain available for any retained tool calls that still depend on them.
- if behavior cannot be rebuilt from durable data, it should use delegates rather than the helper hydration path.

## Where `simpletool` Fits

`threads/simpletool` exists only for very small adapters and tests.

It is not the long-term rich tool API.

If a caller needs:

- richer tool definitions
- reusable catalogs
- versioned handler hydration
- durable registry-backed behavior

they should use the `tool` package instead.

## Implementation Audit Notes (2026-05-13)

- The `ToolResolver` description is stale where it says the resolver turns
  `(tool name, opaque handler load data)` into a handler function. The current
  core signature is `ResolveTool(context.Context, ToolCall, json.RawMessage)
  (ToolDispatch, error)`: the resolver receives the whole call plus opaque load
  data and may execute/dispatch directly. Decision: doc update.
- The "helper execution hydration is not fully wired yet" sentence now needs a
  narrower scope. Core `threads` execution is wired: `Thread` derives
  outstanding calls, finds the nearest preceding `ToolsSnapshot`, reads the
  matching `ToolHandlerBinding`, and calls the installed `ToolResolver`. What is
  still missing is durable registry-backed hydration in the `threads/tool`
  helper package. Decision: doc update.
- The proposed `tool.Registry`, `Catalog.AddBound`, and `tool.NewRegistry`
  example APIs are not implemented. Current `threads/tool.Catalog` stores
  in-memory handlers and exposes `Snapshot`, `LoadTool`, and `Dispatch`, but it
  does not implement `threads.ToolProvider`, does not produce durable
  `ToolHandlerBinding` load data, and does not implement `threads.ToolResolver`.
  Decision: future, with a doc update to avoid presenting the example as current
  API.
- The final guidance overstates today's `threads/tool` package for durable
  registry-backed behavior. Use it today for catalogs, payload helpers, typed
  JSON handlers, and result helpers; durable registry hydration remains future
  work. Decision: doc update plus future.
