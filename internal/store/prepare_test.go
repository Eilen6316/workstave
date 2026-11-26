package store

import (
	"context"
	"testing"
)

// These tests exercise BeginFirstRun against a real SQLite file. They say nothing about harness behaviour.

func countRows(t *testing.T, db *DB, q string) int {
	t.Helper()
	var n int
	if err := db.sql.QueryRowContext(context.Background(), q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestBeginFirstRunRecordsBindingAndRunTogetherAndIsIdempotent(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()
	if err := db.CreateSession(ctx, "s1", "w1", "worker"); err != nil {
		t.Fatal(err)
	}
	b1, r1, created, err := db.BeginFirstRun(ctx, "r1", "s1", "claude-code", "/x", "native-1", "op1")
	if err != nil || !created {
		t.Fatalf("first call: created=%v err=%v", created, err)
	}
	if id, err := db.NativeID(ctx, b1); err != nil || id != "native-1" {
		t.Fatalf("native id %q err=%v, want native-1", id, err)
	}
	b2, r2, created, err := db.BeginFirstRun(ctx, "r-retry", "s1", "claude-code", "/x", "native-2", "op1")
	if err != nil || created || r2.ID != r1.ID || b2 != b1 {
		t.Fatalf("retry: created=%v run=%q binding=%d err=%v, want existing run %q binding %d", created, r2.ID, b2, err, r1.ID, b1)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM native_bindings`); n != 1 {
		t.Fatalf("bindings=%d after a repeated op, want 1", n)
	}
	if _, _, _, err := db.BeginFirstRun(ctx, "r3", "s2", "codex", "/x", "", "op1"); err == nil {
		t.Fatal("op id reused for a different session was accepted")
	}
}

func TestBeginFirstRunRollsBackTheBindingWhenTheRunCannotBeInserted(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()
	if err := db.CreateSession(ctx, "s1", "w1", "worker"); err != nil {
		t.Fatal(err)
	}
	b1, _, _, err := db.BeginFirstRun(ctx, "r1", "s1", "codex", "/a", "", "op1")
	if err != nil {
		t.Fatal(err)
	}
	// Same run id, new op id: the run insert fails after the binding insert succeeded.
	if _, _, _, err := db.BeginFirstRun(ctx, "r1", "s1", "claude-code", "/b", "native-2", "op2"); err == nil {
		t.Fatal("a duplicate run id was accepted")
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM native_bindings`); n != 1 {
		t.Fatalf("bindings=%d, want 1: the failed call left a binding behind", n)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM native_bindings WHERE active = 1 AND id = `+itoa(b1)); n != 1 {
		t.Fatal("the previous active binding was deactivated by the failed call")
	}
}

func itoa(n int64) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{digits[n%10]}, b...)
	}
	return string(b)
}
