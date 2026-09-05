# Item sequence, conversation identity, and item metadata plan

## Goal

Introduce a small, branch-local sequence identity for materialized thread items, use that identity in the completed-conversation/branching API, and replace positional `PreviousItemMetadata` nodes with metadata that is stored on an item and can be patched later by item sequence.

This change intentionally does **not** add context pruning, `BuildWithContext`, summaries, provenance/replacement metadata, context revisions, paging, or any other committed context-management policy. It only puts the identity and generic metadata mechanics in place so a later context API has a sound foundation.

Migration and source compatibility may be broken. Prefer a clean v2 representation over compatibility aliases or inferred IDs.

## Why this foundation is being added now

This work is foundational for a future context-management path between the
durable thread history and construction of an LLM request:

```text
durable thread history
        │
        ▼
sequence-aware conversation/log view
        │
        ├── turn projection and branching
        └── BuildWithContext
              - selection and pruning
              - summaries
              - token budgeting
              - provider request construction
```

The intended direction is for context construction to consume a standardized
conversation/log adapter instead of treating the current linked list or a
fully-materialized `[]Item` as the permanent API. The first implementation can
still wrap the in-memory list, but the abstraction should eventually permit a
store-backed implementation to scan or page durable history. Keeping that
option open is important because retaining the complete history in memory
should not be a requirement of future context management.

A rough, deliberately **non-committed** shape is:

```go
type ConversationItem struct {
    Seq      ItemSeq
    Item     Item
    Metadata map[string]any
}

type ConversationReader interface {
    // Revision identifies the event/WAL state observed by this view.
    Revision() uint32

    // Scan visits transcript-ordered items through a stable boundary.
    // A future implementation may load pages while scanning.
    Scan(
        ctx context.Context,
        through ItemSeq,
        yield func(ConversationItem) error,
    ) error

    // Get supports policies that need to follow an explicit item reference.
    Get(ctx context.Context, seq ItemSeq) (ConversationItem, error)
}

type BuildContextInput struct {
    Conversation ConversationReader
    Through      ItemSeq
    Revision     uint32
    Capabilities StreamerCapabilities
}

type ContextBuilder interface {
    BuildWithContext(
        ctx context.Context,
        input BuildContextInput,
    ) (Req, error)
}
```

The names, exact methods, return type, snapshot semantics, and ownership of
`BuildWithContext` are not decisions made by this plan. In particular, a later
design may make the builder return a context selection before constructing
`Req`, or may separate model-backed summarization from request lowering. The
example only records the architectural requirement: context policy should see
stable, metadata-bearing, transcript-ordered items through an abstraction that
does not require all history to be resident.

`ItemSeq` and canonical per-item metadata are being established first because
they are the durable vocabulary that this future adapter and builder will need:

- `ItemSeq` gives selection, pruning, summary, and provenance decisions stable
  references despite coalescing, stream finalization, insertion, replay, and
  branching.
- Per-item metadata lets request/context logic inspect annotations without
  depending on positional sidecar nodes.
- `PatchItemMetadata` allows information discovered later to update an earlier
  item by identity.
- Atomic item-plus-metadata posting prevents initial annotations from being
  separated from their item by a crash.
- Persisting these primitives in snapshots and WAL means future context
  revisions or summary relationships will not require another item-identity
  migration.
- Branch-local identities leave room for shared or paged durable storage while
  keeping references compact.

This is also the point at which to replace the existing conversation-turn API
with a simpler projection over those identities. Today `Turn` points back to a
live thread, carries a raw list index, becomes stale on any mutation, and owns
checkpoint construction. Branch targets then identify that projection by a
mutable turn index. That model is unsuitable as the lower-level input to
context management and duplicates history-boundary logic.

The simpler model in this plan makes `Turn` a plain display/projection value
with a stable item-sequence identity and makes the thread/branch manager own
checkpoint materialization. Conceptually, a later conversation view can expose
both raw items and turns without making either own storage:

```go
conversation := thread.Conversation() // possible future API

conversation.Scan(ctx, stableThrough, func(item ConversationItem) error {
    // Inspect stable item identity and current metadata.
    return nil
})

turns := conversation.CompletedTurns() // projection over the same view
branch := manager.OpenTurn(ctx, branchID, turns[0].ID())
```

