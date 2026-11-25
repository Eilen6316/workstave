package store

import (
	"context"
	"path/filepath"
	"testing"
)

// These tests exercise the schema constraints on a real SQLite file. They say nothing about
// harness behaviour.

func openTemp(t *testing.T) *DB {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "ws.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func exec(t *testing.T, db *DB, q string, args ...any) error {
	t.Helper()
	_, err := db.sql.ExecContext(context.Background(), q, args...)
	return err
}

func TestOpenIsIdempotent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ws.db")
	for i := 0; i < 2; i++ {
		db, err := Open(context.Background(), p)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		_ = db.Close()
	}
}

func TestOneActiveBindingPerSession(t *testing.T) {
	db := openTemp(t)
	if err := exec(t, db, `INSERT INTO sessions(id, work_id, role, created_at) VALUES ('s1','w1','main',1)`); err != nil {
		t.Fatal(err)
	}
	ins := `INSERT INTO native_bindings(session_id, harness, cwd, active, created_at) VALUES ('s1','codex','/x',?,1)`
	if err := exec(t, db, ins, 1); err != nil {
		t.Fatalf("first active binding: %v", err)
	}
	if err := exec(t, db, ins, 1); err == nil {
		t.Fatal("second active binding for the same session was accepted")
	}
	if err := exec(t, db, ins, 0); err != nil {
		t.Fatalf("inactive historical binding rejected: %v", err)
	}
}

func TestRunConstraints(t *testing.T) {
	db := openTemp(t)
	if err := exec(t, db, `INSERT INTO sessions(id, work_id, role, created_at) VALUES ('s1','w1','worker',1)`); err != nil {
		t.Fatal(err)
	}
	ins := `INSERT INTO runs(id, session_id, op_id, started_at, outcome) VALUES (?, 's1', ?, 1, ?)`
	if err := exec(t, db, ins, "r1", "op1", "unknown"); err != nil {
		t.Fatalf("valid run: %v", err)
	}
	if err := exec(t, db, ins, "r2", "op1", "completed"); err == nil {
		t.Error("duplicate op_id was accepted")
	}
	if err := exec(t, db, ins, "r3", "op3", "success"); err == nil {
		t.Error("outcome outside completed/failed/unknown was accepted")
	}
	if err := exec(t, db, `INSERT INTO runs(id, session_id, op_id, started_at) VALUES ('r4','nope','op4',1)`); err == nil {
		t.Error("run for a missing session was accepted (foreign keys not enforced)")
	}
}

func TestSessionRoleConstraint(t *testing.T) {
	db := openTemp(t)
	if err := exec(t, db, `INSERT INTO sessions(id, work_id, role, created_at) VALUES ('s1','w1','boss',1)`); err == nil {
		t.Fatal("invalid role was accepted")
	}
}
