package claudecode

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"testing"
	"time"
)

// Real-harness test: it spawns the installed Claude Code CLI and makes billable model requests.
// It only runs when WORKSTAVE_REAL_HARNESS=1, so a plain `go test` never spends money.

func newUUID(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func requireRealHarness(t *testing.T) {
	t.Helper()
	if os.Getenv("WORKSTAVE_REAL_HARNESS") != "1" {
		t.Skip("set WORKSTAVE_REAL_HARNESS=1 to run against the real Claude Code CLI (billable)")
	}
}

func assertSession(t *testing.T, res Result, id string) {
	t.Helper()
	if res.ExitCode != 0 {
		t.Fatalf("exit code %d, stderr: %s", res.ExitCode, res.Stderr)
	}
	want := []struct{ typ, sub string }{{"system", "init"}, {"assistant", ""}, {"result", "success"}}
	if len(res.Events) != len(want) {
		t.Fatalf("got %d events, want %d", len(res.Events), len(want))
	}
	for i, w := range want {
		ev := res.Events[i]
		if ev.Type != w.typ || ev.Subtype != w.sub {
			t.Errorf("event %d: got %s/%s, want %s/%s", i, ev.Type, ev.Subtype, w.typ, w.sub)
		}
		if ev.SessionID != id {
			t.Errorf("event %d: session_id %q, want %q", i, ev.SessionID, id)
		}
	}
}

func TestRealCreateThenResume(t *testing.T) {
	requireRealHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	dir := t.TempDir()
	id := newUUID(t)

	created, err := Run(ctx, Options{Dir: dir, SessionID: id, Prompt: "Reply with exactly the word: pong", MaxBudget: "0.05"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	assertSession(t, created, id)

	resumed, err := Run(ctx, Options{Dir: dir, SessionID: id, Resume: true, Prompt: "Reply with exactly the word: pong2", MaxBudget: "0.05"})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	assertSession(t, resumed, id)
}
