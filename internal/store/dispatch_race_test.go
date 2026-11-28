package store

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// TestConcurrentPauseAndDispatchAuthorization submits Pause and a dispatch authorization at the
// same moment from two goroutines against a real SQLite file (Engineering Contracts C3.5). Which
// commit wins is timing dependent, so the test asserts what must hold for either order and logs
// how often each order was observed. It covers the store's authorization point only: it does not
// call a Harness and says nothing about external calls that start after Pause returns.
func TestConcurrentPauseAndDispatchAuthorization(t *testing.T) {
	ctx := context.Background()
	db := openWithSession(t)
	const rounds = 200
	authorizedFirst, rejectedByPause := 0, 0
	for i := 0; i < rounds; i++ {
		runID := fmt.Sprintf("race-run-%d", i)
		seedRunForSession(t, db, runID, fmt.Sprintf("race-session-%d", i))

		start := make(chan struct{})
		var ready, done sync.WaitGroup
		var authErr, pauseErr error
		ready.Add(2)
		done.Add(2)
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			authErr = db.AuthorizeDispatch(ctx, runID)
		}()
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			pauseErr = db.SetWorkPaused(ctx, "w1", true)
		}()
		ready.Wait()
		close(start)
		done.Wait()

		if pauseErr != nil {
			t.Fatalf("round %d: Pause did not commit: %v", i, pauseErr)
		}
		state := dispatchOf(t, db, runID)
		if authErr == nil {
			if state != DispatchDispatching {
				t.Fatalf("round %d: authorization succeeded but the run is %q, want dispatching (in flight)", i, state)
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

		// Pause has committed, so no run may obtain a new authorization until it is lifted.
		afterID := fmt.Sprintf("race-after-%d", i)
		seedRunForSession(t, db, afterID, fmt.Sprintf("race-after-session-%d", i))
		if err := db.AuthorizeDispatch(ctx, afterID); err == nil {
			t.Fatalf("round %d: a run was authorized after Pause committed", i)
		}
		if got := dispatchOf(t, db, afterID); got != DispatchPending {
			t.Fatalf("round %d: run after Pause is %q, want pending", i, got)
		}
		if err := db.SetWorkPaused(ctx, "w1", false); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("%d rounds: authorization committed first in %d, Pause committed first in %d", rounds, authorizedFirst, rejectedByPause)
}