That API is illustrative, not committed here. This change only makes the
existing `CompletedTurns`/branching implementation use the same kind of stable
identity that a future conversation reader would expose. It avoids creating one
identity/boundary model for branching now and a second incompatible one for
`BuildWithContext` later.

The work intentionally stops at these primitives. It does not choose the
conversation-reader API, the final `BuildWithContext` signature, a pruning
algorithm, summary representation, token policy, context revision model, or
context-specific metadata schema. Those decisions should be made after the
underlying history has stable identity, patchable metadata, and simpler
branching boundaries.

## Chosen design

### Public identity and metadata types

Add a branch-local item sequence:

```go
type ItemSeq uint32
```

`ItemSeq` is an identity/lineage anchor, not transcript order and not a content version. It is scoped to a thread branch; a cross-branch reference is `(BranchID, ItemSeq)`.

Replace `PreviousItemMetadata` with:

```go
type metadataDeleteValue struct{}

// DeleteValue removes a top-level key from existing item metadata.
var DeleteValue = metadataDeleteValue{}

type PatchItemMetadata struct {
    // Target is the item to patch. Zero is shorthand for the immediately
    // preceding materialized item at the posting/insertion location.
    Target ItemSeq

    // Metadata is merged by top-level key; later values win. DeleteValue
    // removes the corresponding key.
    Metadata map[string]any
}

func (PatchItemMetadata) Emit() bool { return false }

func (p PatchItemMetadata) ForItem(target ItemSeq) PatchItemMetadata {
    p.Target = target
    return p
}
```

Do not retain a `PreviousItemMetadata` alias. All uses should move to the new, explicit shape.

Change item posting to allow initial metadata in the same mutation:

```go
type Thread interface {
    // Existing one-argument calls continue to compile.
    QueueItem(Item, ...PatchItemMetadata)
    // ...the rest of the interface
}
```

Examples:

```go
// Initial metadata is attached atomically to the posted item.
t.QueueItem(
    threads.UserText("hello"),
    cacheopenai.PromptCacheKey("session-1"),
    cacheopenai.PromptCacheRetention("24h"),
)

// The old adjacent form remains ergonomic. Target zero is resolved before the
// mutation is persisted; zero is never replayed from the WAL.
t.QueueItem(cacheanthropic.Ephemeral5m())

// A later update can target a stable item obtained from the conversation API.
t.QueueItem(cacheanthropic.Ephemeral1h().ForItem(turn.ID()))
```

Rules for the variadic metadata arguments:

- They are initial metadata for the first `Item`, not independent log operations.
- Every supplied patch must have `Target == 0`; panic before mutation if an initial patch names some other item.
- Merge multiple patches in argument order, with the later top-level key winning.
- Initial item plus metadata produces one mutation and one WAL event.
- Calling `QueueItem` with a `PatchItemMetadata` as the first argument performs a metadata patch and does not create a list node.
- An empty patch is a no-op. It should not create a node, seal an item, advance the mutation sequence, or append WAL.

The provider cache helper packages under `llms/cache/*` should return zero-target `threads.PatchItemMetadata` values. This keeps both the adjacent patch form and the atomic item-plus-metadata form concise.

### Canonical in-memory representation

Store identity and current merged metadata on the materialized list node:

```go
type item[T any] struct {
    Item     T
    Next     *item[T]
    Seq      ItemSeq
    Metadata map[string]any // nil for the common case
}
```

`PatchItemMetadata` is a mutation command/WAL operation, not a materialized conversation node. The canonical list and checkpoints contain real thread items with their current merged metadata.

This choice is important for later patches: if a patch occurs much later but targets an old item, a checkpoint or branch prefix containing that old item also contains the latest metadata visible at the checkpoint revision. Snapshot compaction does not need to preserve historical patch nodes.

Initially, resolve explicit targets with a linear linked-list scan. Do not add an in-memory `map[ItemSeq]*item` index in this change; it consumes memory and is unnecessary until profiling shows patch lookup to be a problem.

