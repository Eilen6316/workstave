package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"workstave/internal/lock"
	"workstave/internal/outcome"
	"workstave/internal/store"
)

// These tests use a real OS file lock and a real SQLite file in a temp directory. They check
// the CLI wiring only (lock before store, refusal while held); they run no harness.

func TestRecoverIsRefusedWhileStateDirIsLocked(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ws.db")

	held, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"recover", "-db", dbPath}); code != 1 {
		t.Errorf("recover exit code while the lock is held = %d, want 1", code)
	}
	if _, err := os.Stat(dbPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the database was created while the lock was held (stat err = %v); the lock must come before the store", err)
	}

	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"recover", "-db", dbPath}); code != 0 {
		t.Errorf("recover exit code after the lock was released = %d, want 0", code)
	}
	// The command released its own lock on exit.
	l, err := lock.Acquire(dir)
	if err != nil {
		t.Fatalf("recover left the state directory locked: %v", err)
	}
	_ = l.Release()
}

func TestResumeRequiresSession(t *testing.T) {
	if code := run([]string{"resume", "-harness", "codex", "-prompt", "continue", "-db", filepath.Join(t.TempDir(), "ws.db")}); code != 2 {
		t.Fatalf("resume without -session exit code = %d, want 2", code)
	}
}

func TestHelperHoldRuntimeForPause(t *testing.T) {
	path := os.Getenv("WORKSTAVE_PAUSE_OWNER_DB")
	if path == "" {
		t.Skip("helper process for TestPauseAndUnpauseWhileRuntimeOwnsStateDirectory")
	}
	ctx := context.Background()
	db, closeDB, err := openDB(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(ctx, "s1", "w1", "worker"); err != nil {
		t.Fatal(err)
	}
	bindingID, _, _, err := db.BeginFirstRun(ctx, "r1", "s1", "codex", t.TempDir(), "", "op1", "intent-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AuthorizeDispatch(ctx, "r1"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetNativeID(ctx, bindingID, "thread-1", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkAcknowledged(ctx, "r1"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(ctx, "s2", "w1", "worker"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := db.BeginFirstRun(ctx, "r2", "s2", "codex", t.TempDir(), "", "op2", "intent-2"); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(os.Stdout, "runtime-ready")
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishRun(ctx, "r1", outcome.Completed); err != nil {
		t.Fatal(err)
	}
	closeDB()
}

func TestPauseAndUnpauseWhileRuntimeOwnsStateDirectory(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ws.db")
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperHoldRuntimeForPause$")
	cmd.Env = append(os.Environ(), "WORKSTAVE_PAUSE_OWNER_DB="+dbPath)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	ready := make(chan bool, 1)
	go func() {
		line, err := bufio.NewReader(stdout).ReadString('\n')
		ready <- err == nil && line == "runtime-ready\n"
	}()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("runtime owner exited without becoming ready")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runtime owner did not become ready")
	}

	if code := run([]string{"pause", "-work", "w1", "-db", dbPath}); code != 0 {
		t.Fatalf("pause while runtime owns state directory exit code = %d, want 0", code)
	}
	control, err := store.OpenControl(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := control.DispatchState(context.Background(), "r1"); err != nil || got != store.DispatchAcknowledged {
		t.Fatalf("pause changed the already-authorized run: dispatch=%q err=%v", got, err)
	}
	if err := control.AuthorizeDispatch(context.Background(), "r2"); err == nil {
		t.Fatal("new dispatch was authorized after the control command committed pause")
	}
	if err := control.Close(); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"unpause", "-work", "w1", "-db", dbPath}); code != 0 {
		t.Fatalf("unpause while runtime owns state directory exit code = %d, want 0", code)
	}
	control, err = store.OpenControl(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := control.AuthorizeDispatch(context.Background(), "r2"); err != nil {
		t.Fatalf("dispatch after unpause: %v", err)
	}
	if err := control.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := lock.Acquire(dir); !errors.Is(err, lock.ErrHeld) {
		t.Fatalf("control commands released runtime lock: err=%v, want ErrHeld", err)
	}
	control, err = store.OpenControl(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	if got, err := control.DispatchState(context.Background(), "r2"); err != nil || got != store.DispatchDispatching {
		t.Fatalf("dispatch after unpause = %q err=%v, want dispatching", got, err)
	}
	if err := control.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintln(stdin, "stop"); err != nil {
		t.Fatal(err)
	}
	_ = stdin.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("runtime owner exited with error: %v", err)
	}
	waited = true
	db, closeDB, err := openDB(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	r, ok, err := db.RunForOp(context.Background(), "op1")
	if err != nil || !ok || r.Outcome.String != string(outcome.Completed) {
		t.Fatalf("in-flight run after pause: ok=%v outcome=%q err=%v, want completed", ok, r.Outcome.String, err)
	}
}

func TestRunExitCodeRequiresPersistedSuccess(t *testing.T) {
	persistErr := errors.New("finish run: database is closed")
	cases := []struct {
		name string
		out  outcome.Outcome
		err  error
		want int
	}{
		{"completed", outcome.Completed, nil, 0},
		{"completed with persistence error", outcome.Completed, persistErr, 1},
		{"unknown", outcome.Unknown, nil, 1},
		{"failed", outcome.Failed, nil, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := runExitCode(c.out, c.err); got != c.want {
				t.Fatalf("runExitCode(%s, %v) = %d, want %d", c.out, c.err, got, c.want)
			}
		})
	}
}

func TestHelperHoldStateOwnerForMailboxAcceptance(t *testing.T) {
	path := os.Getenv("WORKSTAVE_MAILBOX_OWNER_DB")
	if path == "" {
		t.Skip("helper process for TestAskAcceptsWhileStateOwnerIsActive")
	}
	if code := run([]string{"daemon", "-db", path}); code != 0 {
		t.Fatalf("daemon exit code=%d", code)
	}
}

func TestWorkAndConversationCommandsPersistTheirIdentities(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "workstave.db")
	if code := run([]string{"work", "create", "-id", "w-cli", "-harness", "claude", "-dir", dir, "-db", dbPath}); code != 0 {
		t.Fatalf("work create exit code=%d", code)
	}
	if code := run([]string{"conversation", "create", "-work", "w-cli", "-id", "c-cli", "-db", dbPath}); code != 0 {
		t.Fatalf("conversation create exit code=%d", code)
	}
	db, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	work, err := db.Work(context.Background(), "w-cli")
	if err != nil || work.MainHarness != "claude-code" || work.WorkingDir != dir {
		t.Fatalf("Work=%+v err=%v", work, err)
	}
	conversation, err := db.Conversation(context.Background(), "c-cli")
	if err != nil || conversation.WorkID != "w-cli" {
		t.Fatalf("Conversation=%+v err=%v", conversation, err)
	}
	if code := run([]string{"history", "-conversation", "c-cli", "-db", dbPath}); code != 0 {
		t.Fatalf("empty history exit code=%d", code)
	}
	if code := run([]string{"state", "-work", "w-cli", "-db", dbPath}); code != 0 {
		t.Fatalf("empty state exit code=%d", code)
	}
}

