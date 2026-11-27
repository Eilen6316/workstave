package procgroup

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// The test binary re-executes itself in three roles so that no platform-specific
// command (ping, sleep, tasklist) is needed:
//
//	parent     - starts a grandchild and then waits forever
//	grandchild - appends a heartbeat line to a file every 50ms
const roleEnv = "PROCGROUP_TEST_ROLE"
const beatEnv = "PROCGROUP_TEST_BEAT"

func TestMain(m *testing.M) {
	switch os.Getenv(roleEnv) {
	case "parent":
		gc := exec.Command(os.Args[0], "-test.run=^$")
		gc.Env = append(os.Environ(), roleEnv+"=grandchild")
		if err := gc.Start(); err != nil {
			os.Exit(2)
		}
		for {
			time.Sleep(time.Hour)
		}
	case "grandchild":
		f, err := os.OpenFile(os.Getenv(beatEnv), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			os.Exit(2)
		}
		// Exit when asked through a stop file, or after a hard deadline so a failed
		// test can never leave this process running.
		start := time.Now()
		for time.Since(start) < 60*time.Second {
			if _, err := os.Stat(os.Getenv(beatEnv) + ".stop"); err == nil {
				os.Exit(0)
			}
			_, _ = f.WriteString("x")
			time.Sleep(50 * time.Millisecond)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func beatSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// waitBeating waits until the grandchild has written at least one heartbeat.
func waitBeating(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if beatSize(t, path) > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("grandchild never started beating")
}

// stopped reports whether the heartbeat file stopped growing over a window
// comfortably longer than the 50ms beat interval.
func stopped(t *testing.T, path string) bool {
	t.Helper()
	a := beatSize(t, path)
	time.Sleep(500 * time.Millisecond)
	return beatSize(t, path) == a
}

func TestKillRemovesGrandchild(t *testing.T) {
	beat := filepath.Join(t.TempDir(), "beat")
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), roleEnv+"=parent", beatEnv+"="+beat)
	Configure(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()

	waitBeating(t, beat)
	if err := Kill(pid); err != nil {
		t.Fatalf("Kill(%d): %v", pid, err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("direct child %d still running after Kill", pid)
	}
	if !stopped(t, beat) {
		t.Fatalf("grandchild of pid %s kept beating after Kill: the process tree was not removed", strconv.Itoa(pid))
	}
}

// TestPlainKillLeavesGrandchild records the baseline without procgroup: killing only the
// direct child. It logs the observation and never asserts, because the outcome is
// platform-specific; the grandchild is cleaned up through Kill on a second handle.
func TestPlainKillLeavesGrandchild(t *testing.T) {
	beat := filepath.Join(t.TempDir(), "beat")
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), roleEnv+"=parent", beatEnv+"="+beat)
	Configure(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Killing only the direct child orphans the grandchild, and Kill(pid) can no longer
	// find the tree once the parent is gone, so the grandchild is told to exit instead.
	t.Cleanup(func() {
		_ = os.WriteFile(beat+".stop", nil, 0o600)
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) && !stopped(t, beat) {
		}
	})
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()

	waitBeating(t, beat)
	_ = cmd.Process.Kill()
	<-done
	t.Logf("after killing only the direct child, grandchild stopped beating: %v", stopped(t, beat))
}
