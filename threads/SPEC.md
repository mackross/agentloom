# Threads (CPU Thread Metaphor)

This package models a thread as an append-only per-thread history of items, plus
a derived runtime state that acts like a CPU thread control block.

## Core Model

- A **Thread** is the container for:
  - `items`: an append-only linked list of items (the "instruction stream")
  - `cb`: a private control block (the "thread control block")
  - (later) an executor attachment

- An **Item** is one unit in the thread history. Examples:
  - user message items
  - assistant message items
  - tool-call request items
  - tool-result items
  - future fork and merge items

- The **Control Block** (TCB) is derived state for the thread. It is responsible for:
  - tracking where execution is in the log
  - tracking whether the thread is runnable or blocked
  - retaining active request metadata (e.g. the exact tool definitions snapshot sent)
  - gating advancement when there are blocking forks or blocking tool executions
  - managing queued unsent items and throttling policy (declarative data)

The control block must be rebuildable by replaying the thread history.

## Instruction Pointer (IP)

The control block maintains an **Instruction Pointer (IP)** into the thread's items.

- IP is the current "program counter" for this thread.
- Advancing the thread means moving IP forward and updating control-block state.

## Queued Items

The thread may have items that are appended but not yet incorporated into the active
request/processing flow. Conceptually these are "queued instructions" that the executor
has not yet executed/sent.

In v0, the queue is simply the thread's `items` list before IP is advanced.
As behavior grows, the control block becomes the authority for what is considered
queued vs active vs already processed.

## Executor

The **Thread Executor** runs:
- LLM requests
- tool call execution (sync and async)

The executor notifies its delegate as the control block moves (state transitions,
items appended, tool completions, etc.). It is typical for an agent to be the executor's
delegate.

The executor must use the tool definitions snapshot that was active for the request
that produced a given tool call.

## Forking

Forks are separate threads with their own item histories and their own control blocks.

Fork creation can be represented in the parent thread history via a fork item.
Merging can be represented via merge items.

### Fork Types

1) **Detached fork** (previously "silent fork")
   - can be created anywhere in the log
   - does not affect whether the parent thread is runnable
   - results may or may not be merged later

2) **Joinable fork** (previously "blocking fork")
   - parent thread becomes blocked until the fork is joined/merged (like `join()`)
   - once merged, the parent becomes runnable again

3) **Promise fork**
   - returns a handle immediately (future/promise)
   - parent thread continues running
   - the parent can query the fork (handoff) and later decide what to merge
   - the question/answer dialogue occurs in a "merge thread" (a small fork whose
     result is the merge payload)

### Follow-Then-Fork Behavior

When a fork is requested, the new fork may initially need to "follow" the parent thread
until blocking waits resolve (e.g. parent has pending joinable fork or blocking tool).
In this phase, the fork's control block is not runnable because its instruction stream
depends on unresolved parent progress.

This makes the control block naturally representable as a state machine.
