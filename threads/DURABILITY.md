# Threads Durability Notes

This note documents durability behavior in `github.com/mackross/agentloom/threads` so future changes keep restore/replay semantics stable.

## Safe vs Inflight

Thread states:

- `idle`
- `construct_llm_request`
- `receiving_stream`
- `stream_complete`

For durability, only `idle` is treated as a fully safe boundary.
Inflight means any of:

- `construct_llm_request`
- `receiving_stream`
- `stream_complete`

## Checkpoint Policies

`CheckpointOptions.Policy`:

- `skip` (default):
  - if inflight, return last safe idle checkpoint
  - if not inflight, snapshot current state
- `wait`:
  - block until state becomes safe (`idle`) or timeout
- `unsafe`:
  - snapshot current state even if inflight

## Restore Semantics

`RestoreCheckpoint`:

- rejects `Unsafe=true` checkpoints unless `RestoreOptions.AllowUnsafe=true`

`RestoreFromCheckpointAndWAL`:

- replays WAL events over checkpoint
- if `AllowUnsafe=false` and replay lands in a state that needs recovery, it trims trailing unsafe WAL tail by restoring only the longest replay prefix that does not need recovery
- recovery-required state includes inflight stream/request state and tool calls whose resolver had begun but whose terminal tool result was not durably recorded
- this is intentional to avoid loading into a frozen inflight state or into a pending side-effecting tool resolution without explicit recovery handling

## WAL Invariants

- `Seq` must be strictly contiguous increasing during replay (`prev + 1`)
- WAL operations currently used:
  - `queue_item`
  - `begin_stream`
  - `append_stream_item`
  - `end_stream`
- replay order is authoritative for derived thread state

## DurableStore Contract

Implementers of `DurableStore` are expected to:

- perform synchronous durable writes before returning
- panic on storage failure (fail-closed behavior)
- make `ReplaceSnapshot` atomically replace base snapshot and clear prior WAL tail for that base
- make `AppendWALDiff` append-only (no rewrite of existing WAL history)

## Branch Copying

`BranchManager` can open branch refs as durable or ephemeral copies with
`OpenAsDurableCopy` and `OpenAsEphemeralCopy`. Copy opens are read-only with
respect to the parent branch: the manager opens the parent without acquiring a
writer lease, snapshots either the branch head or the selected completed turn,
and creates a child branch from that materialized checkpoint. This allows a
caller to fork from a branch that is already open for writing elsewhere.

The returned child branch is a normal opened branch. Ephemeral children are
process-local and do not require a lease; durable children acquire their own
writer lease.

## Crash Window Expectations

Behavior after abrupt termination depends on the last persisted point:

- crash before inflight WAL completion:
  - replay may end with an unsafe suffix
  - default restore trims to last safe prefix
- crash after safe boundary:
  - restore is direct (no trim needed)
- crash after a tool-resolution marker but before its tool result:
  - default restore trims back to the latest prefix without an unresolved resolving/started tool marker
  - use unsafe restore plus `AttachExecutorForRecoveryWithOptions` when a caller wants to handle those ambiguous tool calls explicitly
- crash during snapshot replace:
  - file store uses temp-file + rename to minimize partial snapshot risk

## Coalescing and Persistence

- WAL stores logical operations, not coalescing deltas
- item coalescing is re-derived during replay by current CB logic
- current CB coalescing merges adjacent:
  - `UserText`
  - `AssistantText`

## File Format and Wire Keys

File layout in `threads/durability/filestore`:

- fixed 16-byte header:
  - magic `TDUR`
  - file version
  - flags (unsafe bit)
  - snapshot JSON length
  - checkpoint seq
- snapshot JSON payload
- WAL NDJSON payload

JSON key choices:

- snapshot: short but readable (`ver`, `state`, `items`, `ip`, `queue`, `stream`, item `kind`, `text`)
- WAL: compact keys (`s`, `o`, `i`)

Backward compatibility for file JSON schema is not guaranteed yet.

## Test Guidance

- Run normal coverage: `go test ./threads ./threads/durability`
- Run full suite: `go test ./...`
- Run strict roundtrip mode for thread tests: `THREAD_TEST_SERIALIZE_ROUNDTRIP=1 go test ./threads`

When changing durability behavior, add/adjust tests for:

- inflight checkpoint policy behavior
- restore trimming behavior
- WAL replay sequence constraints
- file-store snapshot/append semantics

## Implementation Audit Notes (2026-05-13)

- The state and safe-boundary discussion omits the implemented
  `awaiting_tool_results` state. Current code treats only
  `construct_llm_request`, `receiving_stream`, and `stream_complete` as
  inflight; `awaiting_tool_results` is not inflight and can be captured as a
  restorable safe snapshot, though `requiresRecovery` still returns true if it
  contains unresolved resolving/started tool markers. Decision: doc update.
- `Checkpoint.Unsafe` only reflects inflight stream/request state today; it does
  not by itself mark unresolved tool recovery markers. `RestoreCheckpoint` only
  rejects checkpoints whose `Unsafe` bit is set, while
  `RestoreFromCheckpointAndWAL` trims a recovery-required WAL suffix but cannot
  trim recovery-required content already present in the base checkpoint.
  Decision: doc update.
- The checkpoint `wait` policy waits only for inflight states. It does not wait
  for `awaiting_tool_results` or for outstanding tool completion unless the
  thread is also in an inflight state. Decision: doc update.
- The WAL operation list is missing `queue_item_before_send`, which is used when
  tool-resolution items need to be inserted before an already-pending send.
  Decision: doc update.
- The file layout path is stale. The current file store is the
  `threads/durability` package implemented in `filestore.go`; there is no
  `threads/durability/filestore` subpackage. Decision: doc update.
- The JSON key description is stale. Current snapshot and WAL encoding uses the
  exported Go field names (`Version`, `State`, `Items`, `Seq`, `Op`, `Item`,
  etc.) because the durability structs do not define compact JSON tags such as
  `ver`, `kind`, `s`, `o`, or `i`. Decision: doc update.
