// Package store persists the minimal P0-A records: logical sessions, native bindings and runs.
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
	outcome    TEXT CHECK (outcome IN ('completed','failed','unknown'))
);
`

// DB is an open store.
type DB struct {
	sql *sql.DB
}

// Open opens or creates the database at path and applies the schema.
func Open(ctx context.Context, path string) (*DB, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %q: %w", path, err)
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: apply schema: %w", err)
	}
	return &DB{sql: db}, nil
}

// Close releases the database handle.
func (d *DB) Close() error { return d.sql.Close() }
