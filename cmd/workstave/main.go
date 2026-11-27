// Command workstave is the minimal CLI over the store and runner packages.
//
//	workstave run -harness codex|claude -prompt TEXT [-dir DIR] [-db PATH] [-op ID] [-budget USD]
//	workstave resume -session ID -harness codex|claude -prompt TEXT [-db PATH] [-op ID] [-budget USD]
//	workstave pause -work ID [-db PATH]
//	workstave unpause -work ID [-db PATH]
//	workstave recover [-db PATH]
package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"

	"workstave/internal/lock"
	"workstave/internal/outcome"
	"workstave/internal/runner"
	"workstave/internal/store"
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: workstave run|resume|pause|unpause|recover [flags]")
		return 2
	}
	switch args[0] {
	case "run":
		return cmdRun(args[1:])
	case "resume":
		return cmdResume(args[1:])
	case "pause":
		return cmdSetPaused(args[1:], true)
	case "unpause":
		return cmdSetPaused(args[1:], false)
	case "recover":
		return cmdRecover(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "workstave: unknown command %q\n", args[0])
		return 2
	}
}

func defaultDB() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "workstave", "workstave.db")
}

// openDB takes the state-directory lock, then opens the store. The returned close function
// closes the store first and releases the lock after, so the lock outlives every write.
func openDB(ctx context.Context, path string) (*store.DB, func(), error) {
	l, err := lock.Acquire(filepath.Dir(path))
	if err != nil {
		return nil, nil, err
	}
	db, err := store.Open(ctx, path)
	if err != nil {
		_ = l.Release()
		return nil, nil, err
	}
	return db, func() { _ = db.Close(); _ = l.Release() }, nil
}

// openControlDB uses the runtime lock when no owner is active. If a runtime owns the state
// directory, it opens only the existing current-schema database; the control command performs
// no migrations and writes only the short pause-state transaction.
func openControlDB(ctx context.Context, path string) (*store.DB, func(), error) {
	db, closeDB, err := openDB(ctx, path)
	if err == nil {
		return db, closeDB, nil
	}
	if !errors.Is(err, lock.ErrHeld) {
		return nil, nil, err
	}
	db, err = store.OpenControl(ctx, path)
	if err != nil {
		return nil, nil, err
	}
	return db, func() { _ = db.Close() }, nil
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b) // documented to never return an error
	return fmt.Sprintf("%x", b)
}

func runExitCode(out outcome.Outcome, err error) int {
	if err != nil || out != outcome.Completed {
		return 1
	}
	return 0
}

func makeHarness(name, budget string) (runner.Harness, error) {
	switch name {
	case "codex":
		return runner.Codex{}, nil
	case "claude":
		return runner.Claude{MaxBudget: budget}, nil
	default:
		return nil, fmt.Errorf("-harness must be codex or claude")
	}
}

func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	harness := fs.String("harness", "", "codex or claude")
	prompt := fs.String("prompt", "", "prompt to send")
	dir := fs.String("dir", "", "working directory (default: current); use a unique one per run for codex")
	dbPath := fs.String("db", defaultDB(), "database path")
	op := fs.String("op", "", "operation id; reuse it only to retry the same operation")
	budget := fs.String("budget", "0.05", "claude only: USD cap per run")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	h, err := makeHarness(*harness, *budget)
	if err != nil {
		fmt.Fprintf(os.Stderr, "workstave run: %v\n", err)
		return 2
	}
	if *prompt == "" {
		fmt.Fprintln(os.Stderr, "workstave run: -prompt is required")
		return 2
	}
	workDir := *dir
	if workDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(os.Stderr, "workstave run: %v\n", err)
			return 1
		}
		workDir = wd
	}
	opID := *op
	if opID == "" {
		opID = "op-" + newID()
	}

	// Ctrl+C cancels ctx, which kills the harness process tree; the outcome is still recorded.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	db, closeDB, err := openDB(ctx, *dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "workstave run: %v\n", err)
		return 1
	}
	defer closeDB()

	sessionID, runID := "s-"+newID(), "r-"+newID()
	if err := db.CreateSession(ctx, sessionID, "default", "worker"); err != nil {
		fmt.Fprintf(os.Stderr, "workstave run: %v\n", err)
		return 1
	}
	out, err := runner.Create(ctx, db, h, runner.Params{SessionID: sessionID, RunID: runID, OpID: opID, Dir: workDir, Prompt: *prompt})
	fmt.Printf("session=%s run=%s op=%s outcome=%s\n", sessionID, runID, opID, out)
	if err != nil {
		fmt.Fprintf(os.Stderr, "workstave run: %v\n", err)
	}
	return runExitCode(out, err)
}