func TestAskAcceptsWhileStateOwnerIsActiveAndReusesOperationIdentity(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "workstave.db")
	if code := run([]string{"work", "create", "-id", "w-cli", "-dir", dir, "-db", dbPath}); code != 0 {
		t.Fatalf("work create exit code=%d", code)
	}
	if code := run([]string{"conversation", "create", "-work", "w-cli", "-id", "c-cli", "-db", dbPath}); code != 0 {
		t.Fatalf("conversation create exit code=%d", code)
	}
	if code := run([]string{"pause", "-work", "w-cli", "-db", dbPath}); code != 0 {
		t.Fatalf("pause exit code=%d", code)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperHoldStateOwnerForMailboxAcceptance$")
	cmd.Env = append(os.Environ(), "WORKSTAVE_MAILBOX_OWNER_DB="+dbPath)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	ready := make(chan bool, 1)
	go func() {
		line, err := bufio.NewReader(stdout).ReadString('\n')
		ready <- err == nil && line == "daemon=ready\n"
	}()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("owner helper exited before taking the lock")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("owner helper did not become ready")
	}
	args := []string{"ask", "-conversation", "c-cli", "-message", "request-1", "-prompt", "Please change the export to TSV.", "-db", dbPath}
	if code := run(args); code != 0 {
		t.Fatalf("ask while owner active exit code=%d", code)
	}
	control, err := store.OpenControl(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	first, err := control.MailboxByMessage(context.Background(), "request-1")
	if err != nil || first.State != "pending" || first.OpID == "" || first.OpID == "main-request-1" {
		t.Fatalf("accepted queued input=%+v err=%v", first, err)
	}
	if err := control.Close(); err != nil {
		t.Fatal(err)
	}
	if code := run(args); code != 0 {
		t.Fatalf("repeat ask after lost receipt exit code=%d", code)
	}
	control, err = store.OpenControl(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := control.MailboxByMessage(context.Background(), "request-1")
	if err != nil || repeated.OpID != first.OpID || repeated.Sequence != first.Sequence {
		t.Fatalf("repeated acceptance=%+v err=%v, want original identity", repeated, err)
	}
	if err := control.Close(); err != nil {
		t.Fatal(err)
	}
	conflict := []string{"ask", "-conversation", "c-cli", "-message", "request-1", "-prompt", "Use a different requirement.", "-db", dbPath}
	if code := run(conflict); code != 1 {
		t.Fatalf("same request identity with different intent exit code=%d, want 1", code)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	waited = true
	lockAfter, err := lock.Acquire(dir)
	if err != nil {
		t.Fatalf("daemon left the state directory locked: %v", err)
	}
	_ = lockAfter.Release()
}
