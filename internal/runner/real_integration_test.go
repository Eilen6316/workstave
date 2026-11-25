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
func TestRealCreate(t *testing.T) {
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
			t.Logf("native id recorded: %s", id)
		})
	}
}
