package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"time"

	"workstave/internal/lock"
	"workstave/internal/runner"
	"workstave/internal/store"
	"workstave/internal/workflow"
)

func commandHarness(name string) (string, error) {
	switch name {
	case "codex":
		return "codex", nil
	case "claude", "claude-code":
		return "claude-code", nil
	default:
		return "", fmt.Errorf("Harness must be codex or claude")
	}
}

func cmdWork(args []string) int {
	if len(args) == 0 || args[0] != "create" {
		fmt.Fprintln(os.Stderr, "usage: workstave work create [-id ID] [-harness codex|claude] [-dir DIR] [-db PATH]")
		return 2
	}
	fs := flag.NewFlagSet("work create", flag.ContinueOnError)
	workID := fs.String("id", "", "Work identity")
	harnessName := fs.String("harness", "codex", "initial Main Harness")
	workingDir := fs.String("dir", "", "Work directory; defaults to the current directory")
	dbPath := fs.String("db", defaultDB(), "database path")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	harness, err := commandHarness(*harnessName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "workstave work create: %v\n", err)
		return 2
	}
	dir := *workingDir
	if dir == "" {
		dir, err = os.Getwd()
		if err != nil {
			fmt.Fprintf(os.Stderr, "workstave work create: %v\n", err)
			return 1
		}
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "workstave work create: %v\n", err)
		return 1
	}
	id := *workID
	if id == "" {
		id = "w-" + newID()
	}
	mainSessionID := "main-" + newID()
	db, closeDB, err := openControlDB(context.Background(), *dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "workstave work create: %v\n", err)
		return 1
	}
	defer closeDB()
	if err := db.CreateWorkWithHarness(context.Background(), id, mainSessionID, harness, dir); err != nil {
		fmt.Fprintf(os.Stderr, "workstave work create: %v\n", err)
		return 1
	}
	fmt.Printf("work=%s main_session=%s harness=%s dir=%s\n", id, mainSessionID, harness, dir)
	return 0
}

func cmdConversation(args []string) int {
	if len(args) == 0 || args[0] != "create" {
		fmt.Fprintln(os.Stderr, "usage: workstave conversation create -work ID [-id ID] [-db PATH]")
		return 2
	}
	fs := flag.NewFlagSet("conversation create", flag.ContinueOnError)
	workID := fs.String("work", "", "Work that owns this Conversation")
	conversationID := fs.String("id", "", "Conversation identity")
	dbPath := fs.String("db", defaultDB(), "database path")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *workID == "" {
		fmt.Fprintln(os.Stderr, "workstave conversation create: -work is required")
		return 2
	}
	id := *conversationID
	if id == "" {
		id = "c-" + newID()
	}
	db, closeDB, err := openControlDB(context.Background(), *dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "workstave conversation create: %v\n", err)
		return 1
	}
	defer closeDB()
	if err := db.CreateConversation(context.Background(), id, *workID); err != nil {
		fmt.Fprintf(os.Stderr, "workstave conversation create: %v\n", err)
		return 1
	}
	fmt.Printf("conversation=%s work=%s\n", id, *workID)
	return 0
}

func cmdAsk(args []string) int {
	fs := flag.NewFlagSet("ask", flag.ContinueOnError)
	conversationID := fs.String("conversation", "", "Conversation to reply in")
	messageID := fs.String("message", "", "Stable request identity; reuse it after a lost acceptance receipt")
	prompt := fs.String("prompt", "", "natural-language message")
	experimentHarness := fs.String("experiment-harness", "", "explicitly rebuild Main on codex or claude")
	budget := fs.String("budget", "0.05", "claude only: USD cap per Run")
	dbPath := fs.String("db", defaultDB(), "database path")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *conversationID == "" || *prompt == "" {
		fmt.Fprintln(os.Stderr, "workstave ask: -conversation and -prompt are required")
		return 2
	}
	var experiment string
	if *experimentHarness != "" {
		var err error
		experiment, err = commandHarness(*experimentHarness)
		if err != nil {
			fmt.Fprintf(os.Stderr, "workstave ask: %v\n", err)
			return 2
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	db, closeDB, err := openDB(ctx, *dbPath)
	busy := false
	if errors.Is(err, lock.ErrHeld) {
		db, err = store.OpenControl(ctx, *dbPath)
		busy = err == nil
		if err == nil {
			closeDB = func() { _ = db.Close() }
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "workstave ask: %v\n", err)
		return 1
	}
	defer closeDB()
	if !busy {
		if _, err := recoverAtOwnerStart(ctx, db); err != nil {
			fmt.Fprintf(os.Stderr, "workstave ask: runtime recovery: %v\n", err)
			return 1
		}
	}
	id := *messageID
	if id == "" {
		id = "msg-" + newID()
	}
	var accepted store.MailboxItem
	if experiment != "" {
		accepted, err = workflow.AcceptCrossHarnessExperiment(ctx, db, id, *conversationID, experiment, *prompt)
	} else {
		accepted, err = workflow.Accept(ctx, db, id, *conversationID, *prompt)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "workstave ask: accept message: %v\n", err)
		return 1
	}
	fmt.Printf("accepted message=%s operation=%s sequence=%d\n", accepted.MessageID, accepted.OpID, accepted.Sequence)
	if !busy {
		conversation, err := db.Conversation(ctx, *conversationID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "workstave ask: %v\n", err)
			return 1
		}
		processErr := workflow.ProcessPending(ctx, db, conversation.WorkID, "", func(name string) (runner.Harness, error) {
			return makeHarness(name, *budget)
		})
		if processErr != nil {
			fmt.Fprintf(os.Stderr, "workstave ask: process message %s: %v\n", accepted.MessageID, processErr)
			item, lookupErr := db.MailboxByMessage(ctx, accepted.MessageID)
			if lookupErr != nil {
				fmt.Fprintf(os.Stderr, "workstave ask: read message state: %v\n", lookupErr)
				return 1
			}
			if item.Result != "failed" && item.Result != "unknown" {
				return 1
			}
		}
	}
	item, err := db.MailboxByMessage(ctx, accepted.MessageID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "workstave ask: read acceptance result: %v\n", err)
		return 1
	}
	fmt.Printf("message=%s operation=%s state=%s result=%s\n", item.MessageID, item.OpID, item.State, item.Result)
	if item.ResultDetail != "" {
		fmt.Printf("detail=%s\n", item.ResultDetail)
	}
	if item.Result == "failed" || item.Result == "unknown" {
		return 1
	}
	if mainRun, found, err := db.MainRunForMessage(ctx, accepted.MessageID); err != nil {
		fmt.Fprintf(os.Stderr, "workstave ask: read Main Run: %v\n", err)
		return 1
	} else if found && mainRun.OutputMessageID != "" {
		reply, err := db.MessageByID(ctx, mainRun.OutputMessageID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "workstave ask: read reply: %v\n", err)
			return 1
		}
		fmt.Println(reply.Body)
	}
	return 0
}

