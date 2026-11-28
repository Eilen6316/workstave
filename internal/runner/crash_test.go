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
func (crashing) Execute(context.Context, Request, func(string)) (Execution, error) {
	os.Exit(3) // no deferred close, no outcome recorded
	return Execution{}, nil
}

type crashingAfterAck struct{ terminalMarker string }

func (crashingAfterAck) Name() string        { return "codex" }
func (crashingAfterAck) NewNativeID() string { return "" }
func (c crashingAfterAck) Execute(_ context.Context, _ Request, onAcknowledged func(string)) (Execution, error) {
	onAcknowledged("thread-crash")
	if err := os.WriteFile(c.terminalMarker, []byte("terminal_event_observed"), 0o600); err != nil {
		os.Exit(6)
	}
	os.Exit(4) // the terminal event was observed by the adapter, but the Run result is not persisted
	return Execution{}, nil
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

func TestHelperCrashAfterPending(t *testing.T) {
	path := os.Getenv("WORKSTAVE_CRASH_PENDING_DB")
	if path == "" {
		t.Skip("helper process for TestCrashAfterPendingRemainsPending")
	}
	ctx := context.Background()
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	p := Params{SessionID: "s1", RunID: "r-pending", OpID: "op-pending", Dir: "/x", Prompt: "resume the same pending operation"}
	intent, err := intentFingerprint("create", p.SessionID, "codex", p.Dir, p.Prompt)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, created, err := db.BeginFirstRun(ctx, p.RunID, p.SessionID, "codex", p.Dir, "", p.OpID, intent); err != nil || !created {
		t.Fatalf("create pending run: created=%v err=%v", created, err)
	}
	os.Exit(5) // after the run commit, before dispatch authorization
	t.Fatal("helper returned; it should have terminated the process")
}

func TestHelperCrashAfterAcknowledged(t *testing.T) {
	path := os.Getenv("WORKSTAVE_CRASH_ACK_DB")
	if path == "" {
		t.Skip("helper process for TestCrashAfterAcknowledgedIsRecoveredAsUnknown")
	}
	ctx := context.Background()
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(ctx, "s1", "w1", "worker"); err != nil {
		t.Fatal(err)
	}
	_, _ = Create(ctx, db, crashingAfterAck{terminalMarker: os.Getenv("WORKSTAVE_CRASH_TERMINAL_MARKER")}, params)
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

func TestCrashAfterPendingRemainsPending(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending.db")
	ctx := context.Background()
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(ctx, "s1", "w1", "worker"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetWorkPaused(ctx, "w1", true); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperCrashAfterPending$")
	cmd.Env = append(os.Environ(), "WORKSTAVE_CRASH_PENDING_DB="+path)
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 5 {
		t.Fatalf("helper did not exit with code 5: err=%v output=%s", err, out)
	}

	db, err = store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if got, err := db.DispatchState(ctx, "r-pending"); err != nil || got != store.DispatchPending {
		t.Fatalf("dispatch after pending crash = %q err=%v, want pending", got, err)
	}
	rec, err := db.RecoverOnStart(ctx)
	if err != nil || rec.Pending != 1 || rec.Unknown != 0 {
		t.Fatalf("recovery = %+v err=%v, want 1 pending and 0 unknown", rec, err)
	}
	if err := db.AuthorizeDispatch(ctx, "r-pending"); err == nil {
		t.Fatal("pending run recovered while paused received a new dispatch authorization")
	}
	if err := db.SetWorkPaused(ctx, "w1", false); err != nil {
		t.Fatal(err)
	}
	p := Params{SessionID: "s1", RunID: "r-pending", OpID: "op-pending", Dir: "/x", Prompt: "resume the same pending operation"}
	f := &fake{name: "codex", ackID: "thread-pending-retry", out: outcome.Completed}
	if got, err := Create(ctx, db, f, p); err != nil || got != outcome.Completed || f.calls != 1 {
		t.Fatalf("same pending operation after unpause = %q err=%v harness_calls=%d, want one completed dispatch", got, err, f.calls)
	}
	if run, found, err := db.RunForOp(ctx, p.OpID); err != nil || !found || run.ID != p.RunID || run.Outcome.String != string(outcome.Completed) {
		t.Fatalf("pending operation after retry=%+v found=%v err=%v", run, found, err)
	}
}

func TestCrashAfterTerminalObservedBeforeResultWriteIsRecoveredAsUnknown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "acknowledged.db")
	terminalMarker := filepath.Join(t.TempDir(), "terminal-event")
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperCrashAfterAcknowledged$")
	cmd.Env = append(os.Environ(), "WORKSTAVE_CRASH_ACK_DB="+path, "WORKSTAVE_CRASH_TERMINAL_MARKER="+terminalMarker)
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 4 {
		t.Fatalf("helper did not exit with code 4: err=%v output=%s", err, out)
	}
	if data, err := os.ReadFile(terminalMarker); err != nil || string(data) != "terminal_event_observed" {
		t.Fatalf("terminal observation marker=%q err=%v", data, err)
	}

	ctx := context.Background()
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if got, err := db.DispatchState(ctx, "r1"); err != nil || got != store.DispatchAcknowledged {
		t.Fatalf("dispatch after acknowledged crash = %q err=%v, want acknowledged", got, err)
	}
	if id, err := db.NativeID(ctx, 1); err != nil || id != "thread-crash" {
		t.Fatalf("native id after acknowledged crash = %q err=%v, want thread-crash", id, err)
	}
	rec, err := db.RecoverOnStart(ctx)
	if err != nil || rec.Pending != 0 || rec.Unknown != 1 {
		t.Fatalf("recovery = %+v err=%v, want 0 pending and 1 unknown", rec, err)
	}
	r, ok, err := db.RunForOp(ctx, "op1")
	if err != nil || !ok || r.Outcome.String != string(outcome.Unknown) {
		t.Fatalf("run after recovery: ok=%v outcome=%q err=%v, want unknown", ok, r.Outcome.String, err)
	}
	f := &fake{name: "codex", ackID: "thread-duplicate", out: outcome.Completed}
	retry := params
	retry.RunID = "r-duplicate"
	if _, err := Create(ctx, db, f, retry); err == nil || f.calls != 0 {
		t.Fatalf("terminal-observed operation was relaunched: err=%v harness_calls=%d", err, f.calls)
	}
}
