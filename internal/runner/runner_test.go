package runner

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"workstave/internal/outcome"
	"workstave/internal/store"
)

// These tests use a fake harness to check the order of persistence and the error paths of
// Create. They are not evidence about the real Claude Code or Codex behaviour.

type fake struct {
	name, preID, ackID string
	out                outcome.Outcome
	err                error
	during             func()
	calls              int
}

func (f *fake) Name() string        { return f.name }
func (f *fake) NewNativeID() string { return f.preID }
func (f *fake) Execute(_ context.Context, _ Request, onAcknowledged func(string)) (outcome.Outcome, error) {
	f.calls++
	if f.during != nil {
		f.during()
	}
	if f.ackID != "" {
		onAcknowledged(f.ackID)
	}
	return f.out, f.err
}

var params = Params{SessionID: "s1", RunID: "r1", OpID: "op1", Dir: "/x", Prompt: "p"}

func open(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "ws.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.CreateSession(context.Background(), "s1", "w1", "worker"); err != nil {
		t.Fatal(err)
	}
	return db
}

func storedOutcome(t *testing.T, db *store.DB) string {
	t.Helper()
	r, ok, err := db.RunForOp(context.Background(), "op1")
	if err != nil || !ok {
		t.Fatalf("run for op1: ok=%v err=%v", ok, err)
	}
	return r.Outcome.String
}

func TestRunAndPreassignedIDAreRecordedBeforeLaunch(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	f := &fake{name: "claude-code", preID: "native-pre", ackID: "native-pre", out: outcome.Completed}
	f.during = func() {
		if open, err := db.UnfinishedRuns(ctx); err != nil || len(open) != 1 {
			t.Errorf("run not persisted before launch: runs=%d err=%v", len(open), err)
		}
		if id, err := db.NativeID(ctx, 1); err != nil || id != "native-pre" {
			t.Errorf("pre-assigned id not persisted before launch: id=%q err=%v", id, err)
		}
	}
	got, err := Create(ctx, db, f, params)
	if err != nil || got != outcome.Completed {
		t.Fatalf("got %s err=%v", got, err)
	}
	if s := storedOutcome(t, db); s != string(outcome.Completed) {
		t.Fatalf("stored outcome %q", s)
	}
	if s, err := db.DispatchState(ctx, "r1"); err != nil || s != store.DispatchAcknowledged {
		t.Fatalf("dispatch after correlated acknowledgement = %q err=%v, want acknowledged", s, err)
	}
}

func TestHarnessAssignedIDIsRecordedWhenReported(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	f := &fake{name: "codex", ackID: "thread-1", out: outcome.Completed}
	f.during = func() {
		if id, err := db.NativeID(ctx, 1); err != nil || id != "" {
			t.Errorf("native id should be unknown before the harness reports it: id=%q err=%v", id, err)
		}
	}
	if _, err := Create(ctx, db, f, params); err != nil {
		t.Fatal(err)
	}
	if id, err := db.NativeID(ctx, 1); err != nil || id != "thread-1" {
		t.Fatalf("native id %q err=%v, want thread-1", id, err)
	}
	if s, err := db.DispatchState(ctx, "r1"); err != nil || s != store.DispatchAcknowledged {
		t.Fatalf("dispatch after correlated acknowledgement = %q err=%v, want acknowledged", s, err)
	}
}

func TestDispatchingIsCommittedBeforeLaunch(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	f := &fake{name: "codex", out: outcome.Completed}
	f.during = func() {
		if s, err := db.DispatchState(ctx, "r1"); err != nil || s != store.DispatchDispatching {
			t.Errorf("dispatch state during launch = %q err=%v, want dispatching", s, err)
		}
	}
	if _, err := Create(ctx, db, f, params); err != nil {
		t.Fatal(err)
	}
}

func TestPausedWorkDoesNotLaunchHarness(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	if err := db.SetWorkPaused(ctx, "w1", true); err != nil {
		t.Fatal(err)
	}
	f := &fake{name: "codex", ackID: "thread-should-not-start", out: outcome.Completed}
	if _, err := Create(ctx, db, f, params); err == nil {
		t.Fatal("Create succeeded for paused work")
	}
	if f.calls != 0 {
		t.Fatalf("harness launched %d times for paused work, want 0", f.calls)
	}
	if got, err := db.DispatchState(ctx, "r1"); err != nil || got != store.DispatchPending {
		t.Fatalf("dispatch for paused work = %q err=%v, want pending", got, err)
	}
}

func TestPauseAfterAuthorizationLeavesRunInFlight(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	f := &fake{name: "codex", ackID: "thread-in-flight", out: outcome.Completed}
	f.during = func() {
		if err := db.SetWorkPaused(ctx, "w1", true); err != nil {
			t.Errorf("pause while authorized run is entering harness: %v", err)
		}
	}
	got, err := Create(ctx, db, f, params)
	if err != nil || got != outcome.Completed {
		t.Fatalf("authorized run after pause got %q err=%v, want completed", got, err)
	}
	if f.calls != 1 {
		t.Fatalf("authorized run launched %d times, want 1", f.calls)
	}
	if state, err := db.DispatchState(ctx, "r1"); err != nil || state != store.DispatchAcknowledged {
		t.Fatalf("dispatch after in-flight run = %q err=%v, want acknowledged", state, err)
	}
}

func TestExecuteErrorIsRecordedAsUnknown(t *testing.T) {
	db := open(t)
	boom := errors.New("boom")
	got, err := Create(context.Background(), db, &fake{name: "codex", out: outcome.Completed, err: boom}, params)
	if !errors.Is(err, boom) || got != outcome.Unknown {
		t.Fatalf("got %s err=%v, want unknown and boom", got, err)
	}
	if s := storedOutcome(t, db); s != string(outcome.Unknown) {
		t.Fatalf("stored outcome %q, want unknown", s)
	}
}

func TestEmptyOutcomeIsRecordedAsUnknown(t *testing.T) {
	db := open(t)
	got, err := Create(context.Background(), db, &fake{name: "codex"}, params)
	if err != nil || got != outcome.Unknown {
		t.Fatalf("got %s err=%v, want unknown", got, err)
	}
	if s := storedOutcome(t, db); s != string(outcome.Unknown) {
		t.Fatalf("stored outcome %q, want unknown", s)
	}
}

func TestRepeatedOpIsNotLaunchedAgain(t *testing.T) {
	db := open(t)
	f := &fake{name: "codex", out: outcome.Completed}
	if _, err := Create(context.Background(), db, f, params); err != nil {
		t.Fatal(err)
	}
	retry := params
	retry.RunID = "r-retry"
	if _, err := Create(context.Background(), db, f, retry); err == nil {
		t.Fatal("a repeated op id was accepted")
	}
	if f.calls != 1 {
		t.Fatalf("harness launched %d times, want 1", f.calls)
	}
}

func TestOutcomeIsRecordedAfterContextCancel(t *testing.T) {
	db := open(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &fake{name: "codex", out: outcome.Unknown, err: context.Canceled, during: cancel}
	if _, err := Create(ctx, db, f, params); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
	if open, err := db.UnfinishedRuns(context.Background()); err != nil || len(open) != 0 {
		t.Fatalf("run left unfinished after cancel: runs=%d err=%v", len(open), err)
	}
	if s := storedOutcome(t, db); s != string(outcome.Unknown) {
		t.Fatalf("stored outcome %q, want unknown", s)
	}
}
