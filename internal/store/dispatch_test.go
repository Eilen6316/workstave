package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"workstave/internal/outcome"
)

// These tests exercise dispatch states and start-up recovery against a real SQLite file. They say
// nothing about harness behaviour; whether a recovered run is really still alive is Not Tested.

func dispatchOf(t *testing.T, db *DB, runID string) string {
	t.Helper()
	var s string
	if err := db.sql.QueryRowContext(context.Background(), `SELECT dispatch FROM runs WHERE id = ?`, runID).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func outcomeOf(t *testing.T, db *DB, runID string) string {
	t.Helper()
	var s sql.NullString
	if err := db.sql.QueryRowContext(context.Background(), `SELECT outcome FROM runs WHERE id = ?`, runID).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s.String
}

func seedRun(t *testing.T, db *DB, runID string) {
	t.Helper()
	if _, _, _, err := db.BeginFirstRun(context.Background(), runID, "s1", "codex", "/x", "", "op-"+runID); err != nil {
		t.Fatal(err)
	}
}

func openWithSession(t *testing.T) *DB {
	t.Helper()
	db := openTemp(t)
	if err := db.CreateSession(context.Background(), "s1", "w1", "worker"); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestDispatchStateTransitions(t *testing.T) {
	db := openWithSession(t)
	ctx := context.Background()
	seedRun(t, db, "r1")
	if got := dispatchOf(t, db, "r1"); got != DispatchPending {
		t.Fatalf("new run dispatch = %q, want pending", got)
	}
	if err := db.MarkAcknowledged(ctx, "r1"); err == nil {
		t.Fatal("pending -> acknowledged was accepted without dispatching")
	}
	if err := db.MarkDispatching(ctx, "r1"); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkDispatching(ctx, "r1"); err == nil {
		t.Fatal("dispatching -> dispatching was accepted")
	}
	if err := db.MarkAcknowledged(ctx, "r1"); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishRun(ctx, "r1", outcome.Completed); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkDispatching(ctx, "r1"); err == nil {
		t.Fatal("a finished run was moved back to dispatching")
	}
}

func TestRecoverOnStartKeepsPendingAndMarksTheRest(t *testing.T) {
	db := openWithSession(t)
	ctx := context.Background()
	for _, id := range []string{"r-pending", "r-disp", "r-ack", "r-done"} {
		seedRun(t, db, id)
	}
	for _, id := range []string{"r-disp", "r-ack", "r-done"} {
		if err := db.MarkDispatching(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"r-ack", "r-done"} {
		if err := db.MarkAcknowledged(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.FinishRun(ctx, "r-done", outcome.Completed); err != nil {
		t.Fatal(err)
	}

	rec, err := db.RecoverOnStart(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Pending != 1 || rec.Unknown != 2 {
		t.Fatalf("recovery = %+v, want 1 pending kept and 2 unknown", rec)
	}
	want := map[string]string{"r-pending": "", "r-disp": "unknown", "r-ack": "unknown", "r-done": "completed"}
	for id, w := range want {
		if got := outcomeOf(t, db, id); got != w {
			t.Errorf("outcome of %s = %q, want %q", id, got, w)
		}
	}
	if got := dispatchOf(t, db, "r-pending"); got != DispatchPending {
		t.Errorf("pending run dispatch = %q after recovery, want pending", got)
	}

	rec, err = db.RecoverOnStart(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Pending != 1 || rec.Unknown != 0 {
		t.Fatalf("second recovery = %+v, want 1 pending kept and 0 unknown", rec)
	}
}

func TestOpenMigratesDatabaseWithoutDispatchColumn(t *testing.T) {
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "ws.db")

	old, err := sql.Open("sqlite", "file:"+p)
	if err != nil {
		t.Fatal(err)
	}
	stmts := []string{
		schema,
		`INSERT INTO sessions(id, work_id, role, created_at) VALUES ('s1','w1','worker',1)`,
		`INSERT INTO runs(id, session_id, op_id, started_at) VALUES ('r-open','s1','op-open',1)`,
		`INSERT INTO runs(id, session_id, op_id, started_at, ended_at, outcome) VALUES ('r-done','s1','op-done',1,2,'completed')`,
		`PRAGMA user_version = 1`,
	}
	for _, s := range stmts {
		if _, err := old.ExecContext(ctx, s); err != nil {
			t.Fatalf("seed old database: %v", err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(ctx, p)
	if err != nil {
		t.Fatalf("open old database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if got := dispatchOf(t, db, "r-open"); got != DispatchDispatching {
		t.Errorf("unfinished run of an old database has dispatch %q, want dispatching (never pending)", got)
	}
	rec, err := db.RecoverOnStart(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Pending != 0 || rec.Unknown != 1 {
		t.Fatalf("recovery = %+v, want 0 pending and 1 unknown", rec)
	}
	if got := outcomeOf(t, db, "r-done"); got != "completed" {
		t.Errorf("finished run outcome = %q, want completed", got)
	}
}
