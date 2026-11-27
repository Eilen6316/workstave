package workflow

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workstave/internal/outcome"
	"workstave/internal/runner"
	"workstave/internal/store"
)

// TestRealSharedWorkAcrossConversations uses each installed Harness with its existing provider
// configuration. It makes one low-budget Claude request and one Codex request.
func TestRealSharedWorkAcrossConversations(t *testing.T) {
	if os.Getenv("WORKSTAVE_REAL_HARNESS") != "1" {
		t.Skip("set WORKSTAVE_REAL_HARNESS=1 to run the billable shared-Work experiment")
	}
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "workstave.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if err := db.CreateWork(ctx, "real-work", "real-main"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"conversation-a", "conversation-b"} {
		if err := db.CreateConversation(ctx, id, "real-work"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Accept(ctx, db, "message-a", "conversation-a", "claude-code", "Confirmed requirement report_format: the weekly report is TSV.\nConversation A note: orange-comet."); err != nil {
		t.Fatal(err)
	}
	if _, err := Accept(ctx, db, "message-b", "conversation-b", "codex", "What format did we choose for the weekly report? Answer with the format only."); err != nil {
		t.Fatal(err)
	}

	harnesses := map[string]runner.Harness{
		"claude-code": runner.Claude{MaxBudget: "0.05", Model: "claude-haiku-4-5-20251001"},
		"codex":       runner.Codex{},
	}
	err = ProcessPending(ctx, db, "real-work", t.TempDir(), func(name string) (runner.Harness, error) {
		h, ok := harnesses[name]
		if !ok {
			t.Fatalf("unsupported harness %q", name)
		}
		return h, nil
	})
	if err != nil {
		t.Fatalf("shared-Work processing failed: %v", err)
	}
	mainRunA, found, err := db.MainRunForMessage(ctx, "message-a")
	if err != nil || !found || mainRunA.Outcome.String != string(outcome.Completed) {
		t.Fatalf("Conversation A Main Run found=%v outcome=%q err=%v", found, mainRunA.Outcome.String, err)
	}
	mainRunB, found, err := db.MainRunForMessage(ctx, "message-b")
	if err != nil || !found || mainRunB.Outcome.String != string(outcome.Completed) {
		t.Fatalf("Conversation B Main Run found=%v outcome=%q err=%v", found, mainRunB.Outcome.String, err)
	}
	var snapshot inputSnapshot
	if err := json.Unmarshal([]byte(mainRunB.InputSnapshot), &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Context.Facts) != 1 || snapshot.Context.Facts[0].SourceMessageID != "message-a" || !strings.Contains(snapshot.Context.Facts[0].Value, "TSV") || !strings.Contains(snapshot.CurrentInput, "What format did we choose") || len(snapshot.Context.History) != 0 {
		t.Fatalf("Conversation B did not receive an isolated context with shared state: %+v", snapshot)
	}
	if strings.Contains(snapshot.CurrentInput, "orange-comet") {
		t.Fatal("Conversation B repeated a Conversation A-only detail")
	}
	reply, err := db.MessageByID(ctx, mainRunB.OutputMessageID)
	if err != nil || reply.ReplyTo != "message-b" || !strings.Contains(strings.ToLower(reply.Body), "tsv") {
		t.Fatalf("Conversation B reply=%+v err=%v, want a correlated answer containing TSV", reply, err)
	}
}
