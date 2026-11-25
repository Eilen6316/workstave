package codex

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func pingPIDs(t *testing.T) map[string]bool {
	t.Helper()
	out, err := exec.Command("tasklist", "/FI", "IMAGENAME eq PING.EXE", "/FO", "CSV", "/NH").CombinedOutput()
	if err != nil {
		t.Fatalf("tasklist: %v: %s", err, out)
	}
	pids := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(strings.TrimSpace(line), "\",\"")
		if len(f) > 1 {
			pids[strings.Trim(f[1], "\"")] = true
		}
	}
	return pids
}

// TestRealCancelDuringToolExecution kills the Codex process tree while a real tool
// subprocess (ping) is running, then records what the event stream and process table look
// like. It does not assume a terminal event exists.
func TestRealCancelDuringToolExecution(t *testing.T) {
	requireRealHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dir := t.TempDir()

	before := pingPIDs(t)

	var mu sync.Mutex
	var codexPID int
	var lastTree string
	var once, startedOnce sync.Once
	killed := make(chan string, 1)
	started := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		// Wait for Codex to report the command as started, then give ping time to appear.
		select {
		case <-started:
		case <-time.After(150 * time.Second):
			return
		}
		deadline := time.Now().Add(90 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			pid := codexPID
			mu.Unlock()
			tree := descendants(t, pid)
			mu.Lock()
			lastTree = strings.Join(tree, ", ")
			mu.Unlock()
			if hasProc(tree, "powershell.exe") {
				once.Do(func() {
					_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).Run()
					killed <- "root pid " + strconv.Itoa(pid) + " tree before kill: " + strings.Join(tree, ", ")
				})
				return
			}
			time.Sleep(300 * time.Millisecond)
		}
	}()

	res, err := Run(ctx, Options{
		Dir:    dir,
		Prompt: "Run exactly this shell command and nothing else: ping -n 40 127.0.0.1",
		OnEvent: func(ev Event) {
			if ev.Type == "item.started" && ev.ItemType == "command_execution" {
				startedOnce.Do(func() { close(started) })
			}
		},
		OnProcess: func(pid int) {
			mu.Lock()
			codexPID = pid
			mu.Unlock()
		},
	})
	<-watcherDone // the watcher may still be inside taskkill when Run returns
	t.Logf("run returned err=%v exit=%d completed=%v failed=%v thread=%q", err, res.ExitCode, res.Completed, res.Failed, res.ThreadID)
	t.Logf("stderr: %q", res.Stderr)
	for i, ev := range res.Events {
		raw := string(ev.Raw)
		if len(raw) > 300 {
			raw = raw[:300]
		}
		t.Logf("raw event %d: %s", i, raw)
	}

	select {
	case k := <-killed:
		t.Logf("killed %s", k)
	default:
		toolStarted := false
		select {
		case <-started:
			toolStarted = true
		default:
		}
		mu.Lock()
		lt := lastTree
		mu.Unlock()
		t.Fatalf("cancellation was not exercised: command_execution item.started seen=%v, powershell.exe never appeared in the process tree; last tree seen: [%s]", toolStarted, lt)
	}

	for i, ev := range res.Events {
		t.Logf("  event %d: %s item=%s", i, ev.Type, ev.ItemType)
	}

	time.Sleep(1 * time.Second)
	for p := range pingPIDs(t) {
		if !before[p] {
			_ = exec.Command("taskkill", "/F", "/PID", p).Run()
			t.Errorf("tool grandchild ping pid %s survived the tree kill (now cleaned up)", p)
		}
	}
}
