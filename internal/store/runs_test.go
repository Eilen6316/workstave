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
	r1, created, err := db.BeginRun(ctx, "r1", "s1", b1, "op1")
	if err != nil || !created {
		t.Fatalf("first BeginRun: created=%v err=%v", created, err)
	}
	r2, created, err := db.BeginRun(ctx, "r-retry", "s1", b1, "op1")
	if err != nil {
		t.Fatalf("retry BeginRun: %v", err)
	}
	if created || r2.ID != r1.ID {
		t.Fatalf("retry created=%v id=%q, want existing run %q", created, r2.ID, r1.ID)
	}
	if _, _, err := db.BeginRun(ctx, "r3", "s2", 0, "op1"); err == nil {
		t.Fatal("op id reused for a different session was accepted")
	}
	var n int
	if err := db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("runs=%d, want 1", n)
	}
}

func TestFinishRunOnce(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()
	if err := db.CreateSession(ctx, "s1", "w1", "worker"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.BeginRun(ctx, "r1", "s1", 0, "op1"); err != nil {
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

func TestMarkUnfinishedUnknown(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()
	if err := db.CreateSession(ctx, "s1", "w1", "worker"); err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"op-done", "op-open1", "op-open2"} {
		if _, _, err := db.BeginRun(ctx, "run-"+op, "s1", 0, op); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.FinishRun(ctx, "run-op-done", outcome.Completed); err != nil {
		t.Fatal(err)
	}
	open, err := db.UnfinishedRuns(ctx)
	if err != nil || len(open) != 2 {
		t.Fatalf("unfinished before=%d err=%v, want 2", len(open), err)
	}
	n, err := db.MarkUnfinishedUnknown(ctx)
	if err != nil || n != 2 {
		t.Fatalf("marked=%d err=%v, want 2", n, err)
	}
	if open, _ = db.UnfinishedRuns(ctx); len(open) != 0 {
		t.Fatalf("unfinished after=%d, want 0", len(open))
	}
	done, err := db.runByOp(ctx, "op-done")
	if err != nil {
		t.Fatal(err)
	}
	if done.Outcome.String != string(outcome.Completed) {
		t.Fatalf("finished run was overwritten with %q", done.Outcome.String)
	}
	marked, err := db.runByOp(ctx, "op-open1")
	if err != nil {
		t.Fatal(err)
	}
	if marked.Outcome.String != string(outcome.Unknown) {
		t.Fatalf("unfinished run outcome=%q, want unknown", marked.Outcome.String)
	}
}
