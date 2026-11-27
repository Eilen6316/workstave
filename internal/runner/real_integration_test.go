package runner

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"workstave/internal/outcome"
)

// Real-harness test: runs both adapters against the installed CLIs with the user's own
// configuration (provider, endpoint and keys are never overridden). It makes billable
// requests and only runs when WORKSTAVE_REAL_HARNESS=1.
func TestRealCreateThenResume(t *testing.T) {
	if os.Getenv("WORKSTAVE_REAL_HARNESS") != "1" {
		t.Skip("set WORKSTAVE_REAL_HARNESS=1 to run against the real harnesses (billable)")
	}
	cases := []struct {
		name string
		h    Harness
	}{
		{"claude-code", Claude{MaxBudget: "0.05"}},
		{"codex", Codex{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := open(t)
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			defer cancel()
			p := Params{SessionID: "s1", RunID: "r1", OpID: "op1", Dir: t.TempDir(), Prompt: "Reply with exactly the word: pong"}
			got, err := Create(ctx, db, c.h, p)
			if err != nil || got != outcome.Completed {
				t.Fatalf("outcome=%s err=%v, want completed", got, err)
			}
			id, err := db.NativeID(ctx, 1)
			if err != nil || id == "" {
				t.Fatalf("native id not recorded: id=%q err=%v", id, err)
			}
			if s := storedOutcome(t, db); s != string(outcome.Completed) {
				t.Fatalf("stored outcome %q, want completed", s)
			}
			resumed, err := Resume(ctx, db, c.h, ResumeParams{
				SessionID: p.SessionID, RunID: "r2", OpID: "op2", Prompt: "Reply with exactly the word: pong2",
			})
			if err != nil || resumed != outcome.Completed {
				t.Fatalf("resume outcome=%s err=%v, want completed", resumed, err)
			}
			active, err := db.ActiveBinding(ctx, p.SessionID)
			if err != nil || active.NativeID != id || active.WorkingDir != p.Dir {
				t.Fatalf("active binding after resume = %+v err=%v, want same native id and working directory", active, err)
			}
			r, ok, err := db.RunForOp(ctx, "op2")
			if err != nil || !ok || r.Outcome.String != string(outcome.Completed) {
				t.Fatalf("resumed run: ok=%v outcome=%q err=%v, want completed", ok, r.Outcome.String, err)
			}
		})
	}
}

// TestRealSafePointCrossHarnessHandoff is a deliberately minimal real-harness experiment. The
// operator summary is supplied explicitly because Workstave does not yet persist a Work goal,
// constraints, or an event cursor from which to compile one.
func TestRealSafePointCrossHarnessHandoff(t *testing.T) {
	if os.Getenv("WORKSTAVE_REAL_HARNESS") != "1" {
		t.Skip("set WORKSTAVE_REAL_HARNESS=1 to run the billable cross-harness handoff experiment")
	}
	db := open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	dir := t.TempDir()
	workID, sessionID := "w1", "s1"

	firstOutcome, err := Create(ctx, db, Claude{MaxBudget: "0.05", Model: "claude-haiku-4-5-20251001"}, Params{
		SessionID: sessionID, RunID: "r-handoff-claude", OpID: "op-handoff-claude", Dir: dir,
		Prompt: "Reach this safe point and reply with exactly HANDOFF_READY. Do not use tools or perform external actions.",
	})
	if err != nil || firstOutcome != outcome.Completed {
		t.Fatalf("Claude source run did not complete (outcome=%s, error-present=%t)", firstOutcome, err != nil)
	}
	firstRun, ok, err := db.RunForOp(ctx, "op-handoff-claude")
	if err != nil || !ok || firstRun.Outcome.String != string(outcome.Completed) {
		t.Fatalf("Claude source Run is not persisted as completed (found=%t, error-present=%t)", ok, err != nil)
	}
	firstBinding, err := db.ActiveBinding(ctx, sessionID)
	if err != nil || firstBinding.Harness != "claude-code" || firstBinding.NativeID == "" {
		t.Fatalf("Claude source binding is unavailable (found-harness=%t, native-id-present=%t)", firstBinding.Harness == "claude-code", firstBinding.NativeID != "")
	}
	if firstRun.BindingID.Int64 != firstBinding.ID {
		t.Fatal("Claude source Run does not refer to the active binding")
	}

	// This explicit receipt record uses persisted identities and the observed terminal outcome.
	// The store does not yet contain a Work goal or constraints from which to build a fuller handoff.
	handoff := fmt.Sprintf("Safe-point receipt. Work %s. Logical Session %s. Previous Run %s completed. Reply exactly HANDOFF_ACCEPTED.", workID, sessionID, firstRun.ID)
	secondOutcome, err := Create(ctx, db, Codex{}, Params{
		SessionID: sessionID, RunID: "r-handoff-codex", OpID: "op-handoff-codex", Dir: dir,
		Prompt: handoff,
	})
	if err != nil || secondOutcome != outcome.Completed {
		t.Fatalf("Codex candidate run did not complete (outcome=%s, error-present=%t)", secondOutcome, err != nil)
	}
	secondRun, ok, err := db.RunForOp(ctx, "op-handoff-codex")
	if err != nil || !ok || secondRun.Outcome.String != string(outcome.Completed) {
		t.Fatalf("Codex candidate Run is not persisted as completed (found=%t, error-present=%t)", ok, err != nil)
	}
	secondBinding, err := db.ActiveBinding(ctx, sessionID)
	if err != nil || secondBinding.Harness != "codex" || secondBinding.NativeID == "" {
		t.Fatalf("Codex candidate binding is unavailable (found-harness=%t, native-id-present=%t)", secondBinding.Harness == "codex", secondBinding.NativeID != "")
	}
	if secondBinding.ID == firstBinding.ID || secondRun.BindingID.Int64 != secondBinding.ID {
		t.Fatal("Codex candidate did not replace the active Native Binding for the same logical Session")
	}
	if secondBinding.WorkingDir != dir || firstBinding.WorkingDir != dir || secondRun.SessionID != firstRun.SessionID {
		t.Fatal("cross-harness handoff changed the logical Session or working directory")
	}
	if prior, ok, err := db.RunForOp(ctx, "op-handoff-claude"); err != nil || !ok || prior.Outcome.String != string(outcome.Completed) {
		t.Fatalf("Claude source Run history was not preserved (found=%t, error-present=%t)", ok, err != nil)
	}
}
