// Package store persists Work state, Conversations, Mailbox messages, Main Runs, logical
// sessions, per-work dispatch pause, native bindings and Runs.
// It holds data only; it makes no scheduling or recovery decisions.
package store

import (
	"context"
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// schema is applied on every Open; every statement is idempotent.
// native_id is nullable because Codex returns its thread id only after creation.
// runs.outcome is NULL until a terminal event was observed; an unobserved end is recorded
// explicitly as 'unknown', never left NULL by a finished run.
const schema = `
CREATE TABLE IF NOT EXISTS sessions (
	id         TEXT PRIMARY KEY,
	work_id    TEXT NOT NULL,
	role       TEXT NOT NULL CHECK (role IN ('main','worker')),
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS works (
	id              TEXT PRIMARY KEY,
	main_session_id TEXT NOT NULL UNIQUE REFERENCES sessions(id),
	main_harness    TEXT NOT NULL DEFAULT 'codex' CHECK (main_harness IN ('codex','claude-code')),
	working_dir     TEXT NOT NULL DEFAULT '',
	revision        INTEGER NOT NULL DEFAULT 0,
	controller_epoch INTEGER NOT NULL DEFAULT 1,
	created_at      INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS work_pause (
	work_id TEXT PRIMARY KEY,
	paused  INTEGER NOT NULL DEFAULT 0 CHECK (paused IN (0,1))
);
CREATE TABLE IF NOT EXISTS conversations (
	id         TEXT PRIMARY KEY,
	work_id    TEXT NOT NULL REFERENCES works(id),
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS messages (
	id              TEXT PRIMARY KEY,
	work_id         TEXT NOT NULL REFERENCES works(id),
	conversation_id TEXT NOT NULL REFERENCES conversations(id),
	role            TEXT NOT NULL CHECK (role IN ('user','assistant')),
	body            TEXT NOT NULL,
	reply_to        TEXT REFERENCES messages(id),
	created_at      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS messages_by_conversation ON messages(conversation_id, created_at, id);
CREATE TABLE IF NOT EXISTS message_fact_proposals (
	message_id TEXT NOT NULL REFERENCES messages(id),
	kind       TEXT NOT NULL CHECK (kind IN ('requirement','decision')),
	fact_key   TEXT NOT NULL,
	value      TEXT NOT NULL,
	status     TEXT NOT NULL CHECK (status IN ('confirmed','candidate')),
	PRIMARY KEY (message_id, kind, fact_key)
);
CREATE TABLE IF NOT EXISTS mailbox (
	sequence       INTEGER PRIMARY KEY AUTOINCREMENT,
	work_id        TEXT NOT NULL REFERENCES works(id),
	conversation_id TEXT NOT NULL REFERENCES conversations(id),
	message_id     TEXT NOT NULL UNIQUE REFERENCES messages(id),
	op_id          TEXT NOT NULL DEFAULT '',
	harness        TEXT NOT NULL CHECK (harness IN ('claude-code','codex')),
	harness_experiment INTEGER NOT NULL DEFAULT 0 CHECK (harness_experiment IN (0,1)),
	state          TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','processing','completed','unknown')),
	result         TEXT CHECK (result IN ('completed','failed','unknown')),
	result_detail  TEXT,
	accepted_at    INTEGER NOT NULL,
	started_at     INTEGER,
	finished_at    INTEGER
);
CREATE INDEX IF NOT EXISTS pending_mailbox_by_work ON mailbox(work_id, state, sequence);
CREATE TABLE IF NOT EXISTS native_bindings (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id  TEXT NOT NULL REFERENCES sessions(id),
	harness     TEXT NOT NULL,
	native_id   TEXT,
	cwd         TEXT NOT NULL,
	native_path TEXT,
	active      INTEGER NOT NULL DEFAULT 0 CHECK (active IN (0,1)),
	created_at  INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS one_active_binding ON native_bindings(session_id) WHERE active = 1;
CREATE TABLE IF NOT EXISTS runs (
	id         TEXT PRIMARY KEY,
	session_id TEXT NOT NULL REFERENCES sessions(id),
	binding_id INTEGER REFERENCES native_bindings(id),
	op_id      TEXT NOT NULL UNIQUE,
	started_at INTEGER NOT NULL,
	ended_at   INTEGER,
	outcome    TEXT CHECK (outcome IN ('completed','failed','unknown')),
	intent_hash TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS main_runs (
	run_id            TEXT PRIMARY KEY REFERENCES runs(id),
	work_id           TEXT NOT NULL REFERENCES works(id),
	conversation_id   TEXT NOT NULL REFERENCES conversations(id),
	mailbox_sequence  INTEGER NOT NULL UNIQUE REFERENCES mailbox(sequence),
	source_message_id TEXT NOT NULL UNIQUE REFERENCES messages(id),
	input_snapshot    TEXT NOT NULL,
	context_revision  INTEGER NOT NULL,
	controller_epoch  INTEGER NOT NULL,
	output_message_id TEXT REFERENCES messages(id),
	started_at        INTEGER NOT NULL,
	ended_at          INTEGER
);
CREATE TABLE IF NOT EXISTS work_facts (
	work_id           TEXT NOT NULL REFERENCES works(id),
	kind              TEXT NOT NULL CHECK (kind IN ('requirement','decision')),
	fact_key          TEXT NOT NULL,
	revision          INTEGER NOT NULL,
	value             TEXT NOT NULL,
	status            TEXT NOT NULL CHECK (status IN ('confirmed','candidate','superseded')),
	source_message_id TEXT NOT NULL REFERENCES messages(id),
	confirmation_basis TEXT NOT NULL DEFAULT 'legacy',
	created_at        INTEGER NOT NULL,
	PRIMARY KEY (work_id, kind, fact_key, revision)
);
CREATE INDEX IF NOT EXISTS current_work_facts ON work_facts(work_id, status, kind, fact_key, revision);
`

// DB is an open store.
type DB struct {
	sql *sql.DB
}

// schemaVersion is stored in PRAGMA user_version. Open refuses a database whose version is
// newer than this, so an older binary never writes into a schema it does not understand.
// Databases created before versioning have user_version 0 and are stamped on first open.
const schemaVersion = 6

// Open opens or creates the database at path and applies the schema. Every connection runs
// with synchronous=FULL so a committed record survives a power loss, not only a process crash.
func Open(ctx context.Context, path string) (*DB, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %q: %w", path, err)
	}
	if err := checkVersion(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: apply schema: %w", err)
	}
	if err := migrateDispatch(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := migrateWorkPause(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := migrateIdentityAndResults(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: stamp schema version: %w", err)
	}
	return &DB{sql: db}, nil
}

// OpenControl opens an existing current-schema database for a short control or message
// acceptance transaction while the Runtime owner holds the state-directory lock. It performs no
// schema changes.
func OpenControl(ctx context.Context, path string) (*DB, error) {
	dsn := "file:" + path + "?mode=rw&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open control connection %q: %w", path, err)
	}
	closeOnError := func(err error) (*DB, error) {
		_ = db.Close()
		return nil, err
	}
	var v int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v); err != nil {
		return closeOnError(fmt.Errorf("store: read control schema version: %w", err))
	}
	if v != schemaVersion {
		return closeOnError(fmt.Errorf("store: control connection requires schema version %d, got %d", schemaVersion, v))
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('work_pause') WHERE name = 'paused'`).Scan(&n); err != nil {
		return closeOnError(fmt.Errorf("store: inspect control pause state: %w", err))
	}
	if n != 1 {
		return closeOnError(fmt.Errorf("store: control pause state is unavailable"))
	}
	return &DB{sql: db}, nil
}

// checkVersion refuses a database written by a newer schema before anything is changed.
func checkVersion(ctx context.Context, db *sql.DB) error {
	var v int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v); err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}
	if v > schemaVersion {
		return fmt.Errorf("store: database schema version %d is newer than the supported version %d", v, schemaVersion)
	}
	return nil
}

// Close releases the database handle.
func (d *DB) Close() error { return d.sql.Close() }
