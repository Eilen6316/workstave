//go:build windows

package claudecode

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"workstave/internal/procgroup"
)

// TestHelperClaudeOwner is the owner process of a real Claude Code run. It does nothing unless
// the parent test asks for it through the environment.
func TestHelperClaudeOwner(t *testing.T) {
	dir := os.Getenv("WORKSTAVE_OWNER_DIR")
	if dir == "" {
		t.Skip("helper process for TestRealOwnerCrashLeavesClaudeProcessObservation")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	logf := func(format string, args ...any) {
		f, err := os.OpenFile(os.Getenv("WORKSTAVE_OWNER_LOG"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		defer f.Close()
		fmt.Fprintf(f, format+"\n", args...)
	}
	res, runErr := Run(ctx, Options{
		Dir:          dir,
		SessionID:    os.Getenv("WORKSTAVE_OWNER_SESSION"),
		Tools:        "Bash",
		AllowedTools: "Bash(ping *)",
		Prompt:       "Use the Bash tool to run exactly this command and nothing else: ping -n 90 127.0.0.1",
		MaxBudget:    "0.05",
		OnProcess: func(pid int) {
			_ = os.WriteFile(os.Getenv("WORKSTAVE_OWNER_PIDFILE"), []byte(strconv.Itoa(pid)), 0o600)
		},
		OnEvent: func(ev Event) {
			raw := string(ev.Raw)
			if len(raw) > 400 {
				raw = raw[:400]
			}
			logf("event %s/%s: %s", ev.Type, ev.Subtype, raw)
		},
	})
	logf("Run returned err=%v exit=%d events=%d stderr=%q", runErr, res.ExitCode, len(res.Events), res.Stderr)
}

func processAlive(t *testing.T, pid int) bool {
	t.Helper()
	out, err := exec.Command("tasklist", "/FI", "PID eq "+strconv.Itoa(pid), "/FO", "CSV", "/NH").CombinedOutput()
	if err != nil {
		t.Fatalf("tasklist: %v: %s", err, out)
	}
	return strings.Contains(string(out), `"`+strconv.Itoa(pid)+`"`)
}

// TestRealOwnerCrashLeavesClaudeProcessObservation kills only the owner of a real Claude Code
// stdio run while its tool subprocess is executing (no tree kill), then records for 60 seconds
// whether the Claude process and the tool process are still alive. It asserts nothing about
// Claude's behavior: it is an observation for the owner-crash recovery boundary.
func TestRealOwnerCrashLeavesClaudeProcessObservation(t *testing.T) {
	requireRealHarness(t)
	dir := t.TempDir()
	pidFile := filepath.Join(t.TempDir(), "claude.pid")
	logFile := filepath.Join(t.TempDir(), "owner.log")

	before := map[string]bool{}
	for _, p := range pingPIDs(t) {
		before[p] = true
	}

	owner := exec.Command(os.Args[0], "-test.run=^TestHelperClaudeOwner$")
	owner.Env = append(os.Environ(),
		"WORKSTAVE_OWNER_DIR="+dir,
		"WORKSTAVE_OWNER_SESSION="+newUUID(t),
		"WORKSTAVE_OWNER_PIDFILE="+pidFile,
		"WORKSTAVE_OWNER_LOG="+logFile,
	)
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	ownerDone := make(chan struct{})
	go func() { _ = owner.Wait(); close(ownerDone) }()

	claudePID := 0
	t.Cleanup(func() {
		_ = exec.Command("taskkill", "/F", "/PID", strconv.Itoa(owner.Process.Pid)).Run()
		if claudePID != 0 {
			_ = procgroup.Kill(claudePID)
		}
		for _, p := range pingPIDs(t) {
			if !before[p] {
				_ = exec.Command("taskkill", "/F", "/PID", p).Run()
			}
		}
	})

	toolPID := ""
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) && toolPID == "" {
		for _, p := range pingPIDs(t) {
			if !before[p] {
				toolPID = p
				break
			}
		}
		if toolPID == "" {
			time.Sleep(300 * time.Millisecond)
		}
	}
	if data, err := os.ReadFile(pidFile); err == nil {
		claudePID, _ = strconv.Atoi(strings.TrimSpace(string(data)))
	}
	if toolPID == "" || claudePID == 0 {
		if data, err := os.ReadFile(logFile); err == nil {
			t.Logf("owner helper log:\n%s", data)
		} else {
			t.Logf("owner helper wrote no log: %v", err)
		}
		t.Fatalf("owner crash was not exercised: tool ping pid=%q recorded Claude pid=%d", toolPID, claudePID)
	}

	if err := exec.Command("taskkill", "/F", "/PID", strconv.Itoa(owner.Process.Pid)).Run(); err != nil {
		t.Fatalf("kill owner only: %v", err)
	}
	<-ownerDone
	t.Logf("killed owner pid %d only; recorded Claude process pid %d; tool ping pid %s", owner.Process.Pid, claudePID, toolPID)

	for i := 0; i <= 12; i++ {
		t.Logf("t+%02ds: claude process alive=%v tool ping alive=%v", i*5, processAlive(t, claudePID), processAlive(t, mustAtoi(t, toolPID)))
		if i < 12 {
			time.Sleep(5 * time.Second)
		}
	}
}

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
