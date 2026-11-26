package lock

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"
)

// These tests use real OS file locks on a temp directory. The cross-process test re-executes
// the test binary as the lock holder, then kills it.

func TestSecondAcquireIsRefusedUntilRelease(t *testing.T) {
	dir := t.TempDir()
	first, err := Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(dir); !errors.Is(err, ErrHeld) {
		t.Fatalf("second Acquire error = %v, want ErrHeld", err)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	second, err := Acquire(dir)
	if err != nil {
		t.Fatalf("Acquire after Release: %v", err)
	}
	_ = second.Release()
}

func TestReleaseIsIdempotent(t *testing.T) {
	l, err := Acquire(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("second Release: %v", err)
	}
	var none *Lock
	if err := none.Release(); err != nil {
		t.Fatalf("nil Release: %v", err)
	}
}

// TestHelperHold is the lock holder for TestLockIsFreedWhenHolderIsKilled. It does nothing
// unless the parent test asks for it through the environment.
func TestHelperHold(t *testing.T) {
	dir := os.Getenv("WORKSTAVE_LOCK_HELPER_DIR")
	if dir == "" {
		t.Skip("helper process for TestLockIsFreedWhenHolderIsKilled")
	}
	l, err := Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	fmt.Println("locked")
	time.Sleep(60 * time.Second) // deadline so a failed parent cannot leave an orphan
}

func TestLockIsFreedWhenHolderIsKilled(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperHold$")
	cmd.Env = append(os.Environ(), "WORKSTAVE_LOCK_HELPER_DIR="+dir)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	ready := make(chan bool, 1)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if sc.Text() == "locked" {
				ready <- true
				return
			}
		}
		ready <- false
	}()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("helper exited without taking the lock")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("helper did not take the lock in time")
	}

	if _, err := Acquire(dir); !errors.Is(err, ErrHeld) {
		t.Fatalf("Acquire while another process holds the lock = %v, want ErrHeld", err)
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

	deadline := time.Now().Add(5 * time.Second)
	for {
		l, err := Acquire(dir)
		if err == nil {
			_ = l.Release()
			return
		}
		if !errors.Is(err, ErrHeld) || time.Now().After(deadline) {
			t.Fatalf("lock not freed after the holder was killed: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
