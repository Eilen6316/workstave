package codex

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// Real-harness test: spawns the installed Codex CLI with the user's own configuration
// (endpoint, key, provider are never overridden) and makes billable requests.
// It only runs when WORKSTAVE_REAL_HARNESS=1.
func requireRealHarness(t *testing.T) {
	t.Helper()
	if os.Getenv("WORKSTAVE_REAL_HARNESS") != "1" {
		t.Skip("set WORKSTAVE_REAL_HARNESS=1 to run against the real Codex CLI (billable)")
	}
}

func TestRedact(t *testing.T) {
	in := []byte(`{"message":"Incorrect API key provided: sk-abcd1234*****wxyz."}`)
	out := string(Redact(in))
	if strings.Contains(out, "abcd1234") || !strings.Contains(out, "sk-<redacted>") {
		t.Fatalf("key not redacted: %s", out)
	}
}

func TestRealCreateThenResume(t *testing.T) {
	requireRealHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	dir := t.TempDir()

	created, err := Run(ctx, Options{Dir: dir, Prompt: "Reply with exactly the word: pong"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ThreadID == "" || !created.Completed || created.Failed || created.ExitCode != 0 {
		t.Fatalf("create not clean: thread=%q completed=%v failed=%v exit=%d stderr=%s",
			created.ThreadID, created.Completed, created.Failed, created.ExitCode, created.Stderr)
	}

	resumed, err := Run(ctx, Options{Dir: dir, ThreadID: created.ThreadID, Prompt: "Reply with exactly the word: pong2"})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumed.ThreadID != created.ThreadID {
		t.Errorf("resume thread id %q, want %q", resumed.ThreadID, created.ThreadID)
	}
	if !resumed.Completed || resumed.Failed || resumed.ExitCode != 0 {
		t.Fatalf("resume not clean: completed=%v failed=%v exit=%d stderr=%s",
			resumed.Completed, resumed.Failed, resumed.ExitCode, resumed.Stderr)
	}
}
