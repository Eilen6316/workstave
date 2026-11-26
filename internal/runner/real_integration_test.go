package runner

import (
	"context"
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
