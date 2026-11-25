package claudecode

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workstave/internal/procgroup"
)

// TestRealResumeAfterKilledSideEffectTool kills Claude Code while a tool call with a real
// side effect (appending to marker.txt after a delay) is pending, then resumes the same
// session and records whether the tool was replayed. It only logs the observation.
func TestRealResumeAfterKilledSideEffectTool(t *testing.T) {
	requireRealHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	dir := t.TempDir()
	id := newUUID(t)
	marker := filepath.Join(dir, "marker.txt")
	const cmdline = "sleep 20 && echo x >> marker.txt"
	allowed := "Bash(" + cmdline + ")"

	var pid int
	killed := false
	res, err := Run(ctx, Options{
		Dir:          dir,
		SessionID:    id,
		Tools:        "Bash",
		AllowedTools: allowed,
		Prompt:       "Use the Bash tool to run exactly this command and nothing else: " + cmdline,
		MaxBudget:    "0.05",
		OnProcess:    func(p int) { pid = p },
		OnEvent: func(ev Event) {
			if ev.Type == "assistant" && !killed && strings.Contains(string(ev.Raw), `"type":"tool_use"`) {
				killed = true
				if kerr := procgroup.Kill(pid); kerr != nil {
					t.Logf("kill: %v", kerr)
				}
			}
		},
	})
	t.Logf("first run err=%v exit=%d events=%d killed after tool_use=%v", err, res.ExitCode, len(res.Events), killed)
	if !killed {
		t.Fatal("no tool_use event arrived; the dangling tool_use was not created")
	}
	time.Sleep(2 * time.Second)
	_, statErr := os.Stat(marker)
	t.Logf("marker exists right after kill: %v", statErr == nil)

	resumed, err := Run(ctx, Options{
		Dir:          dir,
		SessionID:    id,
		Resume:       true,
		Tools:        "Bash",
		AllowedTools: allowed,
		Prompt:       "Reply with exactly the word: resumed",
		MaxBudget:    "0.05",
	})
	t.Logf("resume err=%v exit=%d events=%d", err, resumed.ExitCode, len(resumed.Events))
	for i, ev := range resumed.Events {
		t.Logf("  resume event %d: %s/%s", i, ev.Type, ev.Subtype)
	}

	// A replayed tool would need ~20s to finish, so wait past that before reading the marker.
	time.Sleep(25 * time.Second)
	data, rerr := os.ReadFile(marker)
	t.Logf("marker after resume (+25s): exists=%v lines=%d", rerr == nil, strings.Count(string(data), "x"))
}