func cmdResume(args []string) int {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	sessionID := fs.String("session", "", "logical session id to resume")
	harness := fs.String("harness", "", "codex or claude; must match the saved binding")
	prompt := fs.String("prompt", "", "prompt to send to the existing native session")
	dbPath := fs.String("db", defaultDB(), "database path")
	op := fs.String("op", "", "operation id; reuse it only to retry this same resume request")
	budget := fs.String("budget", "0.05", "claude only: USD cap per run")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *sessionID == "" {
		fmt.Fprintln(os.Stderr, "workstave resume: -session is required")
		return 2
	}
	if *prompt == "" {
		fmt.Fprintln(os.Stderr, "workstave resume: -prompt is required")
		return 2
	}
	h, err := makeHarness(*harness, *budget)
	if err != nil {
		fmt.Fprintf(os.Stderr, "workstave resume: %v\n", err)
		return 2
	}
	opID := *op
	if opID == "" {
		opID = "op-" + newID()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	db, closeDB, err := openDB(ctx, *dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "workstave resume: %v\n", err)
		return 1
	}
	defer closeDB()

	runID := "r-" + newID()
	out, err := runner.Resume(ctx, db, h, runner.ResumeParams{
		SessionID: *sessionID, RunID: runID, OpID: opID, Prompt: *prompt,
	})
	fmt.Printf("session=%s run=%s op=%s outcome=%s\n", *sessionID, runID, opID, out)
	if err != nil {
		fmt.Fprintf(os.Stderr, "workstave resume: %v\n", err)
	}
	return runExitCode(out, err)
}

func cmdSetPaused(args []string, paused bool) int {
	name := "pause"
	if !paused {
		name = "unpause"
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	workID := fs.String("work", "", "work whose future dispatches should be paused or resumed")
	dbPath := fs.String("db", defaultDB(), "database path")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *workID == "" {
		fmt.Fprintf(os.Stderr, "workstave %s: -work is required\n", name)
		return 2
	}
	db, closeDB, err := openControlDB(context.Background(), *dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "workstave %s: %v\n", name, err)
		return 1
	}
	defer closeDB()
	if err := db.SetWorkPaused(context.Background(), *workID, paused); err != nil {
		fmt.Fprintf(os.Stderr, "workstave %s: %v\n", name, err)
		return 1
	}
	state := "paused"
	if !paused {
		state = "unpaused"
	}
	fmt.Printf("work=%s dispatch=%s\n", *workID, state)
	return 0
}

// cmdRecover marks runs without an outcome as unknown. It takes the state-directory lock, so it
// refuses to run while another workstave process is executing; otherwise a run in flight
// elsewhere would be marked unknown too.
func cmdRecover(args []string) int {
	fs := flag.NewFlagSet("recover", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDB(), "database path")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx := context.Background()
	db, closeDB, err := openDB(ctx, *dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "workstave recover: %v\n", err)
		return 1
	}
	defer closeDB()
	rec, err := db.RecoverOnStart(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "workstave recover: %v\n", err)
		return 1
	}
	fmt.Printf("recovered: %d pending run(s) kept, %d run(s) marked unknown\n", rec.Pending, rec.Unknown)
	return 0
}
