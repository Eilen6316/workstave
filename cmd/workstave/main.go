// Command workstave is the minimal CLI over the store and runner packages.
//
//	workstave run -harness codex|claude -prompt TEXT [-dir DIR] [-db PATH] [-op ID] [-budget USD]
//	workstave recover [-db PATH]
package main

import (
	"context"
	"crypto/rand"
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
		fmt.Fprintln(os.Stderr, "usage: workstave run|recover [flags]")
		return 2
	}
	switch args[0] {
	case "run":
		return cmdRun(args[1:])
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

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b) // documented to never return an error
	return fmt.Sprintf("%x", b)
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
	var h runner.Harness
	switch *harness {
	case "codex":
		h = runner.Codex{}
	case "claude":
		h = runner.Claude{MaxBudget: *budget}
	default:
		fmt.Fprintln(os.Stderr, "workstave run: -harness must be codex or claude")
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
	if out != outcome.Completed {
		return 1
	}
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
	n, err := db.MarkUnfinishedUnknown(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "workstave recover: %v\n", err)
		return 1
	}
	fmt.Printf("marked %d unfinished run(s) as unknown\n", n)
	return 0
}