### Item sequence allocation and coalescing

Use the branch mutation/WAL sequence as the candidate `ItemSeq` whenever a mutation may create a node. This keeps allocation durable without adding another counter.

Required invariants:

1. Every materialized node has a nonzero `ItemSeq`.
2. No two live nodes in one branch have the same `ItemSeq`.
3. A newly inserted node initially uses the mutation/WAL event sequence that inserted it.
4. Text coalescing retains the left node's sequence and discards the right candidate sequence.
5. Repeated `ToolCallChunk` updates retain the original chunk node's sequence.
6. `ToolCallChunk` to final `ToolCall` replacement retains the existing node's sequence and metadata.
7. Removing an unsafe/recovery stream tail removes those node identities.
8. Transcript/list position remains authoritative for order. Never sort or page by `ItemSeq`; insertion before a queued `SendItem` can place a newer sequence before an older one.
9. Item sequences are branch-local. Child checkpoints preserve retained ancestor item sequences; new child mutations continue above the child checkpoint high-water mark.

Metadata is also an explicit control-block coalescing boundary:

- Coalesce text nodes only when both nodes have no metadata.
- If either side has metadata, do not merge the nodes.
- A later nonempty patch to the current item therefore prevents a future same-role item from being absorbed into it, preserving the current `PreviousItemMetadata` behavior.
- Chunk accumulation/finalization for one tool call is still in-place and keeps its metadata; metadata must not prevent chunks for the same `CallID` from completing.

This addresses the tentative-tail issue without delaying allocation. Candidate IDs can be allocated immediately, while the public completed-conversation API exposes only surviving, branchable turn identities. An absorbed right-hand candidate never appears as a completed turn ID.

### Patch targeting semantics

For a standalone `PatchItemMetadata`:

- `Target != 0`: find that materialized item by sequence and merge the patch into it.
- `Target == 0` from ordinary `QueueItem`: target the current transcript tail before applying the patch.
- `Target == 0` from a streamed `appendStreamItem`: target `streamInsertionPoint`, not the list tail, because ordinary user items may have been queued behind the stream while it was running.
- Resolve zero to an explicit target before writing WAL.
- A nonempty patch with no resolvable target is a programmer error. Panic before advancing `mutationSeq`, changing memory, or writing WAL. Do the same for an explicit sequence that is absent (including an already-absorbed tentative sequence).
- Merge only top-level keys. Nested values are replaced wholesale. `DeleteValue` removes a top-level key; `nil` remains JSON null and provider conventions such as `false` remain provider-specific values.
- `DeleteValue` is valid only for standalone/streamed patches against existing metadata. Reject it in initial metadata for a newly queued item.

Validate and deep-copy metadata before mutating the thread. Metadata must be JSON-serializable because it is durable. Use one helper for validation/canonical cloning so callers cannot mutate queued nested maps/slices afterward. Apply the same deep-copy discipline when exposing metadata to request builders and when cloning requests. Avoid `maps.Clone` as the only protection because Anthropic metadata already contains nested maps.

## Conversation API decision

Take the simplifying conversation API break. `Turn` should become a plain projection value instead of retaining a thread pointer, raw item index, and source-head sequence.

Use this public shape conceptually:

```go
type Turn struct {
    index int
    role  TurnRole
    text  string
    id    ItemSeq
}

func (t Turn) Index() int
func (t Turn) Role() TurnRole
func (t Turn) Text() string
func (t Turn) ID() ItemSeq
```

The turn ID is the `ItemSeq` of the **first surviving text node in the turn**, not the last node. This is deliberate:

- Consecutive text coalescing retains the left/first sequence.
- An assistant turn can grow across a resolved tool exchange (`text -> tool call/result -> more text`). Its current branch boundary changes, but its identity should not.
- A branch target created before that later text is appended can still resolve the same logical turn. It resolves the turn as visible at the parent's current head; exact historical content would require a committed checkpoint/revision, which is outside this change.

Internally, calculate completed turns as values plus a current end-node boundary:

```go
type completedTurn struct {
    Turn
    end *item[Item]
}
```

