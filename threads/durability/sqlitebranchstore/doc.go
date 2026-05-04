// Package sqlitebranchstore persists threads branches in SQLite.
// It stores branch records, leases, checkpoints, and WAL events.
// Use it when branch state needs transactions or shared ownership.
package sqlitebranchstore
