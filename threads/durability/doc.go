// Package durability stores threads across process restarts.
// FileStore is a local checkpoint plus WAL file.
// See durability/sqlitebranchstore for branch storage in SQLite.
//
// Thread serialization is schema version 2 and is not compatible with the
// pre-item-sequence v1 schema. Version 1 snapshots are rejected rather than
// migrated, and version 1 WAL tails must not be replayed against version 2
// checkpoints.
package durability