The end pointer never escapes the owning thread. `CompletedTurns` strips the internal boundary and returns plain `Turn` values.

Remove `Turn.Checkpoint` and the stale-turn/thread-pointer machinery. Move turn checkpoint materialization to an unexported thread method used by `BranchManager`, for example:

```go
func (t *thread) checkpointAtTurn(id ItemSeq) (Checkpoint, Turn, error)
```

That method recomputes the currently branchable turns once, matches by stable turn ID, and serializes through the matched internal end node. Return `ErrInvalidTurn` if the ID is zero, absent, or not currently branchable.

This API break is worth taking because it removes:

- `Turn.thread` ownership and stale object checks;
- the overloaded old `Turn.Seq`, which actually meant parent head revision;
- public checkpointing from a display/projection value;
- raw `Turn.end` indexes;
- branch lookup by a projection index;
- duplicate `SourceSeq`/`SourceHeadSeq` meanings.

Keep `Turn.Index()` only as current display metadata. It must not identify a branch or be persisted as the source identity.

## Branching API and storage changes

Change branch targets from turn indexes to stable turn item sequences:

```go
type BranchTarget struct {
    BranchID BranchID
    TurnSeq  *ItemSeq // nil means branch head
}

func BranchTurnTarget(id BranchID, turnSeq ItemSeq) BranchTarget
```

Use a new default string form so old numeric turn-index references cannot silently resolve to unrelated item sequences:

```text
/branch/<branch-id>/seq/<decimal-item-seq>
```

Reject the old `/branch/<id>/turn/<index>` and `/branch/<id>/turn-seq/<item-seq>` forms.

`BranchManager.openTurnTarget` should:

1. Open/load the parent as it does now.
2. Ask the parent thread to resolve and checkpoint `target.TurnSeq`.
3. Record the stable source turn identity and parent head revision.
4. Create/load the child.

Replace branch lineage fields:

```go
type BranchRecord struct {
    // existing ID/kind/ancestor fields
    SourceTurnSeq  ItemSeq
    SourceTurnRole TurnRole
    SourceHeadSeq  uint32
    // existing label/status/timestamps
}

type BranchFromCheckpointOptions struct {
    // existing fields
    SourceTurnSeq  ItemSeq
    SourceTurnRole TurnRole
    SourceHeadSeq  uint32
}
```

Remove `SourceTurnIndex` and `SourceSeq`. `SourceTurnSeq` identifies the logical selected turn; `SourceHeadSeq` identifies the parent revision at which it was materialized.

For a user-turn branch, the checkpoint still needs a synthetic `SendItem` and request-ready state. That synthetic node also needs a unique item sequence:

- Let `sourceHead` be the parent's mutation sequence when resolving the turn.
- Allocate the synthetic send at `ItemSeq(sourceHead + 1)`.
- Set both the child checkpoint `Seq` and snapshot head/high-water sequence to `sourceHead + 1`.
- Keep `BranchRecord.SourceHeadSeq == sourceHead`; it describes the parent, not the synthetic child baseline.
- Detect `uint32` overflow and return an error.

For an assistant-turn branch, no synthetic item is needed. Preserve item sequences in the prefix and use the parent head sequence as the child checkpoint high-water mark. This deliberately avoids reusing sequences belonging to omitted parent-tail events.

Update `MemoryBranchStore` and `durability/sqlitebranchstore` to store the new source fields. In SQLite:

- bump `sqliteBranchSchemaVersion` to `"2"`;
- define new databases with `source_turn_seq`, `source_turn_role`, and `source_head_seq`;
- remove `source_turn_index` and `source_seq` from the v2 schema and all row structs/queries;
- reject existing schema version 1 rather than migrating it;
- update hooks/tests that inspect `BranchRecord`.

## Durability v2

Bump `serializedThreadVersion` from 1 to 2 and reject v1 snapshots/WAL bases. Do not infer IDs for old data.

### Snapshot schema

Add the thread sequence high-water mark and per-item identity/metadata:

