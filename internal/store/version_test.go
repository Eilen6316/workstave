package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// These tests exercise the open-time pragmas and the schema version on a real SQLite file.

func TestOpenSetsDurabilityAndVersion(t *testing.T) {
	db := openTemp(t)
	var sync int
	if err := db.sql.QueryRowContext(context.Background(), `PRAGMA synchronous`).Scan(&sync); err != nil {
		t.Fatal(err)
	}
	if sync != 2 {
		t.Errorf("synchronous = %d, want 2 (FULL)", sync)
	}
	var v int
	if err := db.sql.QueryRowContext(context.Background(), `PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != schemaVersion {
		t.Errorf("user_version = %d, want %d", v, schemaVersion)
	}
}

func TestOpenMigratesV4OperationAndConclusionFields(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v4.db")
	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{
		`CREATE TABLE sessions(id TEXT PRIMARY KEY, work_id TEXT NOT NULL, role TEXT NOT NULL, created_at INTEGER NOT NULL)`,
		`CREATE TABLE works(id TEXT PRIMARY KEY, main_session_id TEXT NOT NULL UNIQUE REFERENCES sessions(id), revision INTEGER NOT NULL DEFAULT 0, controller_epoch INTEGER NOT NULL DEFAULT 1, created_at INTEGER NOT NULL)`,
		`CREATE TABLE work_pause(work_id TEXT PRIMARY KEY, paused INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE conversations(id TEXT PRIMARY KEY, work_id TEXT NOT NULL REFERENCES works(id), created_at INTEGER NOT NULL)`,
		`CREATE TABLE messages(id TEXT PRIMARY KEY, work_id TEXT NOT NULL REFERENCES works(id), conversation_id TEXT NOT NULL REFERENCES conversations(id), role TEXT NOT NULL, body TEXT NOT NULL, reply_to TEXT REFERENCES messages(id), created_at INTEGER NOT NULL)`,
		`CREATE TABLE mailbox(sequence INTEGER PRIMARY KEY AUTOINCREMENT, work_id TEXT NOT NULL REFERENCES works(id), conversation_id TEXT NOT NULL REFERENCES conversations(id), message_id TEXT NOT NULL UNIQUE REFERENCES messages(id), harness TEXT NOT NULL, state TEXT NOT NULL, accepted_at INTEGER NOT NULL, started_at INTEGER, finished_at INTEGER)`,
		`CREATE TABLE native_bindings(id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL REFERENCES sessions(id), harness TEXT NOT NULL, native_id TEXT, cwd TEXT NOT NULL, native_path TEXT, active INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL)`,
		`CREATE TABLE runs(id TEXT PRIMARY KEY, session_id TEXT NOT NULL REFERENCES sessions(id), binding_id INTEGER REFERENCES native_bindings(id), op_id TEXT NOT NULL UNIQUE, started_at INTEGER NOT NULL, ended_at INTEGER, outcome TEXT, dispatch TEXT NOT NULL DEFAULT 'pending')`,
		`CREATE TABLE main_runs(run_id TEXT PRIMARY KEY REFERENCES runs(id), work_id TEXT NOT NULL REFERENCES works(id), conversation_id TEXT NOT NULL REFERENCES conversations(id), mailbox_sequence INTEGER NOT NULL UNIQUE REFERENCES mailbox(sequence), source_message_id TEXT NOT NULL UNIQUE REFERENCES messages(id), input_snapshot TEXT NOT NULL, context_revision INTEGER NOT NULL, controller_epoch INTEGER NOT NULL, output_message_id TEXT REFERENCES messages(id), started_at INTEGER NOT NULL, ended_at INTEGER)`,
		`CREATE TABLE work_facts(work_id TEXT NOT NULL REFERENCES works(id), kind TEXT NOT NULL, fact_key TEXT NOT NULL, revision INTEGER NOT NULL, value TEXT NOT NULL, status TEXT NOT NULL, source_message_id TEXT NOT NULL REFERENCES messages(id), created_at INTEGER NOT NULL, PRIMARY KEY(work_id,kind,fact_key,revision))`,
		`INSERT INTO sessions VALUES ('main-1','w1','main',1)`,
		`INSERT INTO works VALUES ('w1','main-1',1,1,1)`,
		`INSERT INTO work_pause VALUES ('w1',0)`,
		`INSERT INTO conversations VALUES ('conv-1','w1',1)`,
		`INSERT INTO messages VALUES ('msg-1','w1','conv-1','user','source',NULL,1)`,
		`INSERT INTO mailbox(work_id,conversation_id,message_id,harness,state,accepted_at,started_at) VALUES ('w1','conv-1','msg-1','codex','processing',1,2)`,
		`INSERT INTO runs(id,session_id,op_id,started_at,dispatch) VALUES ('run-1','main-1','main-msg-1',2,'dispatching')`,
		`INSERT INTO main_runs VALUES ('run-1','w1','conv-1',1,'msg-1','{}',1,1,NULL,2,NULL)`,
		`INSERT INTO work_facts VALUES ('w1','requirement','auth_method',1,'JWT','confirmed','msg-1',1)`,
		`PRAGMA user_version=4`,
	}
	for _, statement := range statements {
		if _, err := old.ExecContext(ctx, statement); err != nil {
			t.Fatalf("create v4 fixture: %v", err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open v4 database: %v", err)
	}
	defer db.Close()
	item, err := db.MailboxByMessage(ctx, "msg-1")
	if err != nil || item.OpID != "main-msg-1" {
		t.Fatalf("legacy Mailbox identity=%+v err=%v", item, err)
	}
	var basis, intentHash string
	if err := db.sql.QueryRowContext(ctx, `SELECT confirmation_basis FROM work_facts WHERE source_message_id='msg-1'`).Scan(&basis); err != nil || basis != "legacy" {
		t.Fatalf("legacy confirmation basis=%q err=%v", basis, err)
	}
	if err := db.sql.QueryRowContext(ctx, `SELECT intent_hash FROM runs WHERE id='run-1'`).Scan(&intentHash); err != nil || intentHash != "" {
		t.Fatalf("legacy Run intent hash=%q err=%v", intentHash, err)
	}
	var version int
	if err := db.sql.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("schema version=%d err=%v", version, err)
	}
}

func TestOpenRefusesNewerSchema(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ws.db")
	db, err := Open(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if err := exec(t, db, `PRAGMA user_version = 99`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if _, err := Open(context.Background(), p); err == nil {
		t.Fatal("a database with a newer schema version was opened")
	}
}

func TestOpenControlRequiresExistingCurrentSchema(t *testing.T) {
	ctx := context.Background()
	missing := filepath.Join(t.TempDir(), "missing.db")
	if _, err := OpenControl(ctx, missing); err == nil {
		t.Fatal("control connection opened a missing database")
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("control connection changed missing database state: stat err=%v", err)
	}

	path := filepath.Join(t.TempDir(), "current.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	control, err := OpenControl(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := control.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	legacyPath := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := Open(ctx, legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := exec(t, legacy, `PRAGMA user_version = 2`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenControl(ctx, legacyPath); err == nil {
		t.Fatal("control connection accepted a database requiring migration")
	}
}
