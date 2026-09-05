-- SQLite schema from Agentloom before item identities (37598d5^).

CREATE TABLE IF NOT EXISTS thread_branch_meta (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS thread_branches (
	id TEXT PRIMARY KEY,
	kind TEXT NOT NULL,
	ancestors_json TEXT NOT NULL DEFAULT '[]',
	source_turn_index INTEGER NOT NULL DEFAULT 0,
	source_turn_role TEXT NOT NULL DEFAULT '',
	source_seq INTEGER NOT NULL DEFAULT 0,
	source_head_seq INTEGER NOT NULL DEFAULT 0,
	label TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'active',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	last_seq INTEGER NOT NULL DEFAULT 0,
	head_version INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS thread_checkpoints (
	branch_id TEXT PRIMARY KEY REFERENCES thread_branches(id) ON DELETE CASCADE,
	seq INTEGER NOT NULL,
	unsafe INTEGER NOT NULL,
	snapshot_json TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS thread_wal_events (
	branch_id TEXT NOT NULL REFERENCES thread_branches(id) ON DELETE CASCADE,
	seq INTEGER NOT NULL,
	op TEXT NOT NULL,
	event_json TEXT NOT NULL,
	created_at TEXT NOT NULL,
	PRIMARY KEY (branch_id, seq)
);

CREATE INDEX IF NOT EXISTS thread_branches_updated_idx ON thread_branches(updated_at DESC);
CREATE INDEX IF NOT EXISTS thread_wal_events_branch_seq_idx ON thread_wal_events(branch_id, seq);

INSERT INTO thread_branch_meta VALUES('schema_version', '1');