```go
type ThreadSnapshot struct {
    Version int    `json:"ver"`
    HeadSeq uint32 `json:"seq"`
    // existing state/items/index fields
}

type SnapshotItem struct {
    Seq      ItemSeq `json:"seq"`
    Metadata string  `json:"meta,omitempty"` // encoded JSON object
    // existing item payload fields
}
```

Use an encoded JSON string for durable metadata, as the current code does for other `map[string]any` data. This avoids map aliasing in `MemoryDurableStore` and keeps the serialized payload optional for unannotated items.

Refactor serialization into two levels:

- item payload conversion (`Item` to/from the existing kind-specific `SnapshotItem` fields);
- node conversion, which adds/reads `Seq` and `Metadata`.

Remove the `item_meta` snapshot kind entirely. `PatchItemMetadata` is not legal as a materialized snapshot item.

`Snapshot()` must set `HeadSeq = t.mutationSeq`. `RestoreThreadSnapshot` must restore `mutationSeq` from `HeadSeq`, so a thread restored directly from a snapshot cannot reuse old item IDs. `RestoreCheckpoint` must validate `Checkpoint.Seq == Snapshot.HeadSeq` rather than silently accepting disagreement.

On snapshot restore, fail closed if:

- any materialized item has sequence zero;
- two materialized items have the same sequence;
- an item sequence is greater than `HeadSeq`;
- metadata is malformed or not a JSON object;
- pointer indexes are invalid (retain existing checks).

Update `cloneSnapshot`, test snapshot literals, and stable JSON round-trip tests for `HeadSeq`, item sequences, and metadata.

### WAL schema

Add a distinct patch operation and explicit patch fields:

```go
const walOpPatchItemMetadata = "patch_item_metadata"

type WALEvent struct {
    Seq      uint32       `json:"s"`
    Op       string       `json:"o"`
    Item     SnapshotItem `json:"i,omitempty"`
    State    State        `json:"state,omitempty"`
    Target     ItemSeq      `json:"target,omitempty"`
    Metadata   string       `json:"meta,omitempty"`
    DeleteKeys []string     `json:"delete,omitempty"`
}
```

For queue/queue-before-send/append-stream events:

- populate `WALEvent.Item.Seq` with the event sequence candidate;
- include initial merged metadata in `WALEvent.Item.Metadata`;
- replay passes that candidate into the control block/list insertion path;
- if replay coalesces or updates an existing node, the candidate is discarded exactly as it was live.

For patch events:

- resolve target zero before persistence;
- write the explicit target, encoded set values, and explicit deleted keys;
- replay never interprets “previous”; it applies the explicit target;
- applying the patch does not create a node.

Fix the current ordering in `queueBeforePendingSend`: reserve/increment the mutation sequence before inserting so the inserted node receives the same sequence recorded by its WAL event. Make all control-block insertion APIs receive an internal posted value containing candidate sequence, item, and initial metadata rather than allocating identity inside the list without context.

WAL replay must still enforce consecutive mutation event sequences. In addition, validate that item-bearing WAL candidates use the event sequence. Patch events consume a mutation sequence but no `ItemSeq` node, so sparse surviving item sequences are expected.

Update `cloneWALEvents`; encoded metadata strings need no map cloning, delete-key slices do, and all existing byte/pointer fields must retain their current clone behavior.

FileStore needs no new container format: its checkpoint/WAL JSON will carry v2. Loading a v1 file should fail at thread restore with the unsupported serialization version.

## Control block and list refactor

Introduce one internal post value so sequence and metadata cannot be accidentally dropped by an insertion path:

```go
type itemCandidate struct {
    Seq      ItemSeq
    Item     Item
    Metadata map[string]any
}
```

Update `cbStateHandler`, `cbItems`, and the list methods used by the control block so all of these paths consume an `itemCandidate`:

- normal append;
- stream insertion;
- insertion before the first pending send;
- tool result insertion before a blocked send.

Keep type switches against `post.Item`.

Audit every in-place mutation:

- `tryCoalesceAhead` keeps the left node's `Seq`; only unannotated nodes may merge;
- repeated tool chunks mutate only `Item` on the existing node;
- final tool calls replace only `Item` on the existing node;
- metadata remains attached throughout;
- removal/drop-tail helpers need no special ID recycling and must never decrement a sequence high-water mark.

