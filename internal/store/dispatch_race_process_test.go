package store

import (
	"context"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestHelperCrossProcessPause is the Pause side of TestConcurrentPauseAcrossOSProcesses. It does
// nothing unless the parent test asks for it through the environment. It opens the state through
// OpenControl like the CLI, signals readiness, and commits Pause as soon as the start file exists.
func TestHelperCrossProcessPause(t *testing.T) {
	dbPath := os.Getenv("WORKSTAVE_RACE_DB")
	if dbPath == "" {
		t.Skip("helper process for TestConcurrentPauseAcrossOSProcesses")
	}
	ctx := context.Background()
	control, err := OpenControl(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	dir := os.Getenv("WORKSTAVE_RACE_DIR")
	if err := os.WriteFile(filepath.Join(dir, "ready"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(dir, "start")); err == nil {
			if micros, convErr := strconv.Atoi(os.Getenv("WORKSTAVE_RACE_DELAY_US")); convErr == nil {
				time.Sleep(time.Duration(micros) * time.Microsecond)
			}
			if err := control.SetWorkPaused(ctx, "w1", true); err != nil {
				t.Fatalf("Pause did not commit: %v", err)
			}
			return
		}
	}
	t.Fatal("start signal never arrived")
}

// TestConcurrentPauseAcrossOSProcesses races Pause committed by a second OS process against a
// dispatch authorization in this process, on one SQLite file (Engineering Contracts C3.5). The
// Pause is delayed in the helper by a varying amount so both commit orders can occur; the test asserts
// what must hold for either order and logs how often each was observed. It covers the store's
// authorization point only, not a Harness call that starts after Pause returns.
func TestConcurrentPauseAcrossOSProcesses(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ws.db")
	runtime, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	if err := runtime.CreateSession(ctx, "s1", "w1", "worker"); err != nil {
		t.Fatal(err)
	}

	const rounds = 40
	authorizedFirst, rejectedByPause := 0, 0
	for i := 0; i < rounds; i++ {
		runID := fmt.Sprintf("proc-run-%d", i)
		seedRunForSession(t, runtime, runID, fmt.Sprintf("proc-session-%d", i))
		dir := t.TempDir()
		helper := osexec.Command(os.Args[0], "-test.run=^TestHelperCrossProcessPause$")
		// Pause is delayed in the helper, by a varying amount, while this process authorizes
		// immediately, so that both commit orders can occur.
		helper.Env = append(os.Environ(), "WORKSTAVE_RACE_DB="+path, "WORKSTAVE_RACE_DIR="+dir,
			"WORKSTAVE_RACE_DELAY_US="+strconv.Itoa((i%10)*500))
		var output strings.Builder
		helper.Stdout, helper.Stderr = &output, &output
		if err := helper.Start(); err != nil {
			t.Fatalf("round %d: start helper: %v", i, err)
		}
		ready := false
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Millisecond) {
			if _, err := os.Stat(filepath.Join(dir, "ready")); err == nil {
				ready = true
				break
			}
		}
		if !ready {
			_ = helper.Process.Kill()
			_ = helper.Wait()
			t.Fatalf("round %d: helper never became ready: %s", i, output.String())
		}

		if err := os.WriteFile(filepath.Join(dir, "start"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		authErr := runtime.AuthorizeDispatch(ctx, runID)
		if err := helper.Wait(); err != nil {
			t.Fatalf("round %d: helper failed: %v: %s", i, err, output.String())
		}

		state := dispatchOf(t, runtime, runID)
		if authErr == nil {
			if state != DispatchDispatching {
				t.Fatalf("round %d: authorization succeeded but the run is %q, want dispatching", i, state)
			}
			authorizedFirst++
		} else {
			if !strings.Contains(authErr.Error(), "is not an authorized pending run") {
				t.Fatalf("round %d: authorization failed with something other than a clean rejection: %v", i, authErr)
			}
			if state != DispatchPending {
				t.Fatalf("round %d: authorization was rejected but the run is %q, want pending", i, state)
			}
			rejectedByPause++
		}

		// The helper has exited, so Pause is committed: no run may be authorized until it is lifted.
		afterID := fmt.Sprintf("proc-after-%d", i)
		seedRunForSession(t, runtime, afterID, fmt.Sprintf("proc-after-session-%d", i))
		if err := runtime.AuthorizeDispatch(ctx, afterID); err == nil {
			t.Fatalf("round %d: a run was authorized after Pause committed", i)
		}
		if err := runtime.SetWorkPaused(ctx, "w1", false); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("%d rounds across two OS processes: authorization committed first in %d, Pause committed first in %d", rounds, authorizedFirst, rejectedByPause)
}
