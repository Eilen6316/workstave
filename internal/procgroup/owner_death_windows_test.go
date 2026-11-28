//go:build windows

package procgroup

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestOwnerDeathRemovesWholeTree kills only the owner process, with no tree kill. The owner had
// contained its child tree in a Job Object, so the operating system must remove the grandchild
// when the owner's handle closes. Without Attach the grandchild keeps beating.
func TestOwnerDeathRemovesWholeTree(t *testing.T) {
	beat := filepath.Join(t.TempDir(), "beat")
	owner := exec.Command(os.Args[0], "-test.run=^$")
	owner.Env = append(os.Environ(), roleEnv+"=owner", beatEnv+"="+beat, "PROCGROUP_TEST_WAIT_GO=1")
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = owner.Wait(); close(done) }()
	// If the assertion fails the grandchild must still stop, so ask it to exit.
	t.Cleanup(func() {
		_ = os.WriteFile(beat+".stop", nil, 0o600)
		_ = owner.Process.Kill()
	})

	waitBeating(t, beat)
	if err := owner.Process.Kill(); err != nil {
		t.Fatalf("kill owner only: %v", err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("owner still running after Kill")
	}
	if !stopped(t, beat) {
		t.Fatal("grandchild kept beating after the owner died: the Job Object did not contain the tree")
	}
}