Add list helpers that iterate nodes or return node pointers where turn/snapshot code needs identity. Keep `Slice()`/`SliceThrough()` returning `[]Item` for tests and item-only algorithms, but do not use those item-only slices for snapshots, turn boundaries, or request metadata materialization.

## Request builder integration (without a context API)

Do not change `RequestBuilder` to `BuildWithContext` in this work.

Keep its current interface:

```go
type RequestBuilder interface {
    Build(items []Item, caps StreamerCapabilities) Req
}
```

Before invoking a request builder, materialize the canonical nodes through IP into the existing item stream shape:

```text
node item
PatchItemMetadata{Target: 0, Metadata: cloned current metadata}
next node item
...
```

Only emit the synthetic patch item when metadata is nonempty. This preserves the extension point for existing/custom builders while the canonical thread list no longer stores sidecar nodes. The synthetic target must be zero because this is a request projection adjacent to its item, not a durable patch command.

Update the default builder and rollback projection to recognize `PatchItemMetadata` instead of `PreviousItemMetadata`. Their existing positional handling remains valid for this materialized request input. A direct caller of `DefaultRequestBuilder.Build` may use only zero-target adjacent patches; panic on a nonzero target because raw `[]Item` has no item sequences with which to resolve it.

Preserve current request behavior:

- `Req.ItemMeta` remains parallel to emitted `Req.Items` for now;
- metadata on non-emitting control items is not moved to a neighboring emitted item;
- request items coalesce only when normalized metadata is deeply equal;
- rollback projection removes metadata belonging to a removed rollback item and never moves it to a retained sibling;
- latest key values win.

This bridge is intentionally temporary-friendly: a later context reader can consume canonical nodes directly without changing the durable representation introduced here.

## Concrete implementation order

1. **Public types and API skeleton**
   - Add `ItemSeq` and the struct form of `PatchItemMetadata` in `threads/blocks.go` (or a focused metadata file).
   - Add `ForItem` and `Emit`.
   - Change `Thread.QueueItem`, `*thread.QueueItem`, `*Branch.QueueItem`, and test thread wrappers to the variadic signature.
   - Rename cache helper return types and construction across `llms/cache/*`.

2. **Node/list/control-block plumbing**
   - Add `Seq` and `Metadata` to `item`.
   - Add `itemCandidate` and pass it through every append/insert path.
   - Preserve identity and metadata during text coalescing, chunk accumulation, finalization, and removal.
   - Block text coalescing when either node is annotated.

3. **Live posting and patching**
   - Normalize/deep-clone initial metadata before mutation.
   - Allocate candidate sequence from the mutation sequence.
   - Special-case standalone `PatchItemMetadata` so it resolves/updates but never enters the list.
   - Implement normal-tail and stream-insertion-point target-zero resolution.
   - Add invalid-target checks before mutation.
   - Correct sequence reservation for insert-before-send.

4. **Durability v2**
   - Add snapshot head sequence, item sequence, and encoded node metadata.
   - Remove `item_meta` snapshot serialization.
   - Add explicit metadata patch WAL events.
   - Restore/validate sequence high-water and identity invariants.
   - Update checkpoint/safe-snapshot cloning and direct snapshot restore.
   - Add replay equivalence tests before changing branching.

5. **Request projection bridge**
   - Materialize node metadata as zero-target adjacent `PatchItemMetadata` only at the request-builder boundary.
   - Rename/update default request builder and rollback metadata logic.
   - Deep-clone metadata into the builder and `Req`.

6. **Conversation turns and branching**
   - Reimplement completed-turn projection over nodes so it records stable first-text `ItemSeq` plus an internal current end node.
   - Make `Turn` a plain value with `ID()`; remove `Turn.Seq()` and `Turn.Checkpoint()`.
   - Add unexported checkpoint-by-turn-ID logic.
   - Change branch targets/codecs to turn sequence and the new URL form.
   - Replace branch source index/duplicate sequence fields.
   - Handle unique synthetic-send sequence allocation.

