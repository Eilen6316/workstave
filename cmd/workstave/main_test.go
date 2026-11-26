package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"workstave/internal/lock"
	"workstave/internal/outcome"
)

// These tests use a real OS file lock and a real SQLite file in a temp directory. They check
// the CLI wiring only (lock before store, refusal while held); they run no harness.

func TestRecoverIsRefusedWhileStateDirIsLocked(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ws.db")

	held, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"recover", "-db", dbPath}); code != 1 {
		t.Errorf("recover exit code while the lock is held = %d, want 1", code)
	}
	if _, err := os.Stat(dbPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the database was created while the lock was held (stat err = %v); the lock must come before the store", err)
	}

	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"recover", "-db", dbPath}); code != 0 {
		t.Errorf("recover exit code after the lock was released = %d, want 0", code)
	}
	// The command released its own lock on exit.
	l, err := lock.Acquire(dir)
	if err != nil {
		t.Fatalf("recover left the state directory locked: %v", err)
	}
	_ = l.Release()
}

func TestResumeRequiresSession(t *testing.T) {
	if code := run([]string{"resume", "-harness", "codex", "-prompt", "continue", "-db", filepath.Join(t.TempDir(), "ws.db")}); code != 2 {
		t.Fatalf("resume without -session exit code = %d, want 2", code)
	}
}

func TestRunExitCodeRequiresPersistedSuccess(t *testing.T) {
	persistErr := errors.New("finish run: database is closed")
	cases := []struct {
		name string
		out  outcome.Outcome
		err  error
		want int
	}{
		{"completed", outcome.Completed, nil, 0},
		{"completed with persistence error", outcome.Completed, persistErr, 1},
		{"unknown", outcome.Unknown, nil, 1},
		{"failed", outcome.Failed, nil, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := runExitCode(c.out, c.err); got != c.want {
				t.Fatalf("runExitCode(%s, %v) = %d, want %d", c.out, c.err, got, c.want)
			}
		})
	}
}
