package store

import (
	"context"
	"testing"

	"workstave/internal/outcome"
)

// These tests exercise the store against a real SQLite file. They say nothing about harness behaviour.

func TestBindNativeKeepsOneActive(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()
	if err := db.CreateSession(ctx, "s1", "w1", "main"); err != nil {
		t.Fatal(err)
	}
	first, err := db.BindNative(ctx, "s1", "codex", "/a")
	if err != nil {
		t.Fatal(err)
	}
	second, err := db.BindNative(ctx, "s1", "claude", "/b")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("second binding reused the first binding id")
	}
	var active int
	var activeID int64
	if err := db.sql.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MAX(id),0) FROM native_bindings WHERE session_id='s1' AND active=1`).Scan(&active, &activeID); err != nil {
		t.Fatal(err)
	}
	if active != 1 || activeID != second {
		t.Fatalf("active bindings=%d active id=%d, want 1 and %d", active, activeID, second)
	}
	if err := db.SetNativeID(ctx, second, "native-1", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.SetNativeID(ctx, second, "native-2", ""); err == nil {
		t.Fatal("an existing native id was overwritten")
	}
	if err := db.SetNativeID(ctx, 9999, "x", ""); err == nil {
		t.Fatal("setting the native id of a missing binding succeeded")
	}
}

func TestBeginRunIsIdempotentPerOp(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()
	for _, id := range []string{"s1", "s2"} {
		if err := db.CreateSession(ctx, id, "w1", "worker"); err != nil {
			t.Fatal(err)
		}
	}
	b1, err := db.BindNative(ctx, "s1", "codex", "/a")
	if err != nil {
		t.Fatal(err)
	}
	r1, created, err := db.BeginRun(ctx, "r1", "s1", b1, "op1", "intent-1")
	if err != nil || !created {
		t.Fatalf("first BeginRun: created=%v err=%v", created, err)
	}
	r2, created, err := db.BeginRun(ctx, "r-retry", "s1", b1, "op1", "intent-1")
	if err != nil {
		t.Fatalf("retry BeginRun: %v", err)
	}
	if created || r2.ID != r1.ID {
		t.Fatalf("retry created=%v id=%q, want existing run %q", created, r2.ID, r1.ID)
	}
	if _, _, err := db.BeginRun(ctx, "r3", "s2", b1, "op1", "intent-1"); err == nil {
		t.Fatal("op id reused for a different session was accepted")
	}
	if _, _, err := db.BeginRun(ctx, "r4", "s1", b1, "op1", "different-intent"); err == nil {
		t.Fatal("op id reused for a different intent was accepted")
	}
	var n int
	if err := db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("runs=%d, want 1", n)
	}
}

func TestBeginRunRejectsInactiveBinding(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()
	if err := db.CreateSession(ctx, "s1", "w1", "worker"); err != nil {
		t.Fatal(err)
	}
	oldBinding, err := db.BindNative(ctx, "s1", "codex", "/old")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.BindNative(ctx, "s1", "codex", "/new"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.BeginRun(ctx, "r1", "s1", oldBinding, "op1", "intent-1"); err == nil {
		t.Fatal("Run was created on an inactive binding")
	}
}

func TestFinishRunOnce(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()
	if err := db.CreateSession(ctx, "s1", "w1", "worker"); err != nil {
		t.Fatal(err)
	}
	bindingID, err := db.BindNative(ctx, "s1", "codex", "/work")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.BeginRun(ctx, "r1", "s1", bindingID, "op1", "intent-1"); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishRun(ctx, "r1", outcome.Outcome("success")); err == nil {
		t.Fatal("an outcome outside completed, failed, unknown was accepted")
	}
	if err := db.FinishRun(ctx, "r1", outcome.Completed); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishRun(ctx, "r1", outcome.Failed); err == nil {
		t.Fatal("a finished run was finished again")
	}
	r, err := db.runByOp(ctx, "op1")
	if err != nil {
		t.Fatal(err)
	}
	if r.Outcome.String != string(outcome.Completed) || !r.EndedAt.Valid {
		t.Fatalf("stored outcome=%q ended=%v", r.Outcome.String, r.EndedAt.Valid)
	}
}
