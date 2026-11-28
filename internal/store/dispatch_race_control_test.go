package store

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestConcurrentControlPauseAndRuntimeDispatch races Pause on an OpenControl connection against a
// dispatch authorization on the Runtime's connection, both on one SQLite file (Engineering
// Contracts C3.5). The two handles have separate connection pools, as the CLI and the Runtime do.
// They still live in one process, so this does not cover a second OS process. Pause is delayed by
// a varying amount so that both commit orders occur; the test asserts what must hold for either
// order and logs how often each was observed.
func TestConcurrentControlPauseAndRuntimeDispatch(t *testing.T) {
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
	control, err := OpenControl(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = control.Close() })

	const rounds = 200
	authorizedFirst, rejectedByPause := 0, 0
	for i := 0; i < rounds; i++ {
		runID := fmt.Sprintf("ctl-run-%d", i)
		seedRunForSession(t, runtime, runID, fmt.Sprintf("ctl-session-%d", i))
		delay := time.Duration(i%20) * 50 * time.Microsecond

		start := make(chan struct{})
		var ready, done sync.WaitGroup
		var authErr, pauseErr error
		ready.Add(2)
		done.Add(2)
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			authErr = runtime.AuthorizeDispatch(ctx, runID)
		}()
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			time.Sleep(delay)
			pauseErr = control.SetWorkPaused(ctx, "w1", true)
		}()
		ready.Wait()
		close(start)
		done.Wait()

		if pauseErr != nil {
			t.Fatalf("round %d: Pause did not commit: %v", i, pauseErr)
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

		afterID := fmt.Sprintf("ctl-after-%d", i)
		seedRunForSession(t, runtime, afterID, fmt.Sprintf("ctl-after-session-%d", i))
		if err := runtime.AuthorizeDispatch(ctx, afterID); err == nil {
			t.Fatalf("round %d: a run was authorized after Pause committed", i)
		}
		if err := control.SetWorkPaused(ctx, "w1", false); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("%d rounds: authorization committed first in %d, Pause committed first in %d", rounds, authorizedFirst, rejectedByPause)
}
