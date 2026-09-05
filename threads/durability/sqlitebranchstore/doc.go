// Package sqlitebranchstore persists threads branches in SQLite.
// It stores branch records, leases, checkpoints, and WAL events.
// Use it when branch state needs transactions or shared ownership.
//
// Opening a version 1 database transactionally upgrades its schema, checkpoints,
// and WAL to version 2. Checkpoint items receive stable identities, legacy
// metadata nodes become attached metadata, and WAL sequences are rebased above
// the old head. The checkpoint and WAL remain separate to preserve recovery
// boundaries. Unrecoverable source-turn identities are zero; the original
// source_turn_index and source_seq columns remain available for provenance.
// Unknown versions or invalid data fail without committing migration changes.
//
// Opening the store also installs persistent writer-version triggers in the
// same transaction. Old binaries lack the connection-local agentloom_writer_version
// function and fail on their next attempted write, even on an already-open
// connection. Read-only inspection still works. Compatible processes can keep
// sharing the store using the usual transaction and branch lease rules.
// WriteGuardTables extends this fence to application metadata tables. Idle old
// processes are not terminated; their leases must be released before resuming
// those branches. Database tools that write to guarded tables also need a
// compatible writer; these guards prevent accidental stale writes, not hostile
// SQL clients that deliberately remove schema objects.
package sqlitebranchstore
