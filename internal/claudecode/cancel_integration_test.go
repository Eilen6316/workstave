package claudecode

import (
	"context"
	"testing"
	"time"
)

// TestRealCancelMidRun records what the real CLI does when the caller cancels the context
// after the first assistant event. It asserts only that Run returns and the process is gone;
// everything else is logged as evidence, not assumed.
func TestRealCancelMidRun(t *testing.T) {
	requireRealHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	dir := t.TempDir()
	id := newUUID(t)

	var cancelledAfter int
	start := time.Now()
	res, err := Run(ctx, Options{
		Dir:       dir,
		SessionID: id,
		Prompt:    "Count from 1 to 2000, one number per line, with no other text.",
		MaxBudget: "0.05",
		OnEvent: func(ev Event) {
			if ev.Type == "assistant" && cancelledAfter == 0 {
				cancelledAfter = 1
				cancel()
			}
		},
	})
	elapsed := time.Since(start)

	t.Logf("run returned after %s, err=%v", elapsed, err)
	t.Logf("cancel requested after first assistant event: %v", cancelledAfter == 1)
	t.Logf("exit code: %d", res.ExitCode)
	t.Logf("stderr: %q", res.Stderr)
	t.Logf("events observed: %d", len(res.Events))
	hasResult := false
	for i, ev := range res.Events {
		t.Logf("  event %d: %s/%s", i, ev.Type, ev.Subtype)
		if ev.Type == "result" {
			hasResult = true
		}
	}
	t.Logf("terminal result event present: %v", hasResult)

	if cancelledAfter != 1 {
		t.Fatal("no assistant event arrived before the run ended; cancellation was never exercised")
	}
	if elapsed > 60*time.Second {
		t.Errorf("run took %s after cancel; process may not have been stopped", elapsed)
	}
}
