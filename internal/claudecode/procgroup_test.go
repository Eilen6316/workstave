package claudecode

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

// TestCancelKillsDirectChild checks, without any harness, what exec.CommandContext does on
// Windows: the direct child must exit after cancel. Grandchildren are NOT asserted here;
// whether they survive is measured separately and reported as evidence.
func TestCancelKillsDirectChild(t *testing.T) {
	// This test deliberately leaves a grandchild behind; remove only the ones it created.
	before := pingPIDs(t)
	t.Cleanup(func() {
		for _, p := range pingPIDs(t) {
			if !contains(before, p) {
				_ = exec.Command("taskkill", "/F", "/PID", p).Run()
			}
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "cmd", "/c", "ping -n 30 127.0.0.1 >NUL")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	time.Sleep(500 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		t.Logf("direct child pid %d exited after cancel, wait err=%v", pid, err)
	case <-time.After(10 * time.Second):
		t.Fatalf("direct child pid %d still running 10s after cancel", pid)
	}

	out, _ := exec.Command("tasklist", "/FI", "IMAGENAME eq PING.EXE", "/NH").CombinedOutput()
	t.Logf("ping.exe after cancel (grandchild survivors, if any):\n%s", out)
}
