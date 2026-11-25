package claudecode

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestRealCancelDuringToolExecution cancels while a real tool subprocess is running.
// Run only starts the CLI with exec.CommandContext, so here the test itself finds the CLI
// process by its session id on the command line and kills its tree with taskkill /T /F.
// It records what the stream looked like afterwards; it does not assume a result event.
func TestRealCancelDuringToolExecution(t *testing.T) {
	requireRealHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	dir := t.TempDir()
	id := newUUID(t)

	before := map[string]bool{}
	for _, p := range pingPIDs(t) {
		before[p] = true
	}

	var once sync.Once
	killed := make(chan string, 1)
	go func() {
		// Poll for the tool's ping.exe; once it exists the tool call is really executing.
		deadline := time.Now().Add(90 * time.Second)
		for time.Now().Before(deadline) {
			for _, p := range pingPIDs(t) {
				if before[p] {
					continue
				}
				once.Do(func() {
					out, _ := exec.Command("powershell", "-NoProfile", "-Command",
						"(Get-CimInstance Win32_Process -Filter \"Name='claude.exe' OR Name='node.exe'\" | Where-Object { $_.CommandLine -like '*"+id+"*' } | Select-Object -First 1).ProcessId").Output()
					cliPID := strings.TrimSpace(string(out))
					if cliPID != "" {
						_ = exec.Command("taskkill", "/T", "/F", "/PID", cliPID).Run()
					}
					killed <- cliPID + " (tool ping pid " + p + ")"
				})
				return
			}
			time.Sleep(300 * time.Millisecond)
		}
	}()

	res, err := Run(ctx, Options{
		Dir:          dir,
		SessionID:    id,
		Tools:        "Bash",
		AllowedTools: "Bash(ping *)",
		Prompt:       "Use the Bash tool to run exactly this command and nothing else: ping -n 40 127.0.0.1",
		MaxBudget:    "0.05",
	})
	t.Logf("run returned err=%v exit=%d stderr=%q", err, res.ExitCode, res.Stderr)

	select {
	case k := <-killed:
		t.Logf("killed CLI pid %s", k)
	default:
		t.Fatal("the tool's ping.exe never appeared; cancellation during tool execution was not exercised")
	}

	hasResult := false
	for i, ev := range res.Events {
		t.Logf("  event %d: %s/%s", i, ev.Type, ev.Subtype)
		if ev.Type == "result" {
			hasResult = true
		}
	}
	t.Logf("terminal result event present after kill: %v", hasResult)

	time.Sleep(1 * time.Second)
	for _, p := range pingPIDs(t) {
		if !before[p] {
			_ = exec.Command("taskkill", "/F", "/PID", p).Run()
			t.Errorf("tool grandchild ping pid %s survived the tree kill (now cleaned up)", p)
		}
	}
	_ = strconv.Itoa
}