func cmdHistory(args []string) int {
	fs := flag.NewFlagSet("history", flag.ContinueOnError)
	conversationID := fs.String("conversation", "", "Conversation identity")
	dbPath := fs.String("db", defaultDB(), "database path")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *conversationID == "" {
		fmt.Fprintln(os.Stderr, "workstave history: -conversation is required")
		return 2
	}
	db, closeDB, err := openControlDB(context.Background(), *dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "workstave history: %v\n", err)
		return 1
	}
	defer closeDB()
	messages, err := db.ConversationMessages(context.Background(), *conversationID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "workstave history: %v\n", err)
		return 1
	}
	for _, message := range messages {
		if message.Role == "user" {
			fmt.Printf("[%s %s %s] %s\n", message.MailboxState, message.MailboxResult, message.MailboxResultDetail, message.Body)
		} else {
			fmt.Printf("[%s] %s\n", message.Role, message.Body)
		}
	}
	return 0
}

func cmdWorkState(args []string) int {
	fs := flag.NewFlagSet("state", flag.ContinueOnError)
	workID := fs.String("work", "", "Work identity")
	history := fs.Bool("history", false, "include superseded Work fact versions")
	dbPath := fs.String("db", defaultDB(), "database path")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *workID == "" {
		fmt.Fprintln(os.Stderr, "workstave state: -work is required")
		return 2
	}
	db, closeDB, err := openControlDB(context.Background(), *dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "workstave state: %v\n", err)
		return 1
	}
	defer closeDB()
	work, err := db.Work(context.Background(), *workID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "workstave state: %v\n", err)
		return 1
	}
	unresolved, err := db.UnresolvedMainRuns(context.Background(), *workID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "workstave state: %v\n", err)
		return 1
	}
	facts, err := db.WorkFacts(context.Background(), *workID)
	if *history {
		facts, err = db.WorkFactVersions(context.Background(), *workID)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "workstave state: %v\n", err)
		return 1
	}
	fmt.Printf("work=%s revision=%d main_harness=%s dir=%s\n", work.ID, work.Revision, work.MainHarness, work.WorkingDir)
	for _, run := range unresolved {
		outcome := run.Outcome.String
		if !run.Outcome.Valid {
			outcome = "in_progress_or_unobserved"
		}
		fmt.Printf("unresolved_run=%s operation=%s dispatch=%s outcome=%s message=%s\n", run.RunID, run.OpID, run.Dispatch, outcome, run.SourceMessageID)
	}
	for _, fact := range facts {
		fmt.Printf("%s %s revision=%d status=%s source=%s basis=%s value=%s\n", fact.Kind, fact.Key, fact.Revision, fact.Status, fact.SourceMessageID, fact.ConfirmationBasis, fact.Value)
	}
	return 0
}

func cmdDaemon(args []string) int {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDB(), "database path")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	db, closeDB, err := openDB(ctx, *dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "workstave daemon: %v\n", err)
		return 1
	}
	defer closeDB()
	if _, err := recoverAtOwnerStart(ctx, db); err != nil {
		fmt.Fprintf(os.Stderr, "workstave daemon: runtime recovery: %v\n", err)
		return 1
	}
	fmt.Println("daemon=ready")
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	var workers sync.WaitGroup
	for {
		workIDs, err := db.PendingWorkIDs(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "workstave daemon: list pending work: %v\n", err)
			return 1
		}
		for _, workID := range workIDs {
			workers.Add(1)
			go func(workID string) {
				defer workers.Done()
				if err := workflow.ProcessPending(ctx, db, workID, "", func(name string) (runner.Harness, error) {
					return makeHarness(name, "0.05")
				}); err != nil && ctx.Err() == nil {
					fmt.Fprintf(os.Stderr, "workstave daemon: work=%s: %v\n", workID, err)
				}
			}(workID)
		}
		select {
		case <-ctx.Done():
			workers.Wait()
			return 0
		case <-ticker.C:
		}
	}
}
