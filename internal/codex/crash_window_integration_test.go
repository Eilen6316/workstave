package codex

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workstave/internal/procgroup"
)

func codexHome() string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h
	}
	u, _ := os.UserHomeDir()
	return filepath.Join(u, ".codex")
}

// TestRealCrashAfterThreadStarted simulates the window where Codex has created a thread but
// the caller dies before persisting the native id. The id seen in thread.started is only
// logged for comparison; discovery then uses the working directory and time window alone.
// It records evidence and asserts only that the window was actually exercised.
func TestRealCrashAfterThreadStarted(t *testing.T) {
	requireRealHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	dir := t.TempDir()
	started := time.Now().Add(-2 * time.Second)

	var pid int
	killed := false
	res, err := Run(ctx, Options{
		Dir:       dir,
		Prompt:    "Reply with exactly the word: pong",
		OnProcess: func(p int) { pid = p },
		OnEvent: func(ev Event) {
			if ev.Type == "thread.started" && !killed {
				killed = true
				if kerr := procgroup.Kill(pid); kerr != nil {
					t.Logf("kill: %v", kerr)
				}
			}
		},
	})
	t.Logf("run err=%v exit=%d completed=%v failed=%v events=%d", err, res.ExitCode, res.Completed, res.Failed, len(res.Events))
	t.Logf("thread id seen before the kill (treated as lost): %q", res.ThreadID)
	if !killed {
		t.Fatal("thread.started never arrived; the crash window was not exercised")
	}

	// rollout files store cwd JSON-escaped; match the full path, not the weak basename "001".
	marker := strings.ReplaceAll(dir, "\\", "\\\\")
	found := 0
	_ = filepath.WalkDir(filepath.Join(codexHome(), "sessions"), func(p string, d fs.DirEntry, werr error) error {
		if werr != nil || d.IsDir() || !strings.HasPrefix(d.Name(), "rollout-") {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil || info.ModTime().Before(started) {
			return nil
		}
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		if strings.Contains(string(data), marker) {
			found++
			t.Logf("discovered by cwd+time: %s (name has lost id: %v, lines: %d)", p, strings.Contains(d.Name(), res.ThreadID), strings.Count(string(data), "\n"))
		}
		return nil
	})
	t.Logf("rollout files matching cwd marker %q since start: %d", marker, found)
}