7. **Stores and docs**
   - Update memory branch records.
   - Bump/rewrite SQLite branch schema v2 fields and queries.
   - Update package docs, cache helper docs, examples, and comments so none describe “previous item” as the canonical representation.
   - State that old FileStore and SQLite data are unsupported.

8. **Full validation**
   - Run `gofmt` on changed files.
   - Run focused tests while iterating, then `go test ./...` from the repository root.

## Required tests / acceptance criteria

### Identity and coalescing

- Two queued user text items coalesce to one node with the first event's `ItemSeq`.
- Multiple streamed assistant text chunks retain the first surviving text node's sequence.
- Repeated tool chunks and chunk-to-call finalization retain sequence and metadata.
- Metadata supplied with either text item prevents control-block coalescing.
- A later zero-target patch prevents subsequent text from coalescing into the patched current item.
- Insert-before-send demonstrates that transcript order can contain a larger `ItemSeq` before a smaller one.
- No live snapshot contains duplicate or zero item sequences.

### Metadata behavior

- Item plus initial metadata produces one WAL event and survives snapshot/WAL restore.
- A zero-target standalone patch is persisted with a nonzero explicit target.
- A later explicit patch updates an older item and is reflected in the next request.
- Multiple patches merge top-level keys with latest-wins behavior.
- Patching an absent/absorbed target fails before mutation or WAL append.
- Mutating caller-owned nested maps/slices after queueing does not mutate thread state, snapshots, WAL, or requests.
- Canonical `thread.items` contains no `PatchItemMetadata` nodes.

### Durability

- Snapshot encode/decode/encode bytes remain stable for v2.
- Snapshot round trip preserves `HeadSeq`, every `ItemSeq`, metadata, control-block pointers, and state.
- Direct `RestoreThreadSnapshot` followed by a queue operation does not reuse an item sequence.
- Live execution and checkpoint+WAL replay produce identical surviving node sequences and metadata after text coalescing, tool chunk finalization, targeted patches, and insert-before-send.
- v1 thread snapshots and SQLite schema v1 are rejected clearly.
- Corrupt snapshots with zero/duplicate/out-of-range item sequences are rejected.

### Conversation and branching

- `Turn.ID()` is the first surviving text sequence and remains unchanged when same-role text coalesces.
- An assistant turn's ID remains unchanged when it grows across a resolved tool exchange; its branch checkpoint includes the current complete turn boundary.
- Branch manager resolves by `TurnSeq`, not current turn index.
- Appending earlier/later turns does not change existing turn IDs.
- Old `/turn/<index>` and `/turn-seq/<seq>` refs are rejected; `/seq/<seq>` round trips.
- Branch records distinguish `SourceTurnSeq` from `SourceHeadSeq`.
- A branch prefix includes metadata attached to its final included text item, including metadata patched later at the parent head.
- User-turn branches assign a unique sequence to the synthetic send and future child mutations do not collide.
- Child branches preserve retained parent item sequences.

### Request behavior and regressions

- Existing provider/cache request metadata behavior remains unchanged after the rename.
- Initial and later-patched metadata appear at the correct `Req.ItemMeta` index.
- Request coalescing still depends on deep metadata equality.
- Rollbackable tool-failure projection never transfers removed metadata to a retained parallel item.
- Existing reasoning, tool lifecycle, cancellation, recovery, FileStore, MemoryBranchStore, SQLiteBranchStore, and provider tests pass.

## Explicit non-goals

Do not add any of the following in this change:

- `BuildWithContext` or a context-builder interface;
- pruning/token-budget policies;
- summaries, `DerivedFrom`, `Replaces`, pinning, priority, or other context metadata;
- durable context revisions or recording which projection a send used;
- a paged/storage-backed conversation reader;
- immutable log segments or branch structural sharing;
- a global item ID namespace;
- migration from v1 snapshots, WAL, SQLite schema, turn-index URLs, or `PreviousItemMetadata` source code.

The result should be a small v2 durable primitive: stable branch-local item lineage, plain conversation turn identities for branching, canonical per-item generic metadata, atomic item-plus-metadata posting, and explicit later metadata patching. Context management can then be designed separately against these primitives.
