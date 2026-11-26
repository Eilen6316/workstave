package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"workstave/internal/outcome"
	"workstave/internal/store"
)

// This test kills a real helper process (a re-executed test binary) after the dispatching state
// was committed and before the harness returned. It checks what the store holds after such a
// crash; it says nothing about the real Claude Code or Codex.

type crashing struct{}

func (crashing) Name() string        { return "codex" }
func (crashing) NewNativeID() string { return "" }
func (crashing) Execute(context.Context, Request, func(string)) (outcome.Outcome, error) {
	os.Exit(3) // no deferred close, no outcome recorded
	return "", nil
}

// TestHelperCrashAfterDispatching is the crashing process. It does nothing unless the parent
// test asks for it through the environment.
func TestHelperCrashAfterDispatching(t *testing.T) {
	path := os.Getenv("WORKSTAVE_CRASH_DB")
	if path == "" {
		t.Skip("helper process for TestCrashAfterDispatchingIsRecoveredAsUnknown")
	}
	ctx := context.Background()
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(ctx, "s1", "w1", "worker"); err != nil {
		t.Fatal(err)
	}
	_, _ = Create(ctx, db, crashing{}, params)
	t.Fatal("Create returned; the harness should have terminated the process")
}

func TestCrashAfterDispatchingIsRecoveredAsUnknown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ws.db")
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperCrashAfterDispatching$")
	cmd.Env = append(os.Environ(), "WORKSTAVE_CRASH_DB="+path)
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 3 {
		t.Fatalf("helper did not exit with code 3: err=%v output=%s", err, out)
	}

	ctx := context.Background()
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// What the crash left behind: a run without outcome that is already dispatching.
	if s, err := db.DispatchState(ctx, "r1"); err != nil || s != store.DispatchDispatching {
		t.Fatalf("dispatch state after the crash = %q err=%v, want dispatching", s, err)
	}
	if open, err := db.UnfinishedRuns(ctx); err != nil || len(open) != 1 {
		t.Fatalf("unfinished runs after the crash = %d err=%v, want 1", len(open), err)
	}

	rec, err := db.RecoverOnStart(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Pending != 0 || rec.Unknown != 1 {
		t.Fatalf("recovery = %+v, want 0 pending and 1 unknown", rec)
	}
	r, ok, err := db.RunForOp(ctx, "op1")
	if err != nil || !ok || r.Outcome.String != string(outcome.Unknown) {
		t.Fatalf("run after recovery: ok=%v outcome=%q err=%v, want unknown", ok, r.Outcome.String, err)
	}

	// The same operation id must not launch again after recovery.
	f := &fake{name: "codex", out: outcome.Completed}
	retry := params
	retry.RunID = "r-retry"
	if _, err := Create(ctx, db, f, retry); err == nil || f.calls != 0 {
		t.Fatalf("a recovered unknown op was launched again: err=%v calls=%d", err, f.calls)
	}
}
