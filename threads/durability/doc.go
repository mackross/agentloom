// Package durability stores threads across process restarts.
// FileStore is a local checkpoint plus WAL file.
// See durability/sqlitebranchstore for branch storage in SQLite.
//
// Thread serialization is schema version 2. The runtime rejects version 1
// snapshots, and version 1 WAL tails must not be replayed against version 2
// checkpoints. SQLiteBranchStore migrates version 1 databases on open, converting
// checkpoints and WAL together. FileStore does not migrate version 1 files.
package durability
