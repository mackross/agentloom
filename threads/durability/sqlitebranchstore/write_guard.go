package sqlitebranchstore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strings"

	"modernc.org/sqlite"
)

const sqliteWriterVersion = 2
const sqliteWriterVersionFunction = "agentloom_writer_version"

func init() {
	// This function belongs to the running binary's connections, not to the
	// database. Already-running old binaries cannot acquire it just by reading
	// the upgraded schema. Register before applications open any connections.
	sqlite.MustRegisterDeterministicScalarFunction(sqliteWriterVersionFunction, 0,
		func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
			return int64(sqliteWriterVersion), nil
		})
}

// Persistent triggers fence old connections, including statements prepared
// before migration. Checking a version only in new Go code cannot do that.
// Install the fence in the same transaction as migration: an old writer either
// commits before migration reads its data or fails after migration commits.
// Keep the version in trigger names so a future incompatible writer can add a
// stricter fence without a stale process replacing it with a weaker one.
func installSQLiteWriteGuards(ctx context.Context, tx *sql.Tx, extraTables []string) error {
	tables := append([]string{"thread_branch_meta", "thread_branches", "thread_checkpoints", "thread_wal_events"}, extraTables...)
	quote := func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
	for _, table := range tables {
		for _, op := range []string{"INSERT", "UPDATE", "DELETE"} {
			name := fmt.Sprintf("agentloom_writer_v%d_%s_%s", sqliteWriterVersion, table, strings.ToLower(op))
			stmt := fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS %s BEFORE %s ON %s
				WHEN COALESCE(%s(), 0) < %d
				BEGIN
					SELECT RAISE(ABORT, 'SQLite branch store was upgraded; restart this process using the updated application');
				END`, quote(name), op, quote(table), sqliteWriterVersionFunction, sqliteWriterVersion)
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("install sqlite writer fence for %q: %w", table, err)
			}
		}
	}
	return nil
}
