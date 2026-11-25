package claudecode

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func pingPIDs(t *testing.T) []string {
	t.Helper()
	out, err := exec.Command("tasklist", "/FI", "IMAGENAME eq PING.EXE", "/FO", "CSV", "/NH").CombinedOutput()
	if err != nil {
		t.Fatalf("tasklist: %v: %s", err, out)
	}
	var pids []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(strings.TrimSpace(line), "\",\"")
		if len(f) > 1 {
			pids = append(pids, strings.Trim(f[1], "\""))
		}
	}
	return pids
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// TestTaskkillTreeRemovesGrandchild measures whether `taskkill /T /F` on the direct child
// also removes the grandchild. It records the PING.EXE pids before and after so that the
// result is evidence about this one grandchild, not about unrelated processes.
func TestTaskkillTreeRemovesGrandchild(t *testing.T) {
	before := map[string]bool{}
	for _, p := range pingPIDs(t) {
		before[p] = true
	}

	cmd := exec.CommandContext(context.Background(), "cmd", "/c", "ping -n 60 127.0.0.1 >NUL")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	time.Sleep(700 * time.Millisecond)

	var mine []string
	for _, p := range pingPIDs(t) {
		if !before[p] {
			mine = append(mine, p)
		}
	}
	if len(mine) != 1 {
		t.Fatalf("expected exactly one new PING.EXE grandchild, got %v", mine)
	}
	t.Logf("direct child pid %d, grandchild ping pid %s", cmd.Process.Pid, mine[0])

	out, err := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).CombinedOutput()
	t.Logf("taskkill err=%v output=%s", err, strings.TrimSpace(string(out)))

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("direct child still running after taskkill /T /F")
	}

	time.Sleep(500 * time.Millisecond)
	for _, p := range pingPIDs(t) {
		if p == mine[0] {
			// Clean up our own leftover so the test never leaves a process behind.
			_ = exec.Command("taskkill", "/F", "/PID", p).Run()
			t.Fatalf("grandchild ping pid %s survived taskkill /T /F", p)
		}
	}
	t.Logf("grandchild ping pid %s is gone after taskkill /T /F", mine[0])
}
